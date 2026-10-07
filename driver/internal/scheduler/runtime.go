package scheduler

import (
	"context"
	"encoding/json"
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
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type LinearClient interface {
	Snapshot(context.Context, []string, time.Time) ([]linear.Issue, error)
	ListComments(context.Context, string) ([]linear.Comment, error)
	UpdateIssue(context.Context, string, string, []string, []string) (linear.Issue, error)
	CreateComment(context.Context, string, string) (linear.Comment, error)
}
type OrcaClient interface {
	WorkerRelease(context.Context, string, string) (orca.Receipt, error)
	WorktreeSet(context.Context, string, string, string, string) (orca.Receipt, error)
	AutomationsList(context.Context) (orca.Receipt, error)
	WorkerList(context.Context, orca.ListOptions) (orca.Receipt, error)
	TerminalCreate(context.Context, orca.TerminalOptions) (orca.Receipt, error)
}
type Runtime struct {
	Releases             map[string]*release.Manager
	ReleaseEligible      map[string]bool
	GitHub               *github.Client
	Reader               LinearClient
	Clients              map[string]LinearClient
	Repositories         map[string]config.Repository
	Orca                 OrcaClient
	OrcaPath, DriverPath string
	Run                  func(context.Context, runx.Spec) ([]byte, error)
	Now                  func() time.Time
	Tuning               config.Tuning
	refused              map[string]error
}

// NewRuntime binds existing clients. Startup exclusion runs before any Linear
// initialization; Tick repeats the check before each inspection.
func NewRuntime(ctx context.Context, c config.Config) (*Runtime, error) {
	path, err := runx.OrcaPath()
	if err != nil {
		return nil, err
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, errors.New("cannot locate driver binary")
	}
	r := &Runtime{
		Tuning:       c.Tuning,
		Clients:      map[string]LinearClient{},
		Repositories: map[string]config.Repository{},
		Orca:         &orca.Client{Path: path, WorkerStartWait: c.Tuning.Orca.WorkerStartWait, Timeout: c.Tuning.Orca.CallTimeout},
		OrcaPath:     path,
		DriverPath:   binary,
	}
	err = r.initialize(ctx, c, func(ctx context.Context, team string) (LinearClient, error) {
		return linear.New(ctx, linear.Options{TeamID: team, HTTPTimeout: c.Tuning.Linear.HTTPTimeout})
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Runtime) initialize(ctx context.Context, c config.Config, newClient func(context.Context, string) (LinearClient, error)) error {
	r.Clients = map[string]LinearClient{}
	r.Repositories = map[string]config.Repository{}
	r.refused = map[string]error{}
	teams := map[string]LinearClient{}
	projects := map[string]bool{}
	var refusals []error
	for _, repo := range c.Repositories {
		ok, err := r.AutomationDisabled(ctx, repo)
		if err != nil || !ok {
			refusal := fmt.Errorf("repository refused: %s: automation enabled or unverifiable; restart required", repo.Path)
			r.refused[repo.Path] = refusal
			refusals = append(refusals, refusal)
			continue
		}
		team, project := repo.Env["AGENT_LINEAR_TEAM"], repo.Env["AGENT_LINEAR_PROJECT"]
		if team == "" || project == "" || projects[project] {
			return errors.New("repository requires distinct Linear project and team")
		}
		projects[project] = true
		client := teams[team]
		if client == nil {
			bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
			client, err = newClient(bounded, team)
			cancel()
			if err != nil {
				return errors.New("linear initialization failed")
			}
			teams[team] = client
		}
		r.Clients[repo.Path] = client
		r.Repositories[repo.Path] = repo
		if r.Reader == nil {
			r.Reader = client
		}
	}
	if len(r.Clients) == 0 {
		return errors.Join(refusals...)
	}
	return nil
}
func (r *Runtime) Snapshot(ctx context.Context, repos []config.Repository) ([]Ticket, error) {
	// One scheduler inspection is one ordinary-read round. Shared clients reuse
	// duplicate comment reads while critical writes continue to use fresh reads.
	ctx = linear.BeginRound(ctx)
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
		project := repo.Env["AGENT_LINEAR_PROJECT"]
		projects = append(projects, project)
		paths[project] = repo.Path
		scopeEnabled[repo.Path] = repo.Env["AGENT_SCOPE_AVOID"] != "0"
	}
	issues, err := r.Reader.Snapshot(ctx, projects, time.Time{})
	if err != nil {
		return nil, errors.New("linear snapshot failed")
	}
	tickets := []Ticket{}
	for _, issue := range issues {
		path, ok := paths[issue.Project.ID]
		if !ok {
			path, ok = paths[issue.Project.Name]
		}
		if !ok {
			return nil, errors.New("snapshot project outside configured repositories")
		}
		ready, claimed, failed := false, false, false
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
		// Ownership, not the auto-impl label, determines in-flight admission. Read
		// comments even when a user removed the label from a still-owned ticket.
		comments, err := r.Clients[path].ListComments(ctx, issue.ID)
		if err != nil {
			return nil, errors.New("ownership snapshot failed")
		}
		ticket := Ticket{ID: issue.ID, Identifier: issue.Identifier, Repo: path, Priority: issue.Priority, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt}
		if err = readOwnership(&ticket, comments); err != nil {
			return nil, err
		}
		// Legacy failed labels without ownership need a new capacity reservation.
		if failed && ticket.Owner == "" {
			ticket.Owner = machine.OwnerHuman
			ticket.Phase = machine.PhaseHuman
		}
		readMergeHandoff(&ticket, comments)
		ticket.Resolution, err = readResolution(comments)
		if err != nil {
			return nil, err
		}
		// Cancellation cannot erase an already durable deployment barrier.
		// Done may also precede the final merge ownership receipt.
		canceled := issue.State.Name == "Canceled" || issue.State.Name == "Cancelled"
		if (canceled && !DeploymentBarrier(ticket)) || (issue.State.Name == "Done" && ticket.Owner != machine.OwnerMerge && ticket.Owner != machine.OwnerResolver && !DeploymentBarrier(ticket)) {
			continue
		}
		ticket.Legacy = claimed && ticket.Owner == "" && !failed
		ticket.Candidate = ready && !claimed && !failed && (issue.State.Name == "Todo" || issue.State.Name == "Backlog")
		ticket.Blocked = issue.Blocked()
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
		if ticket.Legacy || ticket.Candidate || owned(ticket) || ticket.Owner == machine.OwnerHuman || DeploymentBarrier(ticket) {
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
	return tickets, nil
}

func readOwnership(t *Ticket, comments []linear.Comment) error {
	comments = append([]linear.Comment(nil), comments...)
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
		if _, err := linear.CriticalComments(ctx, client, t.ID); err != nil {
			return errors.New("linear claim evidence unavailable")
		}
		issue, err := client.UpdateIssue(ctx, t.ID, "In Progress", []string{"auto-impl"}, nil)
		if err != nil {
			return errors.New("linear claim failed")
		}
		claimed := false
		for _, label := range issue.Labels.Nodes {
			if label.Name == "auto-impl" {
				claimed = true
			}
		}
		if !claimed {
			return errors.New("linear claim not confirmed")
		}
	case Resume:
	case Merge:
		owner = machine.OwnerMerge
	case Release:
		if _, err := client.UpdateIssue(ctx, t.ID, "Todo", nil, []string{"auto-impl"}); err != nil {
			return errors.New("linear release failed")
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

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func (r *Runtime) Launch(ctx context.Context, t Ticket) error {
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
	_, err := r.Orca.TerminalCreate(ctx, orca.TerminalOptions{Worktree: "path:" + t.Repo, Title: "driver" + verb + t.Identifier, Command: "exec " + quote(r.DriverPath) + verb + quote(t.Identifier) + launchLog(t.Identifier)})
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

// ticketScope stays local to the scheduler because the shared Linear snapshot
// deliberately carries only scheduling metadata, not issue descriptions.
func (r *Runtime) ticketScope(ctx context.Context, t Ticket) ([]string, error) {
	raw, err := r.command(ctx, t.Repo, r.OrcaPath, "linear", "issue", t.Identifier, "--json")
	if err != nil {
		return nil, errors.New("ticket scope read failed")
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Issue *struct {
				Description string `json:"description"`
			} `json:"issue"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || !envelope.OK || envelope.Result.Issue == nil {
		return nil, errors.New("invalid ticket scope response")
	}
	return declaredScope(envelope.Result.Issue.Description), nil
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

// ProbeLinearQuota refreshes the usage headers of every Linear client with one cheap query.
func (r *Runtime) ProbeLinearQuota(ctx context.Context) error {
	var errs []error
	for _, client := range r.Clients {
		if prober, ok := client.(interface{ ProbeQuota(context.Context) error }); ok {
			errs = append(errs, prober.ProbeQuota(ctx))
		}
	}
	return errors.Join(errs...)
}
