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
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type LinearClient interface {
	Snapshot(context.Context, []string, time.Time) ([]linear.Issue, error)
	ListComments(context.Context, string) ([]linear.Comment, error)
	UpdateIssue(context.Context, string, string, []string, []string) (linear.Issue, error)
	CreateComment(context.Context, string, string) (linear.Comment, error)
}
type OrcaClient interface {
	AutomationsList(context.Context) (orca.Receipt, error)
	WorkerList(context.Context, orca.ListOptions) (orca.Receipt, error)
	TerminalCreate(context.Context, orca.TerminalOptions) (orca.Receipt, error)
}
type Runtime struct {
	Reader               LinearClient
	Clients              map[string]LinearClient
	Repositories         map[string]config.Repository
	Orca                 OrcaClient
	OrcaPath, DriverPath string
	Run                  func(context.Context, runx.Spec) ([]byte, error)
	Now                  func() time.Time
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
	r := &Runtime{Clients: map[string]LinearClient{}, Repositories: map[string]config.Repository{}, Orca: &orca.Client{Path: path}, OrcaPath: path, DriverPath: binary}
	teams := map[string]LinearClient{}
	projects := map[string]bool{}
	for _, repo := range c.Repositories {
		ok, err := r.AutomationDisabled(ctx, repo)
		if err != nil || !ok {
			return nil, errors.New("repository refused: automation enabled or unverifiable")
		}
		team, project := repo.Env["AGENT_LINEAR_TEAM"], repo.Env["AGENT_LINEAR_PROJECT"]
		if team == "" || project == "" || projects[project] {
			return nil, errors.New("repository requires distinct Linear project and team")
		}
		projects[project] = true
		client := teams[team]
		if client == nil {
			bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
			client, err = linear.New(bounded, linear.Options{TeamID: team})
			cancel()
			if err != nil {
				return nil, errors.New("Linear initialization failed")
			}
			teams[team] = client
		}
		r.Clients[repo.Path] = client
		r.Repositories[repo.Path] = repo
		if r.Reader == nil {
			r.Reader = client
		}
	}
	return r, nil
}
func (r *Runtime) Snapshot(ctx context.Context, repos []config.Repository) ([]Ticket, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	projects := []string{}
	paths := map[string]string{}
	for _, repo := range repos {
		project := repo.Env["AGENT_LINEAR_PROJECT"]
		projects = append(projects, project)
		paths[project] = repo.Path
	}
	issues, err := r.Reader.Snapshot(ctx, projects, time.Time{})
	if err != nil {
		return nil, errors.New("Linear snapshot failed")
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
		if issue.State.Name == "Done" || issue.State.Name == "Canceled" || issue.State.Name == "Cancelled" {
			continue
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
		ticket.Legacy = claimed && ticket.Owner == "" && !failed
		ticket.Candidate = ready && !claimed && !failed && (issue.State.Name == "Todo" || issue.State.Name == "Backlog")
		if ticket.Legacy || ticket.Candidate || owned(ticket) {
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
			case machine.OwnerNone, machine.OwnerExecutor, machine.OwnerMerge, machine.OwnerHuman:
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
					if value != "-" {
						t.Terminal = value
					}
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
		issue, err := client.UpdateIssue(ctx, t.ID, "In Progress", []string{"auto-impl"}, nil)
		if err != nil {
			return errors.New("Linear claim failed")
		}
		claimed := false
		for _, label := range issue.Labels.Nodes {
			if label.Name == "auto-impl" {
				claimed = true
			}
		}
		if !claimed {
			return errors.New("Linear claim not confirmed")
		}
	case Resume:
	case Merge:
		owner = machine.OwnerMerge
	case Release:
		if _, err := client.UpdateIssue(ctx, t.ID, "Todo", nil, []string{"auto-impl"}); err != nil {
			return errors.New("Linear release failed")
		}
		owner = machine.OwnerNone
	case Human:
		if _, err := client.UpdateIssue(ctx, t.ID, "In Progress", []string{"auto-impl", "auto-failed"}, []string{"ready-for-agent"}); err != nil {
			return errors.New("Linear escalation failed")
		}
		owner = machine.OwnerHuman
	default:
		return errors.New("invalid scheduler action")
	}
	body := fmt.Sprintf("阶段: %s\n归属: %s term=- run=- attempt=%d scheduler-attempt=%d since=%s", a.Phase, owner, a.Attempt, a.Attempt, now.UTC().Format(time.RFC3339))
	if a.Kind == Human {
		body = "转人: " + a.Reason + "\n证据: scheduler inspection\n需要你: 检查执行器并决定是否重新排队\n" + body
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
	_, err := r.Orca.TerminalCreate(ctx, orca.TerminalOptions{Worktree: "path:" + t.Repo, Title: "driver exec " + t.Identifier, Command: "exec " + quote(r.DriverPath) + " exec " + quote(t.Identifier)})
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
