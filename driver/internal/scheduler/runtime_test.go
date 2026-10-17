package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// openRecord projects a fake issue as a managed, open backend record.
func openRecord(issue task.Issue) issues.Task {
	return issues.Task{Issue: issue, URL: "https://tickets.example/" + issue.ID, Complete: true, NativeState: "open", Metadata: issues.Metadata{Complete: true}}
}

type fakeIssues struct {
	issues.Store
	updates       int
	comments      []string
	issueComments []task.Comment
	issues        []task.Issue
	projects      []string
	description   string
}

func (f *fakeIssues) Snapshot(_ context.Context, p []string, _ time.Time) ([]task.Issue, error) {
	f.projects = p
	return f.issues, nil
}
func (f *fakeIssues) ListComments(context.Context, string) ([]task.Comment, error) {
	return f.issueComments, nil
}
func (f *fakeIssues) FreshIssue(_ context.Context, id string) (task.Issue, error) {
	for _, issue := range f.issues {
		if issue.ID == id {
			return issue, nil
		}
	}
	return task.Issue{}, errors.New("issue not found")
}

// Read treats a ticket outside the fixture's issue list as a managed open record.
func (f *fakeIssues) Read(ctx context.Context, id string) (issues.Task, error) {
	issue, err := f.FreshIssue(ctx, id)
	if err != nil {
		issue = task.Issue{ID: id}
	}
	return openRecord(issue), nil
}
func (f *fakeIssues) Description(context.Context, string) (string, error) { return f.description, nil }
func (f *fakeIssues) UpdateIssue(context.Context, string, string, []string, []string) (task.Issue, error) {
	f.updates++
	return task.Issue{ID: "id", Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "auto-impl"}}}}, nil
}
func (f *fakeIssues) CreateComment(_ context.Context, _ string, b string) (task.Comment, error) {
	f.comments = append(f.comments, b)
	c := task.Comment{ID: "comment", Body: b, CreatedAt: time.Now()}
	f.issueComments = append(f.issueComments, c)
	return c, nil
}

func evidenceComments(bodies ...string) []task.Comment {
	comments := make([]task.Comment, 0, len(bodies))
	for i, body := range bodies {
		comments = append(comments, task.Comment{ID: fmt.Sprint("evidence", i), Body: body, CreatedAt: time.Unix(int64(i+1), 0)})
	}
	return comments
}

// fromEvidence derives a ticket from durable comments the way Snapshot does,
// so the fresh backend reread before a decision sees the same evidence.
func fromEvidence(t *testing.T, base Ticket, comments []task.Comment) Ticket {
	t.Helper()
	if err := readOwnership(&base, comments); err != nil {
		t.Fatal(err)
	}
	readMergeHandoff(&base, comments)
	record, err := readResolution(comments)
	if err != nil {
		t.Fatal(err)
	}
	base.Resolution = record
	return base
}

type fakeOrca struct {
	automations orca.Receipt
	pages       []orca.Receipt
	page        int
	launches    []orca.TerminalOptions
	cards       []orca.BoardCard
}

func (f *fakeOrca) AutomationsList(context.Context) (orca.Receipt, error) { return f.automations, nil }
func (f *fakeOrca) WorkerList(context.Context, orca.ListOptions) (orca.Receipt, error) {
	if f.page >= len(f.pages) {
		return nil, errors.New("missing page")
	}
	r := f.pages[f.page]
	f.page++
	return r, nil
}
func (f *fakeOrca) TerminalCreate(_ context.Context, o orca.TerminalOptions) (orca.Receipt, error) {
	f.launches = append(f.launches, o)
	return orca.Receipt{"terminal": json.RawMessage(`{"handle":"term_new"}`)}, nil
}
func (f *fakeOrca) WorkerAbandon(context.Context, string, string) (orca.Receipt, error) {
	return orca.Receipt{}, nil
}
func (f *fakeOrca) WorkerStop(context.Context, string, string) (orca.Receipt, error) {
	return orca.Receipt{}, nil
}
func (f *fakeOrca) TerminalClose(context.Context, string) error { return nil }

func receipt(s string) orca.Receipt { var r orca.Receipt; _ = json.Unmarshal([]byte(s), &r); return r }

type startupOrca struct {
	fakeOrca
	reads    int
	failRead int
}

func (f *startupOrca) AutomationsList(context.Context) (orca.Receipt, error) {
	f.reads++
	if f.reads == f.failRead {
		return nil, errors.New("unavailable")
	}
	return f.automations, nil
}

