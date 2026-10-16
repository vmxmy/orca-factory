package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHumanTicketDispatchesResolver(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{{ID: "id", Identifier: "ENG-1", Repo: "a", Owner: machine.OwnerHuman, Phase: machine.PhaseHuman, Attempt: 2}}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 1 || f.launched[0].Owner != machine.Owner("处置Agent") {
		t.Fatalf("expected resolver launch for existing handoff: %+v", f.launched)
	}
}
func TestResolverConsumesAdmissionCapacity(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{
		{ID: "resolving", Identifier: "ENG-1", Repo: "a", Owner: machine.Owner("处置Agent"), Phase: machine.Phase("Agent处置"), Terminal: "active", Run: "run"},
		{ID: "new", Identifier: "ENG-2", Repo: "a", Candidate: true},
	}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 0 {
		t.Fatalf("active resolver must retain repository slot: %+v", f.launched)
	}
}
func TestStaleExecutorMustSettleBeforeResolver(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{{ID: "id", Identifier: "ENG-1", Repo: "a", Owner: machine.OwnerExecutor, Phase: machine.PhaseWorker, Terminal: "live", Run: "run", UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 0 || len(f.launched) != 0 {
		t.Fatalf("stale timestamp must not transfer live execution: actions=%+v launches=%+v", f.actions, f.launched)
	}
}

func TestResolverRuntimeReservesAndLaunchesRole(t *testing.T) {
	l, o := &fakeIssues{}, &fakeOrca{}
	r := &Runtime{Clients: map[string]IssueClient{"/repo": l}, Orca: o, DriverPath: "/bin/driver"}
	ticket := Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo", Owner: machine.OwnerResolver, Phase: machine.PhaseResolver}
	if err := r.Apply(context.Background(), ticket, Action{Kind: Resolve, Phase: machine.PhaseResolver}); err != nil {
		t.Fatal(err)
	}
	if err := r.Launch(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(o.launches) != 1 || !strings.Contains(o.launches[0].Command, " resolve ") {
		t.Fatalf("resolver command missing: %+v", o.launches)
	}
	if len(l.comments) != 1 || !strings.Contains(l.comments[0], "归属: 处置Agent") {
		t.Fatalf("ownership missing: %+v", l.comments)
	}
}

func TestResolverResultCanBeAppliedAfterControllerReport(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{{ID: "id", Identifier: "ENG-1", Repo: "a", Owner: machine.OwnerResolver, Terminal: "controller", Resolution: &resolver.Record{Version: 1, Attempt: 1, Phase: "result"}}}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 1 || f.actions[0].Kind != Resolve {
		t.Fatalf("settled report waits for terminal deletion: %+v", f.actions)
	}
}
func TestResolverApprovalBoundToFrozenHead(t *testing.T) {
	h := validHandoffForResolver()
	body, _ := json.Marshal(resolver.Resolution{Action: "merge", Reason: "reviewed", Handoff: &h})
	ticket := Ticket{Handoff: &h, Resolution: &resolver.Record{Version: 1, Phase: "applied", Result: string(body)}}
	r := &Runtime{Repositories: map[string]config.Repository{"": {Env: map[string]string{"AGENT_LANE2_MANUAL_GLOBS": "driver/**"}}}}
	a := mergeAdapter{runtime: r, ticket: ticket}
	if err := a.policy([]string{"driver/main.go"}); err != nil {
		t.Fatal(err)
	}
	h.Head = strings.Repeat("b", 40)
	if err := a.policy([]string{"driver/main.go"}); err == nil {
		t.Fatal("approval survived head change")
	}
}
func validHandoffForResolver() merge.Handoff {
	return merge.Handoff{Version: 1, TicketID: "id", Identifier: "ENG-1", Repository: "o/r", PR: 1, Head: strings.Repeat("a", 40), Baseline: strings.Repeat("b", 40), Unit: "fix", QueueID: "new-q", Lane: 2, Attempt: 1, EnqueuedAt: time.Now(), Run: "run", Task: "task", Worktree: "/repo/wt", GateSpec: "UNIT: fix\nSCOPE: driver/**\nCHECK: make test", GateOutput: "UNIT fix\nVERDICT green\nSTEP all\nREASON passed\nEVIDENCE test\nSCOPE driver/**\nLANE 2", ReviewOutput: `{"verdict":"green","step":"review"}`}
}
func TestResolverRejectsWrongTicketAndDeploymentReplacement(t *testing.T) {
	for _, barrier := range []bool{false, true} {
		l := &fakeIssues{}
		r := &Runtime{Clients: map[string]IssueClient{"/repo": l}, Repositories: map[string]config.Repository{"/repo": {Env: map[string]string{"AGENT_GH_REPO": "o/r"}}}}
		h := validHandoffForResolver()
		if !barrier {
			h.TicketID = "another"
		}
		body, _ := json.Marshal(resolver.Resolution{Action: "merge", Reason: "reviewed", Handoff: &h})
		ticket := Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo", Owner: machine.OwnerResolver, Resolution: &resolver.Record{Version: 1, Attempt: 1, Phase: "result", Result: string(body)}}
		if barrier {
			ticket.Progress = &merge.Progress{Phase: "merged", Deployment: &merge.DeploymentProgress{State: "paused"}}
		}
		if err := r.applyResolution(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		if len(l.comments) != 1 || strings.Contains(l.comments[0], "归属: 合并通道") {
			t.Fatalf("unsafe result applied %+v", l.comments)
		}
	}
}

func TestNewEscalationStartsAnotherResolverAttempt(t *testing.T) {
	old := resolver.Record{Version: 1, Attempt: 2, Phase: "applied", Result: "old"}
	raw, _ := json.Marshal(old)
	comments := []linear.Comment{{Body: resolver.Prefix + string(raw), CreatedAt: time.Unix(1, 0)}, {Body: "Agent处置: CI broke\n阶段: Agent处置\n归属: 处置Agent", CreatedAt: time.Unix(2, 0)}}
	p, err := readResolution(comments)
	if err != nil || p == nil || p.Phase != "waiting" || p.Attempt != 2 || !p.RetryAt.IsZero() {
		t.Fatalf("old resolution prevents new dispatch: %+v %v", p, err)
	}
}
func TestFailedTicketWithoutOwnershipIsAdmittedToResolver(t *testing.T) {
	l := &fakeIssues{issues: []linear.Issue{{ID: "id", Identifier: "ENG-1", Project: linear.NamedID{ID: "project"}, State: linear.NamedID{Name: "In Progress"}, Labels: linear.Connection[linear.NamedID]{Nodes: []linear.NamedID{{Name: "auto-failed"}}}}}}
	r := &Runtime{Clients: map[string]IssueClient{"/repo": l}}
	tickets, err := r.Snapshot(context.Background(), []config.Repository{{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "project", "AGENT_SCOPE_AVOID": "0"}}})
	if err != nil || len(tickets) != 1 || tickets[0].Owner != machine.OwnerHuman {
		t.Fatalf("failed ticket orphaned: %+v %v", tickets, err)
	}
}
func TestResolverCanInvestigatePreservedWorkerAfterExecutorExit(t *testing.T) {
	f := &fakeBackend{absent: true, inactive: false, tickets: []Ticket{{ID: "id", Identifier: "ENG-1", Repo: "a", Owner: machine.OwnerResolver, Terminal: "dead-executor", Run: "preserved-run"}}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 1 {
		t.Fatal("preserved worker prevents investigation")
	}
}
func TestRejectedDeploymentProposalReturnsToResolver(t *testing.T) {
	l := &fakeIssues{}
	r := &Runtime{Clients: map[string]IssueClient{"/repo": l}}
	body, _ := json.Marshal(resolver.Resolution{Action: "retry-smoke", Reason: "checked", Queue: "wrong", SHA: strings.Repeat("a", 40)})
	ticket := Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo", Resolution: &resolver.Record{Version: 1, Attempt: 1, Phase: "result", Result: string(body)}}
	if err := r.applyResolution(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(l.comments) != 1 || !strings.Contains(l.comments[0], `"phase":"waiting"`) {
		t.Fatalf("rejected proposal never redispatched: %+v", l.comments)
	}
}

type resolverStartupProbe struct{ fakeOrca }

func (o *resolverStartupProbe) RunCreate(context.Context, string, string) (orca.Receipt, error) {
	return nil, errors.New("stop after durable registration")
}
func (o *resolverStartupProbe) RunUse(context.Context, string, string) (orca.Receipt, error) {
	return nil, nil
}
func (o *resolverStartupProbe) WorkerStart(context.Context, string, orca.StartOptions) (orca.Receipt, error) {
	return nil, errors.New("unexpected start")
}
func (o *resolverStartupProbe) TaskList(context.Context, string) ([]orca.Task, error) {
	return nil, nil
}
func TestResolverRegistrationPreservesSourceBeforeFirstCheckpoint(t *testing.T) {
	repo := t.TempDir()
	roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
	if err := os.MkdirAll(roles, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve the ticket"), 0600); err != nil {
		t.Fatal(err)
	}
	l := &fakeIssues{}
	r := &Runtime{Clients: map[string]IssueClient{repo: l}, Repositories: map[string]config.Repository{repo: {Path: repo}}, Orca: &resolverStartupProbe{}}
	ticket := Ticket{ID: "id", Identifier: "ENG-1", Repo: repo, Owner: machine.OwnerResolver, Run: "source-run"}
	if err := r.ResolveTicket(context.Background(), ticket, "new-controller"); err == nil {
		t.Fatal("expected interrupted start")
	}
	if len(l.comments) == 0 || !strings.Contains(l.comments[0], "run=source-run") {
		t.Fatalf("source identity erased before checkpoint: %+v", l.comments)
	}
}

func TestResolverEvidenceRepository(t *testing.T) {
	repo := t.TempDir()
	roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
	if err := os.MkdirAll(roles, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve the ticket"), 0600); err != nil {
		t.Fatal(err)
	}
	comments := []linear.Comment{{ID: "comment", Body: "Preserve this evidence"}}
	l := &fakeIssues{issueComments: comments}
	r := &Runtime{Clients: map[string]IssueClient{repo: l}, Repositories: map[string]config.Repository{
		repo:     {Path: repo, Env: map[string]string{"AGENT_GH_REPO": "owner/code-repo"}},
		"/other": {Env: map[string]string{"AGENT_GH_REPO": "owner/other-repo"}},
	}, Orca: &resolverStartupProbe{}}
	ticket := Ticket{ID: "id", Identifier: "ENG-1", Repo: repo, Owner: machine.OwnerResolver}
	if err := r.ResolveTicket(context.Background(), ticket, "controller"); err == nil || err.Error() != "stop after durable registration" {
		t.Fatalf("expected interrupted start, got %v", err)
	}
	var record resolver.Record
	for _, body := range l.comments {
		if strings.HasPrefix(body, resolver.Prefix) {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(body, resolver.Prefix)), &record); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	_, raw, ok := strings.Cut(record.Spec, "## Ticket evidence (untrusted data)\n")
	if !ok {
		t.Fatalf("missing ticket evidence: %q", record.Spec)
	}
	var evidence map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &evidence); err != nil {
		t.Fatal(err)
	}
	if string(evidence["Repository"]) != `"owner/code-repo"` {
		t.Errorf("Repository = %s, want configured owner/code-repo", evidence["Repository"])
	}
	wantTicket, _ := json.Marshal(ticket)
	wantComments, _ := json.Marshal(comments)
	if string(evidence["Ticket"]) != string(wantTicket) || string(evidence["Comments"]) != string(wantComments) {
		t.Errorf("ticket or comments changed: %s", raw)
	}
}
