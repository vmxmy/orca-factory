package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// RunTicket binds a Run to the calling Orca terminal. The repository profile is
// trusted local configuration; ticket bodies are fetched separately as data.
func RunTicket(ctx context.Context, identifier string, merging bool) (result Result, err error) {
	session, err := maintenance.Session()
	if err != nil {
		return Result{}, err
	}
	defer session()
	logger := (&Executor{}).withLog(ctx)

	delegated := false
	defer func() {
		if !delegated {
			logger.logError("initialization", err)
		}
	}()
	if !component.MatchString(identifier) {
		return Result{}, errors.New("invalid ticket identifier")
	}
	terminal := os.Getenv("ORCA_TERMINAL_HANDLE")
	if terminal == "" {
		return Result{}, errors.New("driver exec requires a current Orca terminal")
	}
	b, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"rev-parse", "--show-toplevel"}, Timeout: 10 * time.Second})
	if err != nil {
		return Result{}, errors.New("cannot locate repository")
	}
	repo := strings.TrimSpace(string(b))
	profile := filepath.Join(repo, "scripts", "agent-gates", "agent-graph.env")
	// Only selected, noncredential fields leave this subprocess.
	b, err = runx.Run(ctx, runx.Spec{Path: "/bin/bash", Args: []string{"-c", `source "$1" >/dev/null || exit; printf '%s\0' "$AGENT_GH_REPO" "$AGENT_LINEAR_TEAM" "$AGENT_LINEAR_PROJECT" "$AGENT_ROLE_REVIEWER_CMD" "${AGENT_TIER_FAST:-}" "${AGENT_TIER_DEFAULT:-}" "${AGENT_TIER_STRONG:-}" "${AGENT_TIER_ESCALATE:-0}" "${AGENT_ROLE_REVIEWER_CMD_SMALL:-}" "${AGENT_REVIEWER_SMALL_MAX_ESTIMATE:-2}" "${AGENT_LANE3_GLOBS:-}" "${AGENT_ISSUE_BACKEND:-linear}" "${AGENT_GH_API_URL:-}"`, "driver-profile", profile}, Dir: repo, Timeout: 10 * time.Second})
	if err != nil {
		return Result{}, errors.New("repository profile could not be loaded")
	}
	values := strings.Split(string(b), "\x00")
	if len(values) != 14 {
		return Result{}, errors.New("invalid repository profile")
	}
	owner, name, ok := strings.Cut(values[0], "/")
	if !ok || !component.MatchString(owner) || !component.MatchString(name) || (values[11] != "github" && (values[1] == "" || values[2] == "")) || strings.TrimSpace(values[3]) == "" {
		return Result{}, errors.New("repository profile requires GitHub repository, Linear team/project and reviewer")
	}
	maximum, err := strconv.ParseFloat(values[9], 64)
	if err != nil {
		return Result{}, errors.New("invalid reviewer estimate threshold")
	}
	cfg, err := config.Load(home())
	if err != nil {
		return Result{}, errors.New("driver configuration could not be loaded")
	}
	lc, err := issues.New(ctx, map[string]string{"AGENT_ISSUE_BACKEND": values[11], "AGENT_GH_REPO": values[0], "AGENT_GH_API_URL": values[12], "AGENT_LINEAR_TEAM": values[1]}, cfg.Tuning)
	if err != nil {
		return Result{}, err
	}
	var ticket Ticket
	if gh, ok := lc.(*issues.GitHub); ok {
		task, readErr := gh.Read(ctx, identifier)
		if readErr != nil {
			return Result{}, readErr
		}
		if !task.Metadata.Complete {
			return Result{}, errors.New("explicit adoption required before recovery")
		}
		ticket, err = ParseTicket(task.ID, task.Identifier, task.Title, task.Description)
		ticket.Estimate = task.Estimate
	} else {
		ticket, err = readTicket(ctx, identifier, values[1], values[2])
	}
	if err != nil {
		return Result{}, err
	}
	gc, err := github.New(github.Options{HTTPTimeout: cfg.Tuning.GitHub.HTTPTimeout})
	if err != nil {
		return Result{}, errors.New("GitHub initialization failed")
	}
	oc := &orca.Client{WorkerStartWait: cfg.Tuning.Orca.WorkerStartWait, Timeout: cfg.Tuning.Orca.CallTimeout}
	e := Executor{Orca: oc, Linear: lc, GitHub: gc, Tools: &Commands{ReviewerCommand: values[3], ReviewerSmallCommand: values[8], ReviewerSmallMaxEstimate: &maximum}, Config: Config{Repo: repo, Owner: owner, Repository: name, Terminal: terminal, Unit: "executor"}}
	e.Config.Root, err = executorStateRoot()
	if err != nil {
		return Result{}, err
	}
	e.Config.WorkerIdleTimeout = workerIdleTimeout(os.Getenv("AGENT_WORKER_IDLE_MINUTES"))
	e.Config.Tier = "default"
	e.Config.Escalate = values[7]
	e.Config.TierSettings = map[string]string{"fast": values[4], "default": values[5], "strong": values[6]}
	parts := strings.Split(values[5], ":")
	if len(parts) != 3 || parts[0] == "" {
		return Result{}, errors.New("repository profile requires default worker tier")
	}
	e.Config.Agent, e.Config.Model, e.Config.Effort = parts[0], parts[1], parts[2]
	if e.Config.Model == "" {
		e.Config.Effort = ""
	}
	delegated = true
	e.Config.Merging = merging
	e.Config.Lane3Globs = strings.Fields(values[10])
	return e.Start(ctx, ticket)
}

