package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

const PruneContractVersion = "prune-worktrees-v1"

var ticketPattern = regexp.MustCompile(`[A-Z][A-Z0-9]*-[0-9]+`)
var remotePattern = regexp.MustCompile(`^refs/heads/agent/([A-Z][A-Z0-9]*-[0-9]+)/[^/]+$`)
var markerPattern = regexp.MustCompile(`^pr=([1-9][0-9]*)$`)
var issueCommentPattern = regexp.MustCompile(`(?m)^issue: ([A-Za-z0-9][A-Za-z0-9._-]*)$`)

type tree struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Locked  bool   `json:"locked"`
	Ticket  string `json:"linkedLinearIssue"`
	Comment string `json:"comment"`
	Created *int64 `json:"createdAt"`
}
type issue struct {
	Identifier string `json:"identifier"`
	State      struct {
		Type string `json:"type"`
	} `json:"state"`
}
type pull struct {
	Number int     `json:"number"`
	Merged *string `json:"merged_at"`
	Head   struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			Name string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}
type pruner struct {
	ctx                                                                context.Context
	out, warnings                                                      io.Writer
	repo, main, self, prefix, team, project, gh                        string
	age, budget, limit                                                 int64
	started                                                            int64
	removed, calls                                                     int64
	budgetTripped, quotaTripped, inventoryOK, linearOK, prOK, remoteOK bool
	git, orca                                                          []tree
	issues                                                             []issue
	github                                                             *issues.GitHub
	pulls                                                              []pull
	remote                                                             string
}

