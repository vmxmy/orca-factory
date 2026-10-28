package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	r := &Runtime{Clients: map[string]issues.Store{"/repo": l}, Orca: o, DriverPath: "/bin/driver"}
	ticket := Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo"}
	if err := r.Apply(context.Background(), ticket, Action{Kind: Resolve, Phase: machine.PhaseResolver}); err != nil {
		t.Fatal(err)
	}
	ticket.Owner, ticket.Phase = machine.OwnerResolver, machine.PhaseResolver
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
		h := validHandoffForResolver()
		if !barrier {
			h.TicketID = "another"
		}
		body, _ := json.Marshal(resolver.Resolution{Action: "merge", Reason: "reviewed", Handoff: &h})
		record, _ := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "result", Result: string(body)})
		bodies := []string{"阶段: Agent处置\n归属: 处置Agent\n" + resolver.Prefix + string(record)}
		if barrier {
			progress, _ := json.Marshal(merge.Progress{Phase: "merged", Deployment: &merge.DeploymentProgress{State: "paused"}})
			bodies = append(bodies, merge.ProgressPrefix+string(progress))
		}
		comments := evidenceComments(bodies...)
		l := &fakeIssues{issueComments: comments}
		r := &Runtime{Clients: map[string]issues.Store{"/repo": l}, Repositories: map[string]config.Repository{"/repo": {Env: map[string]string{"AGENT_GH_REPO": "o/r"}}}}
		ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo"}, comments)
		if barrier != DeploymentBarrier(ticket) {
			t.Fatalf("barrier fixture: %+v", ticket)
		}
		if err := r.applyResolution(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		if len(l.comments) != 1 || strings.Contains(l.comments[0], "归属: 合并通道") {
			t.Fatalf("unsafe result applied %+v", l.comments)
		}
	}
}

// The lane counts a conflict once; the resolver merging main to fix it is not
// a second round, so the new queue must not leave the lane exhausted.
func TestResolverRequeueKeepsLaneCatchupRounds(t *testing.T) {
	f := newMergeFixture(t)
	var h merge.Handoff
	if err := json.Unmarshal([]byte(strings.Split(f.tracker.rows[0].Body, merge.HandoffPrefix)[1]), &h); err != nil {
		t.Fatal(err)
	}
	conflict, _ := json.Marshal(merge.Progress{Version: 1, QueueID: h.QueueID, TicketID: h.TicketID, Repository: h.Repository, PR: h.PR, Head: h.Head, Main: f.main, Phase: "correction", Corrections: 1, CatchupRounds: 1, Attempt: 1, Reason: "return to scheduler to start a new executor for conflict"})
	h.QueueID, h.CatchupRounds = "q2", 2
	proposal, _ := json.Marshal(resolver.Resolution{Action: "merge", Reason: "conflict resolved", Handoff: &h})
	record, _ := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "result", Outcome: "success", Result: string(proposal)})
	enqueued := f.tracker.rows[0].CreatedAt
	f.tracker.rows = append(f.tracker.rows,
		task.Comment{ID: "conflict", CreatedAt: enqueued.Add(time.Nanosecond), Body: "阶段: Agent处置\n归属: 处置Agent\n" + merge.ProgressPrefix + string(conflict)},
		task.Comment{ID: "result", CreatedAt: enqueued.Add(2 * time.Nanosecond), Body: "阶段: Agent处置\n归属: 处置Agent term=controller\n" + resolver.Prefix + string(record)})
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
	for i := 0; i < 3 && f.tracker.issue.State.Name != "Done"; i++ {
		_ = s.Tick(context.Background())
	}
	if f.puts != 1 || f.tracker.issue.State.Name != "Done" || !strings.Contains(strings.Join(f.tracker.bodies, "\n"), `"catchup_rounds":1,"enqueued_at"`) {
		t.Fatalf("resolver requeue did not keep one catch-up round: puts=%d state=%s bodies=%q", f.puts, f.tracker.issue.State.Name, f.tracker.bodies)
	}
}

