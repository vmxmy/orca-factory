package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runnerhealth"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type OrcaClient interface {
	WorkerRelease(context.Context, string, string) (orca.Receipt, error)
	WorktreeSet(context.Context, orca.BoardCard) (orca.Receipt, error)
	AutomationsList(context.Context) (orca.Receipt, error)
	WorkerList(context.Context, orca.ListOptions) (orca.Receipt, error)
	TerminalCreate(context.Context, orca.TerminalOptions) (orca.Receipt, error)
}
type Runtime struct {
	GitHub               *github.Client
	Clients              map[string]issues.Store
	Repositories         map[string]config.Repository
	Orca                 OrcaClient
	Workspaces           WorkspaceClient
	OrcaPath, DriverPath string
	Run                  func(context.Context, runx.Spec) ([]byte, error)
	Now                  func() time.Time
	Tuning               config.Tuning
	Runners              *runnerhealth.Monitor
	refused              map[string]error
	// newClient binds a ledger client for a repository's env; Track calls it
	// for every repository discovery hands over.
	newClient func(context.Context, map[string]string) (issues.Store, error)
	// runnerState is the isolated CI runners' state directory on the host that
	// runs them; Track starts Runners once their repository is bound.
	runnerState string
	// closed holds, per repository, the Done or Canceled tickets of the last
	// Snapshot; ReclaimWorkspaces removes their workspaces (OF-96).
	closed        map[string]map[string]closedTicket
	reclaimWarned map[string]string
}

// NewRuntime binds a client for each of repos. Startup exclusion runs before
// any issue backend initialization; Tick repeats the check before each
// inspection. serve passes no repos and lets the scheduler Track what
// discovery finds.
func NewRuntime(ctx context.Context, c config.Config, repos []config.Repository) (*Runtime, error) {
	path, err := runx.OrcaPath()
	if err != nil {
		return nil, err
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, errors.New("cannot locate driver binary")
	}
	client := &orca.Client{Path: path, WorkerStartWait: c.Tuning.Orca.WorkerStartWait, Timeout: c.Tuning.Orca.CallTimeout}
	r := &Runtime{
		runnerState:  runnerStateDir(),
		Tuning:       c.Tuning,
		Clients:      map[string]issues.Store{},
		Repositories: map[string]config.Repository{},
		Orca:         client,
		Workspaces:   client,
		OrcaPath:     path,
		DriverPath:   binary,
	}
	if err = r.initialize(ctx, repos, r.newIssueStore); err != nil {
		return nil, err
	}
	return r, nil
}

// runnerStateDir is the isolated CI runners' state directory, or "" on a host
// that does not run them.
func runnerStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".local", "state", "orca-go-isolated-runner")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// watchRunners files runner alerts in repo once it is the repository the
// isolated CI runners serve.
func (r *Runtime) watchRunners(repo config.Repository) {
	client, ok := r.Clients[repo.Path]
	if r.runnerState == "" || r.Runners != nil || !ok || repo.Env["AGENT_GH_REPO"] != runnerhealth.Repository {
		return
	}
	record := filepath.Join(filepath.Dir(r.runnerState), "orca-factory", "runner-health.json")
	r.Runners = &runnerhealth.Monitor{StateDir: r.runnerState, Record: record, Tickets: client, Run: runx.Run}
}

