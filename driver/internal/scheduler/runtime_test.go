package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type fakeLinear struct {
	updates       int
	comments      []string
	issueComments []linear.Comment
	issues        []linear.Issue
	projects      []string
}

func (f *fakeLinear) Snapshot(_ context.Context, p []string, _ time.Time) ([]linear.Issue, error) {
	f.projects = p
	return f.issues, nil
}
func (f *fakeLinear) ListComments(context.Context, string) ([]linear.Comment, error) {
	return f.issueComments, nil
}
func (f *fakeLinear) UpdateIssue(context.Context, string, string, []string, []string) (linear.Issue, error) {
	f.updates++
	return linear.Issue{ID: "id", Labels: linear.Connection[linear.NamedID]{Nodes: []linear.NamedID{{Name: "auto-impl"}}}}, nil
}
func (f *fakeLinear) CreateComment(_ context.Context, _ string, b string) (linear.Comment, error) {
	f.comments = append(f.comments, b)
	return linear.Comment{ID: "comment"}, nil
}

type fakeOrca struct {
	automations orca.Receipt
	pages       []orca.Receipt
	page        int
	launches    []orca.TerminalOptions
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
func receipt(s string) orca.Receipt { var r orca.Receipt; _ = json.Unmarshal([]byte(s), &r); return r }
func TestClaimOneUpdateOneCommentAndSafeLaunch(t *testing.T) {
	lc := &fakeLinear{}
	oc := &fakeOrca{}
	r := &Runtime{Clients: map[string]LinearClient{"/repo": lc}, Orca: oc, DriverPath: "/opt/driver with space"}
	ticket := Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo", Attempt: 1}
	if err := r.Apply(context.Background(), ticket, Action{Kind: Claim, Attempt: 1, Phase: machine.PhaseWorker}); err != nil {
		t.Fatal(err)
	}
	if lc.updates != 1 || len(lc.comments) != 1 || !strings.Contains(lc.comments[0], "scheduler-attempt=1") {
		t.Fatalf("updates=%d comments=%v", lc.updates, lc.comments)
	}
	if err := r.Launch(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(oc.launches) != 1 || oc.launches[0].Command != "exec '/opt/driver with space' exec 'ENG-1'" {
		t.Fatalf("launch=%v", oc.launches)
	}
	ticket.Identifier = "ENG-1; touch secret"
	if err := r.Launch(context.Background(), ticket); err == nil {
		t.Fatal("accepted invalid ticket")
	}
}
func TestResumeOnlyWritesOwnership(t *testing.T) {
	lc := &fakeLinear{}
	r := &Runtime{Clients: map[string]LinearClient{"repo": lc}}
	if err := r.Apply(context.Background(), Ticket{ID: "id", Repo: "repo"}, Action{Kind: Resume, Attempt: 2, Phase: machine.PhaseGate}); err != nil {
		t.Fatal(err)
	}
	if lc.updates != 0 || len(lc.comments) != 1 {
		t.Fatal("resume reclaimed ticket")
	}
}
func TestOwnershipPreservesSchedulerAttempt(t *testing.T) {
	comments := []linear.Comment{
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
	lc := &fakeLinear{issues: []linear.Issue{{ID: "id", Identifier: "ENG-1", Priority: 2, CreatedAt: time.Unix(1, 0), Project: linear.NamedID{ID: "p2"}, State: linear.NamedID{Name: "Todo"}, Labels: linear.Connection[linear.NamedID]{Nodes: []linear.NamedID{{Name: "ready-for-agent"}}}}}}
	r := &Runtime{Reader: lc, Clients: map[string]LinearClient{"a": lc, "b": lc}}
	tickets, err := r.Snapshot(context.Background(), []config.Repository{{Path: "a", Env: map[string]string{"AGENT_LINEAR_PROJECT": "p1"}}, {Path: "b", Env: map[string]string{"AGENT_LINEAR_PROJECT": "p2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lc.projects, []string{"p1", "p2"}) || len(tickets) != 1 || tickets[0].Repo != "b" || !tickets[0].Candidate || tickets[0].Priority != 2 {
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