// ZY-255, 2026-10-28: the lane counted one conflict and resolver-1 proposed two.
// gate_spec, gate_output, review_output, the role spec and the reason are redacted.
func TestRecordedResolverRequeueCatchupRounds(t *testing.T) {
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded(t, "ledger-comments-resolver-requeue.json"), &recording); err != nil {
		t.Fatal(err)
	}
	var lane Ticket
	readMergeHandoff(&lane, recording.Comments[:2])
	var record resolver.Record
	if err := json.Unmarshal([]byte(strings.TrimPrefix(recording.Comments[2].Body, resolver.Prefix)), &record); err != nil {
		t.Fatal(err)
	}
	var proposal resolver.Resolution
	if err := json.Unmarshal([]byte(record.Result), &proposal); err != nil || proposal.Handoff == nil {
		t.Fatalf("recorded proposal: %v", err)
	}
	if rounds, err := laneCatchupRounds(lane); proposal.Handoff.CatchupRounds != 2 || err != nil || rounds != 1 {
		t.Fatalf("resolver proposed %d; lane rounds = %d, %v; want 1", proposal.Handoff.CatchupRounds, rounds, err)
	}
	other := *lane.Progress
	other.Head = proposal.Handoff.Head
	lane.Progress = &other
	if rounds, err := laneCatchupRounds(lane); err == nil {
		t.Fatalf("progress for head %s counted as the lane's %d rounds", other.Head, rounds)
	}
}

func TestNewEscalationStartsAnotherResolverAttempt(t *testing.T) {
	old := resolver.Record{Version: 1, Attempt: 2, Phase: "applied", Result: "old"}
	raw, _ := json.Marshal(old)
	comments := []task.Comment{{Body: resolver.Prefix + string(raw), CreatedAt: time.Unix(1, 0)}, {Body: "Agent处置: CI broke\n阶段: Agent处置\n归属: 处置Agent", CreatedAt: time.Unix(2, 0)}}
	p, err := readResolution(comments)
	if err != nil || p == nil || p.Phase != "waiting" || p.Attempt != 2 || !p.RetryAt.IsZero() {
		t.Fatalf("old resolution prevents new dispatch: %+v %v", p, err)
	}
}
func TestFailedTicketWithoutOwnershipIsAdmittedToResolver(t *testing.T) {
	l := &fakeIssues{issues: []task.Issue{{ID: "id", Identifier: "ENG-1", Project: task.NamedID{ID: "project"}, State: task.NamedID{Name: "In Progress"}, Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "auto-failed"}}}}}}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": l}}
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
	body, _ := json.Marshal(resolver.Resolution{Action: "retry-smoke", Reason: "checked", Queue: "wrong", SHA: strings.Repeat("a", 40)})
	record, _ := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "result", Result: string(body)})
	comments := evidenceComments(resolver.Prefix + string(record))
	l := &fakeIssues{issueComments: comments}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": l}}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo"}, comments)
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
func (o *resolverStartupProbe) TaskListing(context.Context, string) (orca.TaskListing, error) {
	return orca.TaskListing{}, nil
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
	comments := evidenceComments("阶段: Agent处置\n归属: 处置Agent term=- run=source-run attempt=0")
	l := &fakeIssues{issueComments: comments}
	r := &Runtime{Clients: map[string]issues.Store{repo: l}, Repositories: map[string]config.Repository{repo: {Path: repo}}, Orca: &resolverStartupProbe{}}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "ENG-1", Repo: repo}, comments)
	if err := r.ResolveTicket(context.Background(), ticket, "new-controller", nil); err == nil {
		t.Fatal("expected interrupted start")
	}
	if len(l.comments) == 0 || !strings.Contains(l.comments[0], "run=source-run") {
		t.Fatalf("source identity erased before checkpoint: %+v", l.comments)
	}
}