func (r *Runtime) CheckRunners(ctx context.Context) error {
	if r.Runners == nil {
		return nil
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	return r.Runners.Poll(ctx, now)
}

// githubNew constructs a GitHub client from Options; a test may replace it to
// redirect the lazily created client at a recorded fixture server instead of
// the real GitHub API.
var githubNew = github.New

// githubClient returns the Runtime's GitHub client, lazily constructing it on
// first use (serve restarts leave it nil until the merge lane first runs).
// The merge lane, the open-pr resolution path, and deployment checks all
// share this one accessor instead of duplicating the nil check and creation.
func (r *Runtime) githubClient() (*github.Client, error) {
	if r.GitHub != nil {
		return r.GitHub, nil
	}
	gh, err := githubNew(github.Options{HTTPTimeout: r.Tuning.GitHub.HTTPTimeout})
	if err != nil {
		return nil, err
	}
	r.GitHub = gh
	return gh, nil
}

func (r *Runtime) initialize(ctx context.Context, repos []config.Repository, newClient func(context.Context, map[string]string) (issues.Store, error)) error {
	r.Clients = map[string]issues.Store{}
	r.Repositories = map[string]config.Repository{}
	r.refused = map[string]error{}
	r.newClient = newClient
	var refusals []error
	for _, repo := range repos {
		if err := r.Track(ctx, repo); err != nil {
			if r.refused[repo.Path] == nil {
				return err
			}
			refusals = append(refusals, err)
		}
	}
	if len(r.Clients) == 0 {
		return errors.Join(refusals...)
	}
	return nil
}

// Track binds a ledger client for repo unless one is already bound. A
// repository whose Orca Automation is enabled or unverifiable is refused
// until restart; every other error leaves the runtime unchanged.
func (r *Runtime) Track(ctx context.Context, repo config.Repository) error {
	if r.Clients[repo.Path] != nil {
		return nil
	}
	if ok, err := r.AutomationDisabled(ctx, repo); err != nil || !ok {
		refusal := fmt.Errorf("repository refused: %s: automation enabled or unverifiable; restart required", repo.Path)
		r.refused[repo.Path] = refusal
		return refusal
	}
	if _, err := issues.Backend(repo.Env); err != nil {
		return err
	}
	project := issues.Project(repo.Env)
	for _, known := range r.Repositories {
		if issues.Project(known.Env) == project {
			return errors.New("duplicate issue repository")
		}
	}
	client, err := r.newClient(ctx, repo.Env)
	if err != nil {
		return err
	}
	r.Clients[repo.Path] = client
	r.Repositories[repo.Path] = repo
	r.watchRunners(repo)
	return nil
}

// UpdateRepository replaces repo's stored configuration (OF-45). RefreshProjects
// calls this only after confirming repo's ledger identity keys match the
// previous configuration, so no bound GitHub/Feishu client is rebound.
func (r *Runtime) UpdateRepository(repo config.Repository) {
	r.Repositories[repo.Path] = repo
}

type SnapshotEvidenceError struct {
	Tickets []Ticket
	Err     error
}

func (e *SnapshotEvidenceError) Error() string { return e.Err.Error() }
func (e *SnapshotEvidenceError) Unwrap() error { return e.Err }

func (r *Runtime) Snapshot(ctx context.Context, repos []config.Repository) ([]Ticket, error) {
	groups := map[issues.Store][]config.Repository{}
	order := []issues.Store{}
	for _, repo := range repos {
		if r.refused[repo.Path] != nil {
			continue
		}
		client := r.Clients[repo.Path]
		if client == nil {
			return nil, errors.New("missing repository client")
		}
		if _, ok := groups[client]; !ok {
			order = append(order, client)
		}
		groups[client] = append(groups[client], repo)
	}
	tickets := []Ticket{}
	var problems []error
	for _, client := range order {
		rows, err := r.snapshotClient(ctx, client, groups[client])
		if err != nil {
			var evidence *SnapshotEvidenceError
			if !errors.As(err, &evidence) {
				return nil, err
			}
			rows = evidence.Tickets
			problems = append(problems, evidence.Err)
		}
		tickets = append(tickets, rows...)
	}
	if len(problems) != 0 {
		return nil, &SnapshotEvidenceError{Tickets: tickets, Err: errors.Join(problems...)}
	}
	return tickets, nil
}

func (r *Runtime) snapshotClient(ctx context.Context, reader issues.Store, repos []config.Repository) ([]Ticket, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	projects := []string{}
	paths := map[string]string{}
	scopeEnabled := map[string]bool{}
	for _, repo := range repos {
		if r.refused[repo.Path] != nil {
			continue
		}
		project := issues.Project(repo.Env)
		projects = append(projects, project)
		paths[project] = repo.Path
		scopeEnabled[repo.Path] = repo.Env["AGENT_SCOPE_AVOID"] != "0"
	}
	closed := map[string]map[string]closedTicket{}
	snapshot, err := reader.Snapshot(ctx, projects, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("issue snapshot failed: %w", err)
	}
	// Batch only within each configured client's authorization boundary.
	commentsByID := map[string][]task.Comment{}
	groups := map[issues.Store][]task.Issue{}
	for _, issue := range snapshot {
		path, ok := paths[issue.Project.ID]
		if !ok {
			path, ok = paths[issue.Project.Name]
		}
		if !ok || r.Clients[path] == nil {
			return nil, errors.New("snapshot project outside configured repositories")
		}
		groups[r.Clients[path]] = append(groups[r.Clients[path]], issue)
	}
	for client, rows := range groups {
		for _, issue := range rows {
			history, err := client.ListComments(ctx, issue.ID)
			if err != nil {
				return nil, fmt.Errorf("ownership snapshot failed: %w", err)
			}
			commentsByID[issue.ID] = history
		}
	}
	tickets := []Ticket{}
	var problems []error
	for _, issue := range snapshot {
		path, ok := paths[issue.Project.ID]
		if !ok {
			path, ok = paths[issue.Project.Name]
		}
		if !ok {
			return nil, errors.New("snapshot project outside configured repositories")
		}
		candidate, claimed, failed := issueEligibility(issue)
		// Ownership, not the auto-impl label, determines in-flight admission. Read
		// comments even when a user removed the label from a still-owned ticket.
		comments := commentsByID[issue.ID]
		ticket := Ticket{ID: issue.ID, Identifier: issue.Identifier, URL: issue.URL, Title: issue.Title, Repo: path, Priority: issue.Priority, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt}
		if err = readOwnership(&ticket, comments); err != nil {
			readFeishuNotification(&ticket, comments)
			ticket.MergeProblem = err.Error()
			tickets = append(tickets, ticket)
			problems = append(problems, err)
			continue
		}
		// Legacy failed labels without ownership need a new capacity reservation.
		if failed && ticket.Owner == "" {
			ticket.Owner = machine.OwnerHuman
			ticket.Phase = machine.PhaseHuman
		}
		readMergeHandoff(&ticket, comments)
		ticket.Resolution, err = readResolution(comments)
		readFeishuNotification(&ticket, comments)
		if err != nil {
			ticket.MergeProblem = err.Error()
			tickets = append(tickets, ticket)
			problems = append(problems, err)
			continue
		}
		// Cancellation cannot erase an already durable deployment barrier.
		// Done may also precede the final merge ownership receipt.
		canceled := issue.State.Name == "Canceled" || issue.State.Name == "Cancelled"
		if (canceled && !DeploymentBarrier(ticket)) || (issue.State.Name == "Done" && ticket.Owner != machine.OwnerMerge && ticket.Owner != machine.OwnerResolver && !DeploymentBarrier(ticket)) {
			if closed[path] == nil {
				closed[path] = map[string]closedTicket{}
			}
			head := ""
			if ticket.Handoff != nil {
				head = ticket.Handoff.Head
			}
			closed[path][ticket.Identifier] = closedTicket{State: issue.State.Name, Head: head}
			continue
		}
		ticket.Legacy = claimed && ticket.Owner == "" && !failed
		ticket.Candidate = candidate
		ticket.Blocked = issue.Blocked()
		ticket.Held, ticket.Number = issue.Held, issue.Number
		ticket.BlockersIncomplete = issue.InverseRelations.PageInfo.HasNextPage
		for _, relation := range issue.InverseRelations.Nodes {
			if relation.Type != "blocks" {
				continue
			}
			switch relation.Issue.State.Type {
			case "completed", "canceled", "duplicate":
			default:
				ticket.Blockers = append(ticket.Blockers, relation.Issue.Identifier)
			}
		}
		if ticket.Legacy || ticket.Candidate || owned(ticket) || ticket.Owner == machine.OwnerHuman || ticket.MergeProblem != "" || DeploymentBarrier(ticket) {
			if scopeEnabled[path] {
				ticket.Scope, err = r.ticketScope(ctx, ticket)
				if err != nil {
					// Match precheck: unavailable scope is unknown and must not
					// prevent this ticket or other repositories from scheduling.
					ticket.Scope = nil
				}
			}
			tickets = append(tickets, ticket)
		}
	}
	if r.closed == nil {
		r.closed = map[string]map[string]closedTicket{}
	}
	for _, repo := range paths {
		r.closed[repo] = closed[repo]
	}
	if len(problems) != 0 {
		return nil, &SnapshotEvidenceError{Tickets: tickets, Err: errors.Join(problems...)}
	}
	return tickets, nil
}

// issueEligibility shares only label/state policy. Ownership, blockers and
// complete fresh evidence remain independent checks at the action boundary.
func issueEligibility(issue task.Issue) (candidate, claimed, failed bool) {
	ready := false
	for _, label := range issue.Labels.Nodes {
		switch label.Name {
		case "ready-for-agent":
			ready = true
		case "auto-impl":
			claimed = true
		case "auto-failed":
			failed = true
		}
	}
	candidate = ready && !claimed && !failed && (issue.State.Name == "Todo" || issue.State.Name == "Backlog")
	return candidate, claimed, failed
}

func readOwnership(t *Ticket, comments []task.Comment) error {
	comments = append([]task.Comment(nil), comments...)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	schedulerAttempt := 0
	for _, comment := range comments {
		// A handoff hands the ticket to a human even when an older executor omitted the ownership line.
		if strings.HasPrefix(comment.Body, "转人: ") {
			t.Owner = machine.OwnerHuman
			t.Phase = machine.PhaseHuman
		}
		for _, line := range strings.Split(comment.Body, "\n") {
			if strings.HasPrefix(line, "阶段: ") {
				t.Phase = machine.Phase(strings.TrimPrefix(line, "阶段: "))
			}
			if !strings.HasPrefix(line, "归属: ") {
				continue
			}
			fields := strings.Fields(strings.TrimPrefix(line, "归属: "))
			if len(fields) == 0 {
				return errors.New("invalid ownership comment")
			}
			owner := machine.Owner(fields[0])
			switch owner {
			case machine.OwnerNone, machine.OwnerExecutor, machine.OwnerMerge, machine.OwnerHuman, machine.OwnerResolver:
			default:
				return errors.New("unknown ticket owner")
			}
			t.Owner = owner
			t.Terminal = ""
			t.Run = ""
			for _, field := range fields[1:] {
				key, value, ok := strings.Cut(field, "=")
				if !ok {
					continue
				}
				switch key {
				case "term":
					// "-" is the scheduler's claim marker: no executor has registered yet.
					t.Terminal = value
				case "run":
					if value != "-" {
						t.Run = value
					}
				case "attempt", "scheduler-attempt":
					n, err := strconv.Atoi(value)
					if err != nil || n < 0 {
						return errors.New("invalid ownership attempt")
					}
					if key == "scheduler-attempt" {
						schedulerAttempt = max(schedulerAttempt, n)
					} else {
						t.Attempt = n
					}
				}
			}
			if owner == machine.OwnerNone {
				schedulerAttempt = 0
				t.Attempt = 0
			}
		}
	}
	t.Attempt = max(t.Attempt, schedulerAttempt)
	return nil
}

func (r *Runtime) Apply(ctx context.Context, t Ticket, a Action) error {
	if a.Kind == Claim || a.Kind == Resume || (a.Kind == Resolve && (t.Resolution == nil || t.Resolution.Phase != "result")) {
		release, err := maintenance.Admission()
		if err != nil {
			return err
		}
		defer release()
	}
	if err := r.verifyFreshDecision(ctx, t, a.Kind); err != nil {
		return err
	}
	client := r.Clients[t.Repo]
	if client == nil {
		return errors.New("missing repository client")
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	owner := machine.OwnerExecutor
	switch a.Kind {
	case Claim:
		issue, err := r.claimIssue(ctx, client, t)
		if err != nil {
			return err
		}
		candidate, _, _ := issueEligibility(issue)
		if !candidate || issue.Blocked() || issue.Labels.PageInfo.HasNextPage || issue.Relations.PageInfo.HasNextPage {
			return errors.New("claim candidate changed")
		}
		comments, err := client.ListComments(ctx, t.ID)
		if err != nil {
			return errors.New("claim ownership unavailable")
		}
		fresh := Ticket{ID: t.ID, Identifier: t.Identifier, Repo: t.Repo}
		if err := readOwnership(&fresh, comments); err != nil {
			return err
		}
		readMergeHandoff(&fresh, comments)
		if (fresh.Owner != "" && fresh.Owner != machine.OwnerNone) || fresh.Handoff != nil || fresh.MergeProblem != "" || DeploymentBarrier(fresh) {
			return errors.New("claim ownership changed")
		}
		issue, err = client.UpdateIssue(ctx, t.ID, "In Progress", []string{"auto-impl"}, nil)
		if err != nil {
			return errors.New("ticket claim update failed")
		}
		_, claimed, _ := issueEligibility(issue)
		if !claimed {
			return errors.New("ticket claim not confirmed")
		}
	case Resume:
	case Merge:
		owner = machine.OwnerMerge
	case Release:
		if _, err := client.UpdateIssue(ctx, t.ID, "Todo", nil, []string{"auto-impl"}); err != nil {
			return errors.New("ticket release failed")
		}
		owner = machine.OwnerNone
	case Resolve:
		if t.Resolution != nil && t.Resolution.Phase == "result" {
			return r.applyResolution(ctx, t)
		}
		if _, err := client.UpdateIssue(ctx, t.ID, "", []string{"auto-impl", "auto-failed"}, []string{"ready-for-agent"}); err != nil {
			return errors.New("resolver labels unconfirmed")
		}
		owner = machine.OwnerResolver
	default:
		return errors.New("invalid scheduler action")
	}
	body := fmt.Sprintf("阶段: %s\n归属: %s term=- run=- attempt=%d scheduler-attempt=%d since=%s", a.Phase, owner, a.Attempt, a.Attempt, now.UTC().Format(time.RFC3339))
	if a.Kind == Resolve {
		if t.Run != "" {
			body = strings.Replace(body, " run=- ", " run="+t.Run+" ", 1)
		}
		body = "Agent处置: " + a.Reason + "\n证据: scheduler inspection\n处置要求: 完成处置并提交可验证的继续执行提案\n" + body
	}
	if _, err := client.CreateComment(ctx, t.ID, body); err != nil {
		return errors.New("ownership comment failed")
	}
	return nil
}

// claimIssue rereads the ticket itself; absence is unknown, never permission
// to reuse the ticket supplied by discovery.
func (r *Runtime) claimIssue(ctx context.Context, client issues.Store, t Ticket) (task.Issue, error) {
	repo := r.Repositories[t.Repo]
	project := issues.Project(repo.Env)
	issue, err := client.FreshIssue(ctx, t.ID)
	if err != nil {
		return task.Issue{}, errors.New("claim issue unavailable")
	}
	if issue.ID != t.ID || issue.Identifier != t.Identifier || project == "" || (issue.Project.ID != project && issue.Project.Name != project) {
		return task.Issue{}, errors.New("claim issue identity changed")
	}
	return issue, nil
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func (r *Runtime) Launch(ctx context.Context, t Ticket) error {
	release, err := maintenance.Admission()
	if err != nil {
		return err
	}
	defer release()
	if err := r.verifyFreshLaunch(ctx, t); err != nil {
		return err
	}
	if !identifier.MatchString(t.Identifier) || !filepath.IsAbs(r.DriverPath) {
		return errors.New("invalid executor launch configuration")
	}
	verb := " exec "
	if t.Owner == machine.OwnerResolver {
		if t.Resolution != nil && t.Resolution.Phase == "result" {
			return nil
		}
		verb = " resolve "
	}
	if t.Phase == machine.PhaseCorrection && t.Handoff != nil && t.Progress != nil && t.Progress.Phase == "correction" && t.Progress.QueueID == t.Handoff.QueueID {
		// A merge-lane conflict resume runs the executor as a merge resolver.
		verb = " exec --merging "
	}
	_, err = r.Orca.TerminalCreate(ctx, orca.TerminalOptions{Worktree: "path:" + t.Repo, Title: "driver" + verb + t.Identifier, Command: "exec " + quote(r.DriverPath) + verb + quote(t.Identifier) + launchLog(t.Identifier)})
	if err != nil {
		return errors.New("executor launch uncertain")
	}
	return nil
}
func (r *Runtime) command(ctx context.Context, dir, path string, args ...string) ([]byte, error) {
	run := r.Run
	if run == nil {
		run = runx.Run
	}
	return run(ctx, runx.Spec{Path: path, Args: args, Dir: dir, Timeout: 30 * time.Second})
}

// ticketScope stays local to the scheduler because the shared snapshot
// deliberately carries only scheduling metadata, not issue descriptions.
func (r *Runtime) ticketScope(ctx context.Context, t Ticket) ([]string, error) {
	description, err := r.Clients[t.Repo].Description(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	return declaredScope(description), nil
}

// ActualChanges replaces declared scope only after every remote unit diff was
// read successfully. Pruning prevents a deleted branch from blocking admission.
func (r *Runtime) ActualChanges(ctx context.Context, t Ticket) ([]string, bool, error) {
	if !identifier.MatchString(t.Identifier) {
		return nil, false, errors.New("invalid ticket identifier")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := r.command(ctx, t.Repo, "git", "fetch", "--quiet", "--prune", "origin", "+refs/heads/agent/*:refs/remotes/origin/agent/*"); err != nil {
		return nil, false, errors.New("scope fetch failed")
	}
	prefix := "refs/remotes/origin/agent/" + t.Identifier + "/"
	raw, err := r.command(ctx, t.Repo, "git", "for-each-ref", "--format=%(refname)", prefix)
	if err != nil {
		return nil, false, errors.New("scope refs read failed")
	}
	refs := strings.Fields(string(raw))
	if len(refs) == 0 {
		return nil, false, nil
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, prefix) {
			return nil, false, errors.New("unexpected scope ref")
		}
		raw, err = r.command(ctx, t.Repo, "git", "diff", "--name-only", "--no-renames", "-z", "origin/main..."+ref, "--")
		if err != nil {
			return nil, false, errors.New("scope diff failed")
		}
		for _, file := range strings.Split(string(raw), "\x00") {
			if file != "" {
				seen[file] = true
			}
		}
	}
	files := make([]string, 0, len(seen))
	for file := range seen {
		files = append(files, file)
	}
	sort.Strings(files)
	return files, true, nil
}

// launchLog keeps an executor's stderr after its terminal closes, so a failed
// start leaves evidence. The identifier is validated before it reaches the shell.
func launchLog(identifier string) string {
	return ` 2>>"$HOME/.local/state/orca-factory/launch-` + identifier + `.log"`
}