func TestStartupRefusesOnlyEnabledRepository(t *testing.T) {
	for _, badFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("badFirst=%v", badFirst), func(t *testing.T) {
			base := setup(nil)
			cfg, repos := base.Config, base.Repositories
			repos[0].Path = "/a"
			repos[0].Env = map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p-a"}
			repos[1].Path = "/b"
			repos[1].Env = map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p-b"}
			oc := &startupOrca{fakeOrca: fakeOrca{automations: receipt(`{"automations":[{"enabled":false,"runContext":{"path":"/a"}},{"enabled":true,"runContext":{"path":"/b"}}]}`)}}
			if badFirst {
				repos[0], repos[1] = repos[1], repos[0]
			}
			lc := &fakeIssues{issues: []task.Issue{{ID: "a-ticket", Identifier: "ENG-1", Project: task.NamedID{ID: "p-a"}, State: task.NamedID{Name: "Todo"}, Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "ready-for-agent"}}}}}}
			r := &Runtime{Orca: oc, DriverPath: "/driver"}
			var initialized []string
			err := r.initialize(context.Background(), repos, func(_ context.Context, env map[string]string) (issues.Store, error) {
				initialized = append(initialized, env["AGENT_GH_REPO"])
				return lc, nil
			})
			if err != nil {
				t.Fatalf("startup rejected healthy repository: %v", err)
			}
			if !reflect.DeepEqual(initialized, []string{"p-a"}) {
				t.Fatalf("initialized clients for refused repository: %v", initialized)
			}
			// Even after B's automation is disabled, it needs a restart to
			// acquire its client; A must still be inspected and claimed.
			oc.automations = receipt(`{"automations":[]}`)
			s := &Scheduler{Config: cfg, Repositories: repos, Backend: r}
			err = s.Tick(context.Background())
			if err == nil || !strings.Contains(err.Error(), "repository refused") {
				t.Fatalf("refusal was not reported: %v", err)
			}
			if !reflect.DeepEqual(lc.projects, []string{"p-a"}) || lc.updates != 1 || len(lc.comments) != 1 || len(oc.launches) != 1 || oc.launches[0].Worktree != "path:/a" {
				t.Fatalf("healthy repository not served: projects=%v updates=%d comments=%v launches=%v", lc.projects, lc.updates, lc.comments, oc.launches)
			}
		})
	}
}

func TestTrackRetriesUnverifiableAutomation(t *testing.T) {
	base := setup(nil)
	cfg, repos := base.Config, base.Repositories
	repos[0].Path = "/a"
	repos[0].Env = map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p-a"}
	repos[1].Path = "/b"
	repos[1].Env = map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p-b"}
	oc := &startupOrca{fakeOrca: fakeOrca{automations: receipt(`{"automations":[{"enabled":true,"runContext":{"path":"/a"}},{"enabled":false,"runContext":{"path":"/b"}}]}`)}, failRead: 2}
	lc := &fakeIssues{issues: []task.Issue{{ID: "b-ticket", Identifier: "ENG-2", Project: task.NamedID{ID: "p-b"}, State: task.NamedID{Name: "Todo"}, Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "ready-for-agent"}}}}}}
	r := &Runtime{Orca: oc, DriverPath: "/driver"}
	var initialized []string
	err := r.initialize(context.Background(), repos, func(_ context.Context, env map[string]string) (issues.Store, error) {
		initialized = append(initialized, env["AGENT_GH_REPO"])
		return lc, nil
	})
	if err != nil {
		t.Fatalf("unverifiable automation failed startup: %v", err)
	}
	if len(initialized) != 0 {
		t.Fatalf("initialized clients before automation was verified: %v", initialized)
	}
	// /a stays refused even once its automation is gone; /b is retried.
	oc.automations = receipt(`{"automations":[]}`)
	s := &Scheduler{Config: cfg, Repositories: repos, Backend: r}
	err = s.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "repository refused") {
		t.Fatalf("enabled automation refusal was not reported: %v", err)
	}
	if !reflect.DeepEqual(initialized, []string{"p-b"}) || !reflect.DeepEqual(lc.projects, []string{"p-b"}) || len(oc.launches) != 1 || oc.launches[0].Worktree != "path:/b" {
		t.Fatalf("unverifiable repository not taken over: initialized=%v projects=%v launches=%v", initialized, lc.projects, oc.launches)
	}
}