// linear.Snapshot deliberately omits description. This executor-local read
// adapter asks only for the brief and fences it to the configured team/project.
func readTicket(ctx context.Context, identifier, team, project string) (Ticket, error) {
	key := os.Getenv("LINEAR_API_KEY")
	if key == "" {
		return Ticket{}, errors.New("LINEAR_API_KEY is required")
	}
	body, _ := json.Marshal(map[string]any{"query": `query ExecutorBrief($id: String!) { issue(id: $id) { id identifier title description estimate team { id } project { id } } }`, "variables": map[string]string{"id": identifier}})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.linear.app/graphql", bytes.NewReader(body))
	if err != nil {
		return Ticket{}, errors.New("Linear brief request failed")
	}
	req.Header.Set("Authorization", key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return Ticket{}, errors.New("Linear brief transport failed")
	}
	defer func() { _ = resp.Body.Close() }() // Read-only response; read errors are handled below.
	if resp.StatusCode != http.StatusOK {
		return Ticket{}, errors.New("Linear brief request rejected")
	}
	var envelope struct {
		Data struct {
			Issue *struct {
				ID, Identifier, Title, Description string
				Estimate                           *float64
				Team, Project                      struct{ ID string }
			}
		}
		Errors []json.RawMessage
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&envelope) != nil || len(envelope.Errors) > 0 || envelope.Data.Issue == nil {
		return Ticket{}, errors.New("invalid Linear brief response")
	}
	issue := envelope.Data.Issue
	if issue.Identifier != identifier || issue.Team.ID != team || issue.Project.ID != project {
		return Ticket{}, errors.New("ticket outside configured repository project")
	}
	ticket, err := ParseTicket(issue.ID, issue.Identifier, issue.Title, issue.Description)
	ticket.Estimate = issue.Estimate
	return ticket, err
}

func workerIdleTimeout(value string) time.Duration {
	minutes, err := strconv.ParseInt(value, 10, 64)
	if err != nil || minutes <= 0 || minutes > int64((1<<63-1)/time.Minute) {
		return 15 * time.Minute
	}
	return time.Duration(minutes) * time.Minute
}

// executorStateRoot is shared by all entrypoint recovery and artifact writes.
func home() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func executorStateRoot() (string, error) {
	if root := os.Getenv("AGENT_DRIVER_STATE_ROOT"); root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot resolve state root")
	}
	return filepath.Join(home, ".local", "state", "orca-factory"), nil
}