func TestResolverEvidenceRepository(t *testing.T) {
	comments := evidenceComments("阶段: Agent处置\n归属: 处置Agent", "Preserve this evidence")
	ticket, record := resolverRecord(t, comments)
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

// A long ticket history must not push the persisted resolver record past the
// ledger cell limit: older and empty comments give way to the newest evidence.
func TestResolverEvidenceBoundedByNewestComments(t *testing.T) {
	bodies := []string{"阶段: Agent处置\n归属: 处置Agent", "Oldest evidence"}
	for range 60 {
		bodies = append(bodies, strings.Repeat("历史", 1000), "")
	}
	bodies = append(bodies, "Newest evidence")
	_, record := resolverRecord(t, evidenceComments(bodies...))
	raw, _ := json.Marshal(record)
	if n := utf8.RuneCount(raw); n > 60000 {
		t.Fatalf("resolver record has %d characters", n)
	}
	if !strings.Contains(record.Spec, "Newest evidence") || strings.Contains(record.Spec, "Oldest evidence") || strings.Contains(record.Spec, `"Body":""`) {
		t.Fatalf("evidence kept the wrong comments: %.300s", record.Spec)
	}
	if !strings.Contains(record.Spec, `"OmittedComments":`) {
		t.Fatal("omitted comments not reported")
	}
}

// resolverRecord starts a resolver over comments and returns the first
// resolver record it persisted.
func resolverRecord(t *testing.T, comments []task.Comment) (Ticket, resolver.Record) {
	t.Helper()
	repo := t.TempDir()
	roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
	if err := os.MkdirAll(roles, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve the ticket"), 0600); err != nil {
		t.Fatal(err)
	}
	l := &fakeIssues{issueComments: comments}
	r := &Runtime{Clients: map[string]issues.Store{repo: l}, Repositories: map[string]config.Repository{
		repo:     {Path: repo, Env: map[string]string{"AGENT_GH_REPO": "owner/code-repo"}},
		"/other": {Env: map[string]string{"AGENT_GH_REPO": "owner/other-repo"}},
	}, Orca: &resolverStartupProbe{}}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "ENG-1", Repo: repo}, comments)
	if err := r.ResolveTicket(context.Background(), ticket, "controller", nil); err == nil || err.Error() != "stop after durable registration" {
		t.Fatalf("expected interrupted start, got %v", err)
	}
	for _, body := range l.comments {
		if strings.HasPrefix(body, resolver.Prefix) {
			var record resolver.Record
			if err := json.Unmarshal([]byte(strings.TrimPrefix(body, resolver.Prefix)), &record); err != nil {
				t.Fatal(err)
			}
			return ticket, record
		}
	}
	t.Fatal("no resolver record persisted")
	return Ticket{}, resolver.Record{}
}

// A record that returns to an earlier value (a refused dispatch clearing its
// run) must still become the latest record instead of matching the old copy.
func TestResolutionSaveRevisitsEarlierState(t *testing.T) {
	l := &fakeIssues{}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": l}}
	store := resolutionStore{r, Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo"}}
	pending := resolver.Record{Version: 1, Attempt: 2, Phase: "pending", Creator: "controller"}
	withRun := pending
	withRun.Run = "run_1"
	for _, p := range []resolver.Record{pending, withRun, pending, pending} {
		if err := store.Save(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Load(context.Background())
	if err != nil || got == nil || got.Run != "" {
		t.Fatalf("latest record %+v err=%v, want the cleared run", got, err)
	}
	if len(l.comments) != 3 {
		t.Fatalf("writes=%d, want 3 (a repeated latest state is not rewritten)", len(l.comments))
	}
}

// A save whose create call errors but did land is confirmed even when another
// writer appended a newer record afterwards; retrying would bury that record.
func TestResolutionSaveConfirmsLandedWriteBehindNewerRecord(t *testing.T) {
	l := &racingIssues{}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": l}}
	store := resolutionStore{r, Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo"}}
	if err := store.Save(context.Background(), resolver.Record{Version: 1, Attempt: 1, Phase: "pending", Creator: "mine"}); err != nil {
		t.Fatalf("landed write reported unconfirmed: %v", err)
	}
	if len(l.comments) != 2 {
		t.Fatalf("writes=%d, want only ours and the other writer's", len(l.comments))
	}
}

// racingIssues stores a create, then lets another writer append a newer
// record and reports the create as failed.
type racingIssues struct{ fakeIssues }

func (f *racingIssues) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	if _, err := f.fakeIssues.CreateComment(ctx, id, body); err != nil {
		return task.Comment{}, err
	}
	other, _ := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "pending", Creator: "other"})
	_, _ = f.fakeIssues.CreateComment(ctx, id, resolver.Prefix+string(other))
	return task.Comment{}, errors.New("write uncertain")
}

// OF-162: a round that never dispatched neither fails nor succeeds, so its
// waiting record keeps the consecutive failure count unchanged.
func TestResolverEmptyRunIsNotUncertainKeepsFailures(t *testing.T) {
	var comments []task.Comment
	for i, r := range []resolver.Record{
		{Version: 1, Attempt: 1, Phase: "waiting", Outcome: "failed"},
		{Version: 1, Attempt: 2, Phase: "waiting", Outcome: "undispatched"},
		{Version: 1, Attempt: 3, Phase: "waiting", Outcome: "failed"},
		{Version: 1, Attempt: 4, Phase: "waiting", Outcome: "undispatched"},
	} {
		raw, _ := json.Marshal(r)
		comments = append(comments, task.Comment{Body: resolver.Prefix + string(raw), CreatedAt: time.Unix(int64(i+1), 0)})
	}
	p, err := readResolution(comments)
	if err != nil || p == nil || p.Failures != 2 {
		t.Fatalf("undispatched round changed failures: %+v %v", p, err)
	}
}
