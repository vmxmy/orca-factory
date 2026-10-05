package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

func (r *Runtime) AutomationDisabled(ctx context.Context, repo config.Repository) (bool, error) {
	// Startup refusals persist until restart: no client was initialized for
	// these repositories, so a later disabled automation cannot admit them.
	if err := r.refused[repo.Path]; err != nil {
		return false, err
	}
	receipt, err := r.Orca.AutomationsList(ctx)
	if err != nil {
		return false, errors.New("automation read failed")
	}
	var rows []struct {
		Enabled    *bool                            `json:"enabled"`
		RunContext struct{ Path, ProjectID string } `json:"runContext"`
	}
	raw, ok := receipt["automations"]
	if !ok || string(raw) == "null" || json.Unmarshal(raw, &rows) != nil {
		return false, errors.New("incomplete automation list")
	}
	for _, row := range rows {
		matches := row.RunContext.Path != "" && filepath.Clean(row.RunContext.Path) == filepath.Clean(repo.Path)
		project := repo.Env["AGENT_GH_REPO"]
		if project != "" && row.RunContext.ProjectID == "github:"+project {
			matches = true
		}
		unknown := row.RunContext.Path == "" && row.RunContext.ProjectID == ""
		if (matches || unknown) && (row.Enabled == nil || *row.Enabled) {
			return false, nil
		}
	}
	return true, nil
}

// A successful complete terminal enumeration provides positive absence proof.
// A rejected terminal-show command cannot distinguish absence from read failure.
func (r *Runtime) TerminalAbsent(ctx context.Context, t Ticket) (bool, error) {
	if t.Terminal == "" {
		return false, errors.New("executor terminal unknown")
	}
	if !filepath.IsAbs(r.OrcaPath) {
		return false, errors.New("orca path must be absolute")
	}
	raw, err := r.command(ctx, "", r.OrcaPath, "terminal", "list", "--limit", "10000", "--json")
	if err != nil {
		return false, errors.New("terminal list failed")
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result *struct {
			Terminals *[]struct {
				Handle string `json:"handle"`
			} `json:"terminals"`
			TotalCount *int  `json:"totalCount"`
			Truncated  *bool `json:"truncated"`
			HostScope  *struct {
				OmittedHostIDs *[]string `json:"omittedHostIds"`
			} `json:"hostScope"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || !envelope.OK || envelope.Result == nil {
		return false, errors.New("invalid terminal list")
	}
	result := envelope.Result
	if result.Terminals == nil || result.TotalCount == nil || result.Truncated == nil || *result.Truncated || *result.TotalCount != len(*result.Terminals) || result.HostScope == nil || result.HostScope.OmittedHostIDs == nil || len(*result.HostScope.OmittedHostIDs) != 0 {
		return false, errors.New("incomplete terminal enumeration")
	}
	for _, terminal := range *result.Terminals {
		if terminal.Handle == "" {
			return false, errors.New("invalid terminal row")
		}
		if terminal.Handle == t.Terminal {
			return false, nil
		}
	}
	return true, nil
}
func (r *Runtime) NoActiveDispatch(ctx context.Context, t Ticket) (bool, error) {
	if t.Run == "" {
		return false, errors.New("executor run unknown")
	}
	cursor := ""
	seen := map[string]bool{}
	for {
		receipt, err := r.Orca.WorkerList(ctx, orca.ListOptions{Run: t.Run, Cursor: cursor, Limit: 100, IncludeRemote: true})
		if err != nil {
			return false, errors.New("dispatch read failed")
		}
		var scope struct {
			Run string `json:"run"`
		}
		var page struct {
			HasMore    *bool  `json:"hasMore"`
			NextCursor string `json:"nextCursor"`
		}
		var workers []struct {
			Status string `json:"dispatchStatus"`
		}
		if json.Unmarshal(receipt["scope"], &scope) != nil || scope.Run != t.Run || json.Unmarshal(receipt["page"], &page) != nil || page.HasMore == nil || json.Unmarshal(receipt["workers"], &workers) != nil || string(receipt["workers"]) == "null" {
			return false, errors.New("incomplete dispatch enumeration")
		}
		for _, worker := range workers {
			switch worker.Status {
			case "completed", "failed", "abandoned", "cancelled":
			default:
				return false, nil
			}
		}
		if !*page.HasMore {
			return true, nil
		}
		if page.NextCursor == "" || seen[page.NextCursor] {
			return false, errors.New("invalid dispatch cursor")
		}
		cursor = page.NextCursor
		seen[cursor] = true
	}
}

// InspectLegacy checks remote evidence. Lookup failures never become "no PR"
// or "no complete branch" and therefore never release an uncertain ticket.
func (r *Runtime) InspectLegacy(ctx context.Context, t Ticket) (Legacy, error) {
	if !identifier.MatchString(t.Identifier) {
		return Legacy{}, errors.New("invalid ticket identifier")
	}
	repo := r.Repositories[t.Repo].Env["AGENT_GH_REPO"]
	if repo == "" {
		return Legacy{}, errors.New("missing GitHub repository")
	}
	raw, err := r.command(ctx, t.Repo, "gh", "pr", "list", "--repo", repo, "--state", "open", "--limit", "1000", "--json", "headRefName")
	if err != nil {
		return Legacy{}, errors.New("legacy PR lookup failed")
	}
	var prs []struct {
		Head string `json:"headRefName"`
	}
	if json.Unmarshal(raw, &prs) != nil || strings.TrimSpace(string(raw)) == "null" || len(prs) >= 1000 {
		return Legacy{}, errors.New("incomplete legacy PR list")
	}
	prefix := "agent/" + t.Identifier + "/"
	for _, pr := range prs {
		if strings.HasPrefix(pr.Head, prefix) {
			return Legacy{HasPR: true}, nil
		}
	}
	raw, err = r.command(ctx, t.Repo, "git", "ls-remote", "--heads", "origin", "refs/heads/"+prefix+"*")
	if err != nil {
		return Legacy{}, errors.New("legacy branch lookup failed")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/heads/"+prefix) || !commitID(fields[0]) {
			return Legacy{}, errors.New("invalid legacy branch response")
		}
		sha := fields[0]
		if _, err = r.command(ctx, t.Repo, "git", "fetch", "--no-tags", "origin", sha); err != nil {
			return Legacy{}, errors.New("legacy branch fetch failed")
		}
		raw, err = r.command(ctx, t.Repo, "git", "show", "-s", "--format=%(trailers:key=agent-status,valueonly)", sha)
		if err != nil {
			return Legacy{}, errors.New("legacy commit lookup failed")
		}
		if strings.TrimSpace(string(raw)) == "complete" {
			return Legacy{Complete: true}, nil
		}
	}
	return Legacy{}, nil
}
func commitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