func TestStartupAllRefusedFailsWithoutInitializingClients(t *testing.T) {
	repos := setup(nil).Repositories
	oc := &fakeOrca{automations: receipt(`{"automations":[{"enabled":true,"runContext":{"path":"a"}},{"enabled":true,"runContext":{"path":"b"}}]}`)}
	r := &Runtime{Orca: oc}
	err := r.initialize(context.Background(), repos, func(context.Context, map[string]string) (issues.Store, error) {
		t.Fatal("initialized client for refused repository")
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "repository refused: a") || !strings.Contains(err.Error(), "repository refused: b") {
		t.Fatalf("all-repository refusal was not reported: %v", err)
	}
}
func TestClaimOneUpdateOneCommentAndSafeLaunch(t *testing.T) {
	lc := &fakeIssues{issues: []task.Issue{{ID: "id", Identifier: "ENG-1", Project: task.NamedID{ID: "p1"}, State: task.NamedID{Name: "Todo"}, Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "ready-for-agent"}}}}}}
	oc := &fakeOrca{}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": lc}, Repositories: map[string]config.Repository{"/repo": {Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p1"}}}, Orca: oc, DriverPath: "/opt/driver with space"}
	ticket := Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo"}
	if err := r.Apply(context.Background(), ticket, Action{Kind: Claim, Attempt: 1, Phase: machine.PhaseWorker}); err != nil {
		t.Fatal(err)
	}
	if lc.updates != 1 || len(lc.comments) != 1 || !strings.Contains(lc.comments[0], "scheduler-attempt=1") {
		t.Fatalf("updates=%d comments=%v", lc.updates, lc.comments)
	}
	ticket.Owner, ticket.Phase, ticket.Attempt = machine.OwnerExecutor, machine.PhaseWorker, 1
	if err := r.Launch(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(oc.launches) != 1 || oc.launches[0].Command != "exec '/opt/driver with space' exec 'ENG-1' 2>>\"$HOME/.local/state/orca-factory/launch-ENG-1.log\"" {
		t.Fatalf("launch=%v", oc.launches)
	}
	ticket.Identifier = "ENG-1; touch secret"
	if err := r.Launch(context.Background(), ticket); err == nil {
		t.Fatal("accepted invalid ticket")
	}
}
func TestResumeOnlyWritesOwnership(t *testing.T) {
	lc := &fakeIssues{}
	r := &Runtime{Clients: map[string]issues.Store{"repo": lc}}
	if err := r.Apply(context.Background(), Ticket{ID: "id", Repo: "repo"}, Action{Kind: Resume, Attempt: 2, Phase: machine.PhaseGate}); err != nil {
		t.Fatal(err)
	}
	if lc.updates != 0 || len(lc.comments) != 1 {
		t.Fatal("resume reclaimed ticket")
	}
}
func TestOwnershipPreservesSchedulerAttempt(t *testing.T) {
	comments := []task.Comment{
		{CreatedAt: time.Unix(1, 0), Body: "阶段: gate\n归属: 执行器 term=- run=- attempt=2 scheduler-attempt=2"},
		{CreatedAt: time.Unix(2, 0), Body: "阶段: worker 运行中\n归属: 执行器 term=term_new run=run_new attempt=1 since=2026-01-01T00:00:00Z"},
	}
	ticket := Ticket{}
	if err := readOwnership(&ticket, comments); err != nil {
		t.Fatal(err)
	}
	if ticket.Attempt != 2 || ticket.Terminal != "term_new" || ticket.Run != "run_new" || ticket.Owner != machine.OwnerExecutor {
		t.Fatalf("ticket=%+v", ticket)
	}
}
func TestRuntimeSnapshotAllProjects(t *testing.T) {
	lc := &fakeIssues{issues: []task.Issue{{ID: "id", Identifier: "ENG-1", Priority: 2, CreatedAt: time.Unix(1, 0), Project: task.NamedID{ID: "p2"}, State: task.NamedID{Name: "Todo"}, Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "ready-for-agent"}}}}}}
	lc.description = "## SCOPE\nsrc/*"
	r := &Runtime{Clients: map[string]issues.Store{"a": lc, "b": lc}}
	tickets, err := r.Snapshot(context.Background(), []config.Repository{{Path: "a", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p1"}}, {Path: "b", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lc.projects, []string{"p1", "p2"}) || len(tickets) != 1 || tickets[0].Repo != "b" || !tickets[0].Candidate || tickets[0].Priority != 2 || !reflect.DeepEqual(tickets[0].Scope, []string{"src/*"}) {
		t.Fatalf("tickets=%v projects=%v", tickets, lc.projects)
	}
}
func TestRuntimeAutomationRequiresExplicitDisabled(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"automations":[{"enabled":true,"runContext":{"path":"/repo"}}]}`, false},
		{`{"automations":[{"enabled":false,"runContext":{"path":"/repo"}}]}`, true},
		{`{"automations":[]}`, true}, {`{}`, false},
		{`{"automations":[{"runContext":{"path":"/repo"}}]}`, false},
	} {
		r := &Runtime{Orca: &fakeOrca{automations: receipt(tc.raw)}}
		ok, _ := r.AutomationDisabled(context.Background(), config.Repository{Path: "/repo"})
		if ok != tc.want {
			t.Errorf("%s: %v", tc.raw, ok)
		}
	}
}
func TestTerminalAbsenceRejectsTruncatedOrFailedRead(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"ok":true,"result":{"terminals":[],"totalCount":0,"truncated":false,"hostScope":{"omittedHostIds":[]}}}`, true},
		{`{"ok":true,"result":{"terminals":[],"totalCount":3,"truncated":true,"hostScope":{"omittedHostIds":[]}}}`, false},
		{`{"ok":true,"result":{"terminals":[],"totalCount":0,"truncated":false,"hostScope":{"omittedHostIds":["remote"]}}}`, false},
		{`{"ok":false}`, false}, {`{"ok":true,"result":{}}`, false},
	} {
		r := &Runtime{OrcaPath: "/orca", Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
			if s.Path != "/orca" || s.Timeout <= 0 {
				t.Fatal("unbounded orca command")
			}
			return []byte(tc.raw), nil
		}}
		ok, _ := r.TerminalAbsent(context.Background(), Ticket{Terminal: "term_old"})
		if ok != tc.want {
			t.Errorf("%s: %v", tc.raw, ok)
		}
	}
}
func TestNoActiveDispatchChecksEveryPage(t *testing.T) {
	oc := &fakeOrca{pages: []orca.Receipt{
		receipt(`{"workers":[{"dispatchStatus":"completed"}],"scope":{"run":"run_one"},"page":{"hasMore":true,"nextCursor":"two"}}`),
		receipt(`{"workers":[{"dispatchStatus":"dispatched"}],"scope":{"run":"run_one"},"page":{"hasMore":false}}`),
	}}
	r := &Runtime{Orca: oc}
	ok, err := r.NoActiveDispatch(context.Background(), Ticket{Run: "run_one"})
	if err != nil || ok || oc.page != 2 {
		t.Fatalf("absent=%v err=%v pages=%d", ok, err, oc.page)
	}
}

func TestLaunchMergingArgumentOrder(t *testing.T) {
	o := &fakeOrca{}
	comments := evidenceComments("阶段: 写 correction\n归属: 执行器 term=- run=- attempt=0", merge.HandoffPrefix+`{"queue_id":"q1"}`, merge.ProgressPrefix+`{"queue_id":"q1","phase":"correction"}`)
	r := &Runtime{Clients: map[string]issues.Store{"/repo": &fakeIssues{issueComments: comments}}, Orca: o, DriverPath: "/driver path/bin"}
	ticket := fromEvidence(t, Ticket{Identifier: "ENG-1", Repo: "/repo"}, comments)
	if err := r.Launch(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(o.launches) != 1 || !strings.HasPrefix(o.launches[0].Command, "exec '/driver path/bin' exec --merging 'ENG-1'") {
		t.Fatalf("wrong launch: %+v", o.launches)
	}
}

func (f *fakeOrca) WorkerRelease(context.Context, string, string) (orca.Receipt, error) {
	return orca.Receipt{}, nil
}
func (f *fakeOrca) WorktreeSet(_ context.Context, card orca.BoardCard) (orca.Receipt, error) {
	f.cards = append(f.cards, card)
	return orca.Receipt{}, nil
}

func TestLaunchOrdinaryCorrectionKeepsRecoveryRoute(t *testing.T) {
	o := &fakeOrca{}
	comments := evidenceComments("阶段: 写 correction\n归属: 执行器 term=- run=- attempt=0")
	r := &Runtime{Clients: map[string]issues.Store{"/repo": &fakeIssues{issueComments: comments}}, Orca: o, DriverPath: "/driver"}
	if err := r.Launch(context.Background(), fromEvidence(t, Ticket{Identifier: "ENG-1", Repo: "/repo"}, comments)); err != nil {
		t.Fatal(err)
	}
	if len(o.launches) != 1 || !strings.HasPrefix(o.launches[0].Command, "exec '/driver' exec 'ENG-1'") {
		t.Fatalf("ordinary correction wrongly requires merge evidence: %+v", o.launches)
	}
}