// RunPrune preserves the distributed entry while keeping destructive work behind evidence checks.
func RunPrune(ctx context.Context, args []string, out, warnings io.Writer) int {
	if len(args) == 1 && args[0] == "--contract-version" {
		_, err := fmt.Fprintln(out, PruneContractVersion)
		if err != nil {
			return 1
		}
		return 0
	}
	release, guardErr := maintenance.Write()
	if guardErr != nil {
		_, _ = fmt.Fprintln(warnings, guardErr)
		return 1
	}
	defer release()
	if _, err := issues.Backend(map[string]string{"AGENT_ISSUE_BACKEND": os.Getenv("AGENT_ISSUE_BACKEND")}); err != nil {
		_, _ = fmt.Fprintln(warnings, err)
		return 2
	}
	p := pruner{ctx: ctx, out: out, warnings: warnings, repo: os.Getenv("AGENT_REPO_PATH"), prefix: os.Getenv("AGENT_RUN_WORKTREE_PREFIX"), team: os.Getenv("AGENT_LINEAR_TEAM"), project: os.Getenv("AGENT_LINEAR_PROJECT"), gh: os.Getenv("AGENT_GH_REPO"), started: time.Now().Unix()}
	required := []string{"AGENT_REPO_PATH", "AGENT_RUN_WORKTREE_PREFIX"}
	if os.Getenv("AGENT_ISSUE_BACKEND") == "github" {
		required = append(required, "AGENT_GH_REPO")
	} else {
		required = append(required, "AGENT_LINEAR_TEAM", "AGENT_LINEAR_PROJECT")
	}
	for _, key := range required {
		if os.Getenv(key) == "" {
			p.warn("prune: %s required", key)
			return 1
		}
	}
	for _, setting := range []struct {
		key      string
		fallback int64
		value    *int64
	}{{"AGENT_RUN_MAX_AGE_HOURS", 24, &p.age}, {"AGENT_PRUNE_BUDGET_SECONDS", 60, &p.budget}, {"AGENT_PRUNE_MAX_PER_RUN", 5, &p.limit}} {
		value := os.Getenv(setting.key)
		if value == "" {
			*setting.value = setting.fallback
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			p.warn("prune: invalid integer %s: %s", setting.key, value)
			return 1
		}
		*setting.value = n
	}
	defer func() { p.warn("prune_linear_calls=%d", p.calls) }()
	var err error
	p.main, err = filepath.Abs(p.repo)
	if err == nil {
		p.main, err = filepath.EvalSymlinks(p.main)
	}
	if err != nil {
		p.warn("prune: main checkout unavailable: %v", err)
		return 1
	}
	p.self, err = os.Getwd()
	if err != nil {
		p.warn("prune: current directory unavailable: %v", err)
		return 1
	}
	p.self, err = filepath.EvalSymlinks(p.self)
	if err != nil {
		return 1
	}
	p.compensate()
	if ctx.Err() != nil {
		return 1
	}
	if code := p.legacyRuns(); code != 0 {
		return code
	}
	p.legacyUnits()
	if ctx.Err() != nil {
		return 1
	}
	return 0
}
func (p *pruner) warn(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(p.warnings, format+"\n", args...)
}
func (p *pruner) keep(object, reason string) {
	p.warn("skip %s: %s", object, strings.ReplaceAll(strings.TrimRight(reason, "\n"), "\n", " "))
}
func (p *pruner) command(path string, args ...string) (string, string, int) {
	output, err := runx.Run(p.ctx, runx.Spec{Path: path, Args: args, Timeout: 24 * time.Hour})
	if err == nil {
		return strings.TrimRight(string(output), "\n"), "", 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
		return strings.TrimRight(string(output), "\n"), strings.TrimRight(string(exit.Stderr), "\n"), code
	}
	return strings.TrimRight(string(output), "\n"), err.Error(), 1
}
func (p *pruner) gitAt(path string, args ...string) (string, string, int) {
	return p.command("git", append([]string{"-C", path}, args...)...)
}
func (p *pruner) gitTrees() ([]tree, bool) {
	raw, diagnostic, code := p.gitAt(p.repo, "worktree", "list", "--porcelain", "-z")
	if code != 0 {
		p.keep("compensation", "Git worktree inventory unavailable: "+diagnostic)
		return nil, false
	}
	var result []tree
	for _, block := range strings.Split(raw, "\x00\x00") {
		if block == "" {
			continue
		}
		var item tree
		for _, field := range strings.Split(block, "\x00") {
			switch {
			case strings.HasPrefix(field, "worktree "):
				item.Path = strings.TrimPrefix(field, "worktree ")
			case strings.HasPrefix(field, "branch refs/heads/"):
				item.Branch = strings.TrimPrefix(field, "branch refs/heads/")
			case field == "locked" || strings.HasPrefix(field, "locked "):
				item.Locked = true
			}
		}
		if item.Path == "" {
			return nil, false
		}
		result = append(result, item)
	}
	return result, true
}
func (p *pruner) orcaTrees() ([]tree, bool) {
	raw, _, code := p.command("orca", "worktree", "list", "--repo", "path:"+p.repo, "--json")
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			Trees []tree `json:"worktrees"`
		} `json:"result"`
	}
	if code != 0 || json.Unmarshal([]byte(raw), &response) != nil || !response.OK || response.Result.Trees == nil {
		return nil, false
	}
	result := make([]tree, 0, len(response.Result.Trees))
	for _, item := range response.Result.Trees {
		if item.Path == "" || !filepath.IsAbs(item.Path) {
			return nil, false
		}
		if os.Getenv("AGENT_ISSUE_BACKEND") == "github" {
			item.Ticket = ""
			matches := issueCommentPattern.FindAllStringSubmatch(item.Comment, -1)
			if len(matches) > 1 {
				return nil, false
			}
			if len(matches) == 1 {
				item.Ticket = matches[0][1]
			}
		}
		result = append(result, item)
	}
	return result, true
}
func tempPath(key, name string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	root := os.Getenv("TMPDIR")
	if root == "" {
		root = "/tmp"
	}
	return filepath.Join(root, name)
}
func (p *pruner) loadIssues() bool {
	if os.Getenv("AGENT_ISSUE_BACKEND") == "github" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		cfg, err := config.Load(home)
		if err != nil {
			return false
		}
		client, err := issues.New(p.ctx, map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": p.gh, "AGENT_GH_API_URL": os.Getenv("AGENT_GH_API_URL")}, cfg.Tuning)
		if err != nil {
			return false
		}
		ghClient := client.(*issues.GitHub)
		p.github = ghClient
		rows, err := ghClient.List(p.ctx)
		if err != nil {
			return false
		}
		for _, task := range rows {
			row := issue{Identifier: task.Identifier}
			comments, readErr := ghClient.ListComments(p.ctx, task.ID)
			if readErr != nil {
				return false
			}
			if scheduler.GitHubCleanupAllowed(task, comments) {
				row.State.Type = task.State.ID
			}
			p.issues = append(p.issues, row)
		}
		p.linearOK = true
		return true
	}
	raw, err := os.ReadFile(tempPath("AGENT_SNAPSHOT_FILE", "orca-factory-linear-snapshot.json"))
	var snapshot struct {
		Generated *float64 `json:"generatedAt"`
		Issues    []issue  `json:"issues"`
	}
	if err == nil && json.Unmarshal(raw, &snapshot) == nil && snapshot.Generated != nil && float64(time.Now().Unix())-*snapshot.Generated <= 900 && snapshot.Issues != nil {
		p.issues = snapshot.Issues
		p.linearOK = true
		return true
	}
	p.calls++
	file, err := os.OpenFile(tempPath("AGENT_LINEAR_COUNT_FILE", "orca-factory-linear-calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666)
	if err == nil {
		_, _ = fmt.Fprintf(file, "%d prune\n", time.Now().Unix())
		_ = file.Close()
	}
	result, diagnostic, code := p.command("orca", "linear", "list-issues", "--team", p.team, "--project", p.project, "--json")
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			Issues []issue `json:"issues"`
		} `json:"result"`
	}
	if code == 0 && json.Unmarshal([]byte(result), &response) == nil && response.OK && response.Result.Issues != nil {
		p.issues = response.Result.Issues
		p.linearOK = true
		return true
	}
	if strings.Contains(result+diagnostic, "Rate limit") {
		p.warn("prune: 不确定: linear rate limited")
	}
	return false
}
func (p *pruner) ticketSafe(id, object, mode string) bool {
	if os.Getenv("AGENT_ISSUE_BACKEND") == "github" {
		if p.github != nil {
			task, err := p.github.Read(p.ctx, id)
			if err == nil {
				comments, readErr := p.github.ListComments(p.ctx, task.ID)
				if readErr == nil && scheduler.GitHubCleanupAllowed(task, comments) {
					return true
				}
			}
		}
		p.keep(object, "GitHub task cleanup evidence unavailable or changed: "+id)
		return false
	}
	if !p.linearOK {
		p.keep(object, "linear snapshot unavailable, cannot verify ticket "+id)
		return false
	}
	for _, item := range p.issues {
		if item.Identifier == id {
			if item.State.Type != "" && ((mode == "completed" && item.State.Type == "completed") || (mode != "completed" && item.State.Type != "started")) {
				return true
			}
			break
		}
	}
	p.keep(object, fmt.Sprintf("ticket %s is not confirmed %s (in flight or missing from Linear snapshot)", id, mode))
	return false
}
func (p *pruner) issueSafe(path, branch string) bool {
	object := path
	if object == "" {
		object = branch
	}
	if !p.inventoryOK {
		p.keep(object, "Orca inventory unavailable")
		return false
	}
	for _, item := range p.orca {
		match := item.Path == path
		for _, git := range p.git {
			match = match || (branch != "" && git.Path == item.Path && git.Branch == branch)
		}
		if match && item.Ticket != "" && !p.ticketSafe(item.Ticket, object, "inactive") {
			return false
		}
	}
	if id := ticketPattern.FindString(branch + " " + path); id != "" {
		return p.ticketSafe(id, object, "inactive")
	}
	return true
}
func (p *pruner) pathSafe(path string) bool {
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		p.keep(path, "worktree path unavailable: "+err.Error())
		return false
	}
	if physical == p.self || physical == p.main || path == p.repo {
		return false
	}
	if !p.inventoryOK {
		p.keep(path, "Orca inventory unavailable")
		return false
	}
	for _, item := range p.git {
		if item.Path == path {
			if item.Locked {
				p.keep(path, "worktree is locked")
				return false
			}
			return p.issueSafe(path, item.Branch)
		}
	}
	p.keep(path, "Git worktree ownership unavailable")
	return false
}
func (p *pruner) legacySafe(path string) bool {
	if !p.pathSafe(path) {
		return false
	}
	for _, item := range p.orca {
		if item.Path == path && item.Ticket != "" {
			return false
		}
	}
	return true
}
func (p *pruner) clean(path string, all bool) bool {
	args := []string{"status", "--porcelain"}
	if all {
		args = []string{"status", "--porcelain=v1", "--untracked-files=all"}
	}
	result, diagnostic, code := p.gitAt(path, args...)
	if code != 0 {
		p.keep(path, "git status failed: "+result+diagnostic)
		return false
	}
	if result != "" {
		p.keep(path, "worktree is dirty")
		return false
	}
	return true
}
func (p *pruner) merged(sha, branch string) bool {
	if !p.prOK {
		return false
	}
	for _, pr := range p.pulls {
		if pr.Merged != nil && pr.Head.SHA == sha && (branch == "" || pr.Head.Ref == branch) {
			return true
		}
	}
	return false
}
func (p *pruner) stopped(budget bool) bool {
	if p.ctx.Err() != nil {
		return true
	}
	if budget && (p.budgetTripped || time.Now().Unix()-p.started >= p.budget) {
		if !p.budgetTripped {
			p.warn("prune: 超过时间预算，剩余对象留到下次")
			p.budgetTripped = true
		}
		return true
	}
	if p.quotaTripped || p.removed >= p.limit {
		if !p.quotaTripped {
			p.warn("prune: 达到单次回收上限，剩余对象留到下次")
			p.quotaTripped = true
		}
		return true
	}
	return false
}
func (p *pruner) loadPulls() {
	if p.gh != "" {
		raw, _, code := p.command("gh", "api", "--paginate", "repos/"+p.gh+"/pulls?state=closed&per_page=100")
		if code == 0 {
			decoder := json.NewDecoder(strings.NewReader(raw))
			valid := false
			for {
				var page []pull
				err := decoder.Decode(&page)
				if err == io.EOF {
					p.prOK = valid
					break
				}
				if err != nil || page == nil {
					break
				}
				valid = true
				for _, pr := range page {
					if pr.Head.Repo.Name == p.gh {
						p.pulls = append(p.pulls, pr)
					}
				}
			}
		}
	}
	if !p.prOK {
		p.keep("compensation", "closed PR query failed; PR-dependent objects retained")
	}
	var code int
	p.remote, _, code = p.gitAt(p.repo, "ls-remote", "--heads", "origin")
	p.remoteOK = code == 0
}
func (p *pruner) compensate() {
	var ok bool
	p.git, ok = p.gitTrees()
	if !ok {
		return
	}
	p.orca, ok = p.orcaTrees()
	if !ok {
		p.keep("compensation", "Orca worktree inventory unavailable")
		return
	}
	p.inventoryOK = true
	if !p.loadIssues() {
		p.warn("prune: 不确定: linear unavailable，跳过补偿")
		return
	}
	p.loadPulls()
	// Remote heads are loaded once and retained for both worker and remote branch checks.
	p.compensateWorkers()
	p.compensateGit()
	p.compensateBranches()
	p.compensateRemote()
}
func (p *pruner) compensateWorkers() {
	for _, item := range p.orca {
		if item.Ticket == "" {
			continue
		}
		if p.stopped(true) {
			break
		}
		if !p.pathSafe(item.Path) {
			continue
		}
		branch, _, code := p.gitAt(item.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
		if code != 0 {
			p.keep(item.Path, "branch unavailable")
			continue
		}
		if !p.issueSafe(item.Path, branch) || !p.ticketSafe(item.Ticket, item.Path, "completed") || !p.clean(item.Path, true) {
			continue
		}
		sha, _, code := p.gitAt(item.Path, "rev-parse", "--verify", "HEAD")
		if code != 0 {
			p.keep(item.Path, "head unavailable")
			continue
		}
		remoteSHA := ""
		if p.remoteOK {
			for _, line := range strings.Split(p.remote, "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 && fields[1] == "refs/heads/"+branch {
					remoteSHA = fields[0]
					break
				}
			}
		}
		if sha != remoteSHA && !p.merged(sha, "") {
			p.keep(item.Path, "head is neither confirmed pushed nor merged")
			continue
		}
		if p.github != nil && !p.issueSafe(item.Path, branch) {
			continue
		}
		result, diagnostic, code := p.command("orca", "worktree", "rm", "--worktree", "path:"+item.Path, "--json")
		var response struct {
			OK bool `json:"ok"`
		}
		if code != 0 || json.Unmarshal([]byte(result), &response) != nil || !response.OK {
			p.keep(item.Path, "Orca removal failed: "+result+diagnostic)
		} else {
			p.removed++
		}
	}
}
func (p *pruner) compensateGit() {
	for _, item := range p.git {
		if p.stopped(true) {
			break
		}
		if !strings.HasPrefix(item.Path, p.main+"/.claude/worktrees/agent-") || item.Path == p.self {
			continue
		}
		managed := false
		for _, orca := range p.orca {
			managed = managed || orca.Path == item.Path
		}
		if managed {
			continue
		}
		if !p.pathSafe(item.Path) {
			continue
		}
		if !p.issueSafe(item.Path, item.Branch) || !p.clean(item.Path, true) {
			continue
		}
		sha, _, code := p.gitAt(item.Path, "rev-parse", "--verify", "HEAD")
		if code != 0 {
			p.keep(item.Path, "head unavailable")
			continue
		}
		if !p.merged(sha, "") {
			p.keep(item.Path, "head has no matching merged PR")
			continue
		}
		if p.github != nil && !p.issueSafe(item.Path, item.Branch) {
			continue
		}
		if p.visibleGit("worktree", "remove", item.Path) {
			p.removed++
		} else {
			p.keep(item.Path, "Git removal failed")
		}
	}
}
func (p *pruner) visibleGit(args ...string) bool {
	command := exec.CommandContext(p.ctx, "git", append([]string{"-C", p.repo}, args...)...)
	command.Stdout, command.Stderr = p.out, p.warnings
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 100 * time.Millisecond
	return command.Run() == nil
}
func (p *pruner) compensateBranches() {
	current, ok := p.gitTrees()
	result, _, code := p.gitAt(p.repo, "for-each-ref", "--format=%(objectname) %(refname:short)", "refs/heads/")
	if !ok || code != 0 {
		p.keep("local branches", "cannot verify branch occupancy")
		return
	}
	for _, line := range strings.Split(result, "\n") {
		if p.stopped(true) {
			break
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sha, branch := fields[0], fields[1]
		occupied := false
		for _, item := range current {
			occupied = occupied || item.Branch == branch
		}
		if occupied || !p.issueSafe("", branch) {
			continue
		}
		if !p.merged(sha, "") {
			p.keep(branch, "head has no matching merged PR")
			continue
		}
		if p.github != nil && !p.issueSafe("", branch) {
			continue
		}
		if p.visibleGit("branch", "-D", "--", branch) {
			p.removed++
		} else {
			p.keep(branch, "branch removal failed")
		}
	}
}
func (p *pruner) compensateRemote() {
	if !p.remoteOK {
		p.keep("remote branches", "remote heads query failed")
		return
	}
	for _, line := range strings.Split(p.remote, "\n") {
		if p.stopped(true) {
			break
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		match := remotePattern.FindStringSubmatch(fields[1])
		if match == nil {
			continue
		}
		sha, branch := fields[0], strings.TrimPrefix(fields[1], "refs/heads/")
		if !p.issueSafe("", branch) || !p.ticketSafe(match[1], branch, "completed") {
			continue
		}
		if !p.merged(sha, branch) {
			p.keep(branch, "remote head has no matching merged PR")
			continue
		}
		if p.github != nil && !p.ticketSafe(match[1], branch, "completed") {
			continue
		}
		if p.visibleGit("push", "origin", "--delete", branch) {
			p.removed++
		} else {
			p.keep(branch, "remote removal failed")
		}
	}
}
func (p *pruner) legacyRuns() int {
	items, ok := p.orcaTrees()
	if !ok {
		return 0
	}
	p.orca = items
	cutoff := (time.Now().Unix() - p.age*3600) * 1000
	for _, item := range items {
		if !strings.HasPrefix(filepath.Base(item.Path), p.prefix) {
			continue
		}
		if p.stopped(false) {
			break
		}
		if !p.legacySafe(item.Path) {
			continue
		}
		marker, err := os.Stat(filepath.Join(item.Path, ".omc/agent-run-finished"))
		marked := err == nil && marker.Mode().IsRegular()
		if !marked && (item.Created == nil || *item.Created >= cutoff) {
			continue
		}
		if !p.clean(item.Path, true) {
			continue
		}
		result, diagnostic, code := p.command("orca", "worktree", "rm", "--worktree", "path:"+item.Path, "--json")
		var response struct {
			OK interface{} `json:"ok"`
		}
		if err := json.Unmarshal([]byte(result), &response); err != nil {
			p.keep(item.Path, "Orca removal failed: "+result+diagnostic)
			if code != 0 {
				return code
			}
			return 4
		}
		_, _ = fmt.Fprintf(p.out, "pruned %s ok=%v\n", item.Path, response.OK)
		if code != 0 {
			return code
		}
		if response.OK != true {
			p.keep(item.Path, "Orca removal failed: "+result)
			continue
		}
		p.removed++
	}
	return 0
}
func (p *pruner) legacyUnits() {
	items, ok := p.orcaTrees()
	if !ok {
		return
	}
	p.orca = items
	for _, item := range items {
		if p.stopped(false) {
			break
		}
		if !p.legacySafe(item.Path) {
			continue
		}
		marker := filepath.Join(item.Path, ".omc/agent-unit")
		if _, err := os.Lstat(marker); err != nil {
			continue
		}
		raw, err := os.ReadFile(marker)
		if err != nil {
			p.keep(item.Path, "cannot read agent-unit: "+err.Error())
			continue
		}
		match := markerPattern.FindStringSubmatch(strings.TrimRight(string(raw), "\n"))
		if match == nil {
			p.keep(item.Path, "invalid agent-unit (expected pr=<N>)")
			continue
		}
		if p.gh == "" {
			p.keep(item.Path, "AGENT_GH_REPO is not set")
			continue
		}
		state := ""
		number, _ := strconv.Atoi(match[1])
		if p.prOK {
			for _, pr := range p.pulls {
				if pr.Number == number {
					state = "CLOSED"
					if pr.Merged != nil {
						state = "MERGED"
					}
					break
				}
			}
		}
		if state == "" {
			result, diagnostic, code := p.command("gh", "pr", "view", match[1], "--repo", p.gh, "--json", "state", "-q", ".state")
			if code != 0 {
				p.keep(item.Path, "gh pr view "+match[1]+" failed: "+diagnostic)
				continue
			}
			state = result
		}
		if (state != "MERGED" && state != "CLOSED") || !p.clean(item.Path, false) {
			continue
		}
		result, diagnostic, code := p.command("orca", "worktree", "rm", "--worktree", "path:"+item.Path)
		if code != 0 {
			p.keep(item.Path, "orca worktree rm failed: "+result+diagnostic)
			continue
		}
		_, _ = fmt.Fprintf(p.out, "pruned %s (PR %s %s)\n", item.Path, match[1], state)
		p.removed++
	}
}
