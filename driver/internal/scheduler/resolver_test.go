package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/executor"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
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

type resolverMergeIssues struct{ *mergeIssues }

func (m resolverMergeIssues) Description(context.Context, string) (string, error) {
	return "要做: implement feature\nSCOPE: feature\nCHECK: true", nil
}

type resolverFaceIssues struct {
	*mergeIssues
	face string
}

func (m resolverFaceIssues) Description(context.Context, string) (string, error) {
	return m.face, nil
}

func resolverMergeProposal(t *testing.T, f *mergeFixture, edit func(*merge.Handoff)) {
	t.Helper()
	f.runtime.Clients[f.repos[0].Path] = resolverMergeIssues{f.tracker}
	resolverFrozenProposal(t, f, "UNIT: executor\nSCOPE: feature\nCHECK: true\nCHECK_IS_NEW_TEST: no\nCHECK_TEST_FILES: \nCHECK_ASSERTION: \nLANE: 2\n", edit)
}

func resolverFrozenProposal(t *testing.T, f *mergeFixture, spec string, edit func(*merge.Handoff)) {
	t.Helper()
	rewriteHandoff(t, f, func(h map[string]any) { h["gate_spec"] = spec })
	var h merge.Handoff
	if err := json.Unmarshal([]byte(strings.Split(f.tracker.rows[0].Body, merge.HandoffPrefix)[1]), &h); err != nil {
		t.Fatal(err)
	}
	frozen, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	f.tracker.rows[0].Body = "阶段: 排队合并\n归属: 合并通道\n" + merge.HandoffPrefix + string(frozen)
	if edit != nil {
		edit(&h)
	}
	proposal, err := json.Marshal(resolver.Resolution{Action: "merge", Reason: "reviewed", Handoff: &h})
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "result", Outcome: "succeeded", Result: string(proposal)})
	if err != nil {
		t.Fatal(err)
	}
	f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "result", CreatedAt: time.Now(), Body: "阶段: Agent处置\n归属: 处置Agent\n" + resolver.Prefix + string(record)})
}

func TestResolverHandoffIsGatedByDriverBeforeSquash(t *testing.T) {
	for _, verdict := range []string{"green", "red"} {
		t.Run(verdict, func(t *testing.T) {
			f := newMergeFixture(t)
			resolverMergeProposal(t, f, nil)
			closeMergeGates(t, f.runtime)
			started, release := make(chan struct{}, 1), make(chan struct{})
			f.runtime.DriverPath = "/test/driver"
			f.runtime.Run = func(ctx context.Context, spec runx.Spec) ([]byte, error) {
				if spec.Path != "bash" || len(spec.Args) != 3 {
					return runx.Run(ctx, spec)
				}
				if !slices.Contains(spec.Env, "GATE_BASE="+f.main) || !slices.Contains(spec.Env, "GATE_PROJECT_SRC="+f.main) {
					t.Error("gate must use the pinned main")
				}
				parents := gitMergeTest(t, spec.Args[1], "show", "-s", "--format=%P", "HEAD")
				if parents != f.head+" "+f.main {
					t.Errorf("gate must validate the merged result: parents=%s", parents)
				}
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				out := []byte("UNIT executor\nVERDICT " + verdict + "\nSTEP all\nREASON checked\nEVIDENCE test\nSCOPE feature\nLANE 1")
				if verdict == "red" {
					return out, gateExit{}
				}
				return out, nil
			}
			s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
			for range 2 {
				if err := tickAlone(t, s); err != nil {
					t.Fatal(err)
				}
			}
			if f.puts != 0 {
				t.Fatalf("resolver evidence squashed before driver gate: puts=%d", f.puts)
			}
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("driver gate did not start for an up-to-date resolver head")
			}
			close(release)
			awaitMergeGate(t, f.runtime, f)
			if err := tickAlone(t, s); err != nil {
				t.Fatal(err)
			}
			if verdict == "green" {
				if f.puts != 1 || f.tracker.issue.State.Name != "Done" {
					t.Fatalf("green gate did not permit squash: puts=%d state=%s", f.puts, f.tracker.issue.State.Name)
				}
			} else {
				progress := mergeProgress(t, f.tracker.bodies)
				if f.puts != 0 || len(progress) == 0 || progress[len(progress)-1].Phase != "correction" {
					t.Fatalf("red driver gate did not prevent squash: puts=%d progress=%+v", f.puts, progress)
				}
			}
		})
	}
}

func TestResolverCannotReplaceGateSpec(t *testing.T) {
	for _, field := range []string{"CHECK", "SCOPE", "CHECK_IS_NEW_TEST", "CHECK_TEST_FILES", "CHECK_ASSERTION", "LANE"} {
		t.Run(field, func(t *testing.T) {
			f := newMergeFixture(t)
			resolverMergeProposal(t, f, func(h *merge.Handoff) {
				h.QueueID = "replacement"
				if field == "SCOPE" {
					h.GateOutput = strings.Replace(h.GateOutput, "SCOPE feature", "SCOPE changed", 1)
				}
				lines := strings.Split(h.GateSpec, "\n")
				for i, line := range lines {
					if strings.HasPrefix(line, field+":") {
						lines[i] = field + ": changed"
					}
				}
				h.GateSpec = strings.Join(lines, "\n")
			})
			s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			bodies := strings.Join(f.tracker.bodies, "\n")
			if !strings.Contains(bodies, "gate_spec") || !strings.Contains(bodies, `"phase":"waiting"`) || strings.Contains(bodies, "归属: 合并通道") || f.puts != 0 {
				t.Fatalf("replacement gate spec accepted: puts=%d bodies=%s", f.puts, bodies)
			}
		})
	}
}

// gate.sh reads only `KEY: value` lines that start at column 0 and compares
// values exactly, so a spec line differing only in whitespace would pass a
// trimmed comparison yet change what the gate executes.
func TestResolverGateSpecLinesMatchDriverRendering(t *testing.T) {
	face := "要做: implement feature\nSCOPE: feature\nCHECK: true\nCHECK_IS_NEW_TEST: yes\nCHECK_TEST_FILES: feature_test.go\nCHECK_ASSERTION: feature_test.go"
	spec := "UNIT: executor\nSCOPE: feature\nCHECK: true\nCHECK_IS_NEW_TEST: yes\nCHECK_TEST_FILES: feature_test.go\nCHECK_ASSERTION: feature_test.go\nLANE: 2\n"
	for name, line := range map[string]string{"leading space": " CHECK_IS_NEW_TEST: yes", "trailing space": "CHECK_IS_NEW_TEST: yes ", "no separator space": "CHECK_IS_NEW_TEST:yes"} {
		t.Run(name, func(t *testing.T) {
			f := newMergeFixture(t)
			f.runtime.Clients[f.repos[0].Path] = resolverFaceIssues{f.tracker, face}
			resolverFrozenProposal(t, f, spec, func(h *merge.Handoff) {
				h.QueueID = "replacement"
				h.GateSpec = strings.Replace(h.GateSpec, "CHECK_IS_NEW_TEST: yes", line, 1)
			})
			s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			bodies := strings.Join(f.tracker.bodies, "\n")
			if !strings.Contains(bodies, "gate_spec differs") || strings.Contains(bodies, "归属: 合并通道") || f.puts != 0 {
				t.Fatalf("gate_spec line %q accepted: puts=%d bodies=%s", line, f.puts, bodies)
			}
		})
	}
}

// The recorded face keeps Markdown escapes in CHECK; the executor renders them
// verbatim (OF-377), so a resolver spec must too.
func TestRecordedResolverGateSpec(t *testing.T) {
	face, err := os.ReadFile("../../testdata/recorded/linear-description-escaped.md")
	if err != nil {
		t.Fatal(err)
	}
	check := `set -a; . scripts/agent-gates/agent-graph.env; set +a; \[\[ $AGENT_ROLE_REVIEWER_CMD == claude\\ -p\* \]\] && ! grep -q 'AGENT_ROLE_REVIEWER_CMD -"' core/COORDINATOR.md scripts/agent-gates/COORDINATOR.md && bin/orca-factory check . >/dev/null`
	scope := "core/COORDINATOR.md scripts/agent-gates/COORDINATOR.md scripts/agent-gates/agent-graph.env"
	spec := "UNIT: executor\nSCOPE: " + scope + "\nCHECK: " + check + "\nCHECK_IS_NEW_TEST: no\nCHECK_TEST_FILES: \nCHECK_ASSERTION: \nLANE: 2\n"
	for name, accepted := range map[string]bool{"rendered": true, "unescaped": false} {
		t.Run(name, func(t *testing.T) {
			f := newMergeFixture(t)
			closeMergeGates(t, f.runtime)
			f.runtime.Clients[f.repos[0].Path] = resolverFaceIssues{f.tracker, string(face)}
			resolverFrozenProposal(t, f, spec, func(h *merge.Handoff) {
				h.QueueID = "recorded"
				h.GateOutput = strings.Replace(h.GateOutput, "SCOPE feature", "SCOPE "+scope, 1)
				if !accepted {
					h.GateSpec = strings.Replace(h.GateSpec, `\[\[`, "[[", 1)
				}
			})
			s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			bodies := strings.Join(f.tracker.bodies, "\n")
			if strings.Contains(bodies, "gate_spec differs") == accepted || strings.Contains(bodies, `"queue_id":"recorded"`) != accepted {
				t.Fatalf("recorded face spec accepted=%v: bodies=%s", accepted, bodies)
			}
		})
	}
}

// A real resolver merge proposal (OF-572) carries the executor-rendered
// gate_spec for its ticket face; whitespace-only drift must still be refused.
func TestRecordedResolverMergeProposal(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/resolver-merge-proposal-OF-572.json")
	if err != nil {
		t.Fatal(err)
	}
	var sample struct {
		Ticket struct{ ID, Identifier, Title, Description string } `json:"ticket"`
		Result string                                              `json:"result"`
	}
	if err = json.Unmarshal(raw, &sample); err != nil {
		t.Fatal(err)
	}
	proposal, err := resolver.ParseResult(sample.Result)
	if err != nil || proposal.Action != "merge" || proposal.Handoff == nil {
		t.Fatalf("recorded proposal: action=%q err=%v", proposal.Action, err)
	}
	face, err := executor.ParseTicket(sample.Ticket.ID, sample.Ticket.Identifier, sample.Ticket.Title, sample.Ticket.Description)
	if err != nil {
		t.Fatal(err)
	}
	spec := proposal.Handoff.GateSpec
	if problem := resolverGateSpecProblem(spec, proposal.Handoff.Unit, face); problem != "" {
		t.Fatalf("recorded gate_spec refused: %s", problem)
	}
	for _, drift := range []string{strings.Replace(spec, "\nCHECK: ", "\n CHECK: ", 1), strings.Replace(spec, "driver/CONTRACTS.md\n", "driver/CONTRACTS.md \n", 1), strings.Replace(spec, "LANE: 2", "LANE:2", 1)} {
		if drift == spec {
			t.Fatal("drift did not change the recorded spec")
		}
		if resolverGateSpecProblem(drift, proposal.Handoff.Unit, face) == "" {
			t.Fatalf("whitespace drift accepted: %q", drift)
		}
	}
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
			handoff, _ := json.Marshal(h)
			progress := merge.Progress{
				Version: 1, QueueID: h.QueueID, TicketID: h.TicketID, Repository: h.Repository,
				PR: h.PR, Head: h.Head, Main: h.Baseline, Phase: "merged", MergeSHA: strings.Repeat("c", 40),
				Deployment: &merge.DeploymentProgress{Workflow: "deploy.yml", State: "pending"},
			}
			pending, _ := json.Marshal(progress)
			progress.Deployment.State = "paused"
			paused, _ := json.Marshal(progress)
			bodies = append(bodies, merge.HandoffPrefix+string(handoff), merge.ProgressPrefix+string(pending), merge.ProgressPrefix+string(paused))
		}
		comments := evidenceComments(bodies...)
		l := &fakeIssues{issueComments: comments}
		r := &Runtime{Clients: map[string]issues.Store{"/repo": l}, Repositories: map[string]config.Repository{"/repo": {Env: map[string]string{"AGENT_GH_REPO": "o/r"}}}}
		ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo"}, comments)
		if barrier != DeploymentBarrier(ticket) {
			t.Fatalf("barrier fixture: %+v", ticket)
		}
		if ticket.MergeProblem != "" {
			t.Fatalf("invalid barrier fixture: %s", ticket.MergeProblem)
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
	f.runtime.Clients[f.repos[0].Path] = resolverMergeIssues{f.tracker}
	closeMergeGates(t, f.runtime)
	var h merge.Handoff
	if err := json.Unmarshal([]byte(strings.Split(f.tracker.rows[0].Body, merge.HandoffPrefix)[1]), &h); err != nil {
		t.Fatal(err)
	}
	conflict, _ := json.Marshal(merge.Progress{Version: 1, QueueID: h.QueueID, TicketID: h.TicketID, Repository: h.Repository, PR: h.PR, Head: h.Head, Main: f.main, Phase: "correction", Corrections: 1, CatchupRounds: 1, Attempt: 1, Reason: "return to scheduler to start a new executor for conflict"})
	h.QueueID, h.CatchupRounds = "q2", 2
	h.GateSpec = strings.Replace(h.GateSpec, "LANE: 1", "LANE: 2", 1)
	proposal, _ := json.Marshal(resolver.Resolution{Action: "merge", Reason: "conflict resolved", Handoff: &h})
	record, _ := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "result", Outcome: "success", Result: string(proposal)})
	enqueued := f.tracker.rows[0].CreatedAt
	f.tracker.rows = append(f.tracker.rows,
		task.Comment{ID: "conflict", CreatedAt: enqueued.Add(time.Nanosecond), Body: "阶段: Agent处置\n归属: 处置Agent\n" + merge.ProgressPrefix + string(conflict)},
		task.Comment{ID: "result", CreatedAt: enqueued.Add(2 * time.Nanosecond), Body: "阶段: Agent处置\n归属: 处置Agent term=controller\n" + resolver.Prefix + string(record)})
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
	for i := 0; i < 3 && f.tracker.issue.State.Name != "Done"; i++ {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		c := merge.Candidate{Request: merge.Request{Repository: h.Repository, QueueID: h.QueueID}, Head: h.Head}
		deadline := time.Now().Add(10 * time.Second)
		for !f.runtime.mergeValidations().Idle(c) {
			if time.Now().After(deadline) {
				t.Fatal("resolver gate still running")
			}
			time.Sleep(10 * time.Millisecond)
		}
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

type resolverTierProbe struct {
	resolverStartupProbe
	starts []orca.StartOptions
	stop   error
}

func (o *resolverTierProbe) RunCreate(context.Context, string, string) (orca.Receipt, error) {
	return orca.Receipt{"runId": json.RawMessage(`"run_top"`)}, nil
}

func (o *resolverTierProbe) WorkerStart(_ context.Context, _ string, opts orca.StartOptions) (orca.Receipt, error) {
	o.starts = append(o.starts, opts)
	return nil, o.stop
}

func (o *resolverTierProbe) TaskList(context.Context, string) ([]orca.Task, error) {
	return nil, o.stop
}

// OF-555, 2026-11-18: attempt 7 waited on the frozen Fable tier after the
// operator switched AGENT_ROLE_RESOLVER_TOP; attempt 8 was the running round.
// spec and top_reason are redacted.
func TestTopResolverFollowsChangedConfig(t *testing.T) {
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded(t, "ledger-comments-top-resolver-tier.json"), &recording); err != nil {
		t.Fatal(err)
	}
	waiting, running := recording.Comments[0].Body, recording.Comments[1].Body
	var recordedWaiting, recordedRunning resolver.Record
	if err := json.Unmarshal([]byte(strings.TrimPrefix(waiting, resolver.Prefix)), &recordedWaiting); err != nil || recordedWaiting.Phase != "waiting" {
		t.Fatalf("recorded waiting round: %+v, %v", recordedWaiting, err)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(running, resolver.Prefix)), &recordedRunning); err != nil || recordedRunning.Phase != "running" {
		t.Fatalf("recorded running round: %+v, %v", recordedRunning, err)
	}
	for _, tc := range []struct {
		name, tier, agent, model, effort string
	}{
		{"changed", "codex:gpt-6-astra:max", "codex", "gpt-6-astra", "max"},
		{"unchanged", "claude:claude-fable-5-2:xhigh", "claude", "claude-fable-5-2", "xhigh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
			if err := os.MkdirAll(roles, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve"), 0600); err != nil {
				t.Fatal(err)
			}
			store := &fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", waiting)}
			probe := &resolverTierProbe{stop: errors.New("stop after worker start")}
			r := &Runtime{Clients: map[string]issues.Store{repo: store}, Repositories: map[string]config.Repository{repo: {Env: map[string]string{"AGENT_ROLE_RESOLVER_TOP": tc.tier}}}, Orca: probe}
			ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-583", Repo: repo}, store.issueComments)
			if err := r.ResolveTicket(context.Background(), ticket, nil, "controller", nil); !errors.Is(err, probe.stop) {
				t.Fatalf("expected worker start probe stop, got %v", err)
			}
			if len(probe.starts) != 1 {
				t.Fatalf("worker starts = %d, want 1", len(probe.starts))
			}
			start := probe.starts[0]
			if start.Agent != tc.agent || start.Model != tc.model || start.Effort != tc.effort {
				t.Errorf("worker tier = %s:%s:%s, want %s", start.Agent, start.Model, start.Effort, tc.tier)
			}
			after := fromEvidence(t, ticket, store.issueComments).Resolution
			if after == nil {
				t.Fatal("no resolver record persisted")
			}
			if after.Agent != tc.agent || after.Model != tc.model || after.Effort != tc.effort {
				t.Errorf("persisted tier = %s:%s:%s, want %s", after.Agent, after.Model, after.Effort, tc.tier)
			}
			if after.Attempt != recordedWaiting.Attempt+1 || !after.Top || !after.TopUsed || after.Failures != 1 {
				t.Errorf("retry state changed: %+v", after)
			}
		})
	}
	t.Run("running round keeps frozen tier when config cleared", func(t *testing.T) {
		repo := t.TempDir()
		roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
		if err := os.MkdirAll(roles, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve"), 0600); err != nil {
			t.Fatal(err)
		}
		store := &fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", running)}
		probe := &resolverTierProbe{stop: errors.New("stop during running recovery")}
		r := &Runtime{Clients: map[string]issues.Store{repo: store}, Repositories: map[string]config.Repository{repo: {Env: map[string]string{"AGENT_ROLE_RESOLVER_TOP": ""}}}, Orca: probe}
		ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-583", Repo: repo}, store.issueComments)
		if err := r.ResolveTicket(context.Background(), ticket, nil, recordedRunning.Creator, nil); !errors.Is(err, probe.stop) {
			t.Errorf("expected running recovery probe stop, got %v", err)
		}
		after := fromEvidence(t, ticket, store.issueComments).Resolution
		if after == nil {
			t.Fatal("no resolver record persisted")
		}
		if after.Phase != "running" || after.Agent != "claude" || after.Model != "claude-fable-5-2" || after.Effort != "xhigh" {
			t.Errorf("running record changed: %+v", after)
		}
		if len(probe.starts) != 0 {
			t.Errorf("worker starts = %d, want 0", len(probe.starts))
		}
		for _, comment := range store.comments {
			if strings.Contains(comment, "Agent处置:") || strings.Contains(comment, `"phase":"human"`) {
				t.Errorf("running round escalated: %s", comment)
			}
		}
	})
}

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
	if err := r.ResolveTicket(context.Background(), ticket, nil, "new-controller", nil); err == nil {
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

func TestResolverEvidenceRelated(t *testing.T) {
	comments := evidenceComments("阶段: Agent处置\n归属: 处置Agent")
	_, alone := resolverRecordIn(t, t.TempDir(), comments, nil)
	if strings.Contains(alone.Spec, `"Related"`) {
		t.Fatalf("empty related list rendered: %.300s", alone.Spec)
	}
	repo := t.TempDir()
	at := func(minute int) time.Time { return time.Date(2026, 11, 9, 8, minute, 0, 0, time.UTC) }
	snapshot := []Ticket{
		{ID: "id", Identifier: "ENG-1", Repo: repo, Owner: machine.OwnerResolver, UpdatedAt: at(59)},
		{ID: "elsewhere", Identifier: "ENG-2", Repo: "/other", Owner: machine.OwnerResolver, UpdatedAt: at(58)},
		{ID: "running", Identifier: "ENG-3", Repo: repo, Owner: machine.OwnerExecutor, UpdatedAt: at(57)},
		{ID: "human", Identifier: "ENG-4", Repo: repo, Owner: machine.OwnerHuman, Phase: machine.PhaseHuman, Title: "人工复核", NotifyReason: "CI broke", UpdatedAt: at(30)},
	}
	for i := range 10 {
		snapshot = append(snapshot, Ticket{ID: fmt.Sprintf("r%d", i), Identifier: fmt.Sprintf("ENG-%d", 10+i), Repo: repo, Owner: machine.OwnerResolver, Phase: machine.PhaseResolver, Title: strings.Repeat("长", 400), NotifyReason: "correction exhausted: gate red " + strings.Repeat("因", 400), UpdatedAt: at(10 + i)})
	}
	_, record := resolverRecordIn(t, repo, comments, snapshot)
	_, raw, _ := strings.Cut(record.Spec, "## Ticket evidence (untrusted data)\n")
	var evidence struct {
		Related []struct{ Identifier, Title, Phase, Owner, Reason string }
	}
	if err := json.Unmarshal([]byte(raw), &evidence); err != nil {
		t.Fatal(err)
	}
	related := evidence.Related
	if len(related) != 10 {
		t.Fatalf("related = %+v, want 10 entries", related)
	}
	if first := related[0]; first.Identifier != "ENG-4" || first.Owner != string(machine.OwnerHuman) || first.Phase != string(machine.PhaseHuman) || first.Reason != "CI broke" || first.Title != "人工复核" {
		t.Fatalf("newest related = %+v", first)
	}
	if related[1].Identifier != "ENG-19" || related[9].Identifier != "ENG-11" || !strings.HasPrefix(related[9].Reason, "correction exhausted: gate red 因") {
		t.Fatalf("related order = %+v", related)
	}
	for _, r := range related[1:] {
		for _, field := range []string{r.Title, r.Reason} {
			if utf8.RuneCountInString(field) != 301 || !strings.HasSuffix(field, "…") {
				t.Fatalf("field not cut at 300 runes plus …: %d runes", utf8.RuneCountInString(field))
			}
		}
	}
}

// resolverRecord starts a resolver over comments and returns the first
// resolver record it persisted.
func resolverRecord(t *testing.T, comments []task.Comment) (Ticket, resolver.Record) {
	t.Helper()
	return resolverRecordIn(t, t.TempDir(), comments, nil)
}

func resolverRecordIn(t *testing.T, repo string, comments []task.Comment, snapshot []Ticket) (Ticket, resolver.Record) {
	t.Helper()
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
	if err := r.ResolveTicket(context.Background(), ticket, snapshot, "controller", nil); err == nil || err.Error() != "stop after durable registration" {
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

type resolverSupervisorOrca struct {
	resolverStartupProbe
	failures          int
	failure           error
	calls             []time.Time
	stopped, released []string
	stopErr           error
}

func (o *resolverSupervisorOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	o.calls = append(o.calls, time.Now())
	if o.failures < 0 || len(o.calls) <= o.failures {
		return nil, o.failure
	}
	report, _ := json.Marshal(orca.WorkerReport{Provenance: "worker_report", Outcome: "succeeded", Body: "resolver proposal"})
	return []orca.Task{{ID: "task_1", RunID: "run_1", DispatchID: "ctx_1", Status: "completed", Result: report}}, nil
}

func (o *resolverSupervisorOrca) WorkerStop(_ context.Context, _, dispatch string) (orca.Receipt, error) {
	o.stopped = append(o.stopped, dispatch)
	return nil, o.stopErr
}

func (o *resolverSupervisorOrca) WorkerRelease(_ context.Context, _, dispatch string) (orca.Receipt, error) {
	o.released = append(o.released, dispatch)
	return nil, nil
}

type recordedResolverSupervisorOrca struct {
	resolverSupervisorOrca
	runID         json.RawMessage
	workerReceipt orca.Receipt
}

func (o *recordedResolverSupervisorOrca) RunCreate(context.Context, string, string) (orca.Receipt, error) {
	return orca.Receipt{"runId": o.runID}, nil
}

func (o *recordedResolverSupervisorOrca) WorkerStart(context.Context, string, orca.StartOptions) (orca.Receipt, error) {
	return o.workerReceipt, nil
}

func TestResolverSupervisorRetriesTransientOrcaFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failures    int
		stopErr     error
		permanent   bool
		cancelAfter time.Duration
	}{
		{name: "recovers after two failures", failures: 2},
		{name: "nontransient exits immediately", failures: -1, permanent: true},
		{name: "cancellation interrupts backoff", failures: -1, cancelAfter: time.Second},
		{name: "stops and releases after five minutes", failures: -1},
		{name: "stop failure is logged before exit", failures: -1, stopErr: errors.New("stop unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
			if err := os.MkdirAll(roles, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve the ticket"), 0600); err != nil {
				t.Fatal(err)
			}
			record, err := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "running", Creator: "controller", Run: "run_1", Task: "task_1", Dispatch: "ctx_1"})
			if err != nil {
				t.Fatal(err)
			}
			comments := evidenceComments("阶段: Agent处置\n归属: 处置Agent term=controller", resolver.Prefix+string(record))
			ledger := &fakeIssues{issueComments: comments}
			transient := &infra.Transient{Err: errors.New("Orca connection reset")}
			oc := &resolverSupervisorOrca{failures: tc.failures, failure: fmt.Errorf("task list: %w", transient), stopErr: tc.stopErr}
			if tc.permanent {
				oc.failure = errors.New("invalid Orca task response")
			}
			r := &Runtime{Clients: map[string]issues.Store{repo: ledger}, Repositories: map[string]config.Repository{repo: {Path: repo}}, Orca: oc, StateRoot: t.TempDir()}
			ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "ENG-1", Repo: repo}, comments)
			var logs bytes.Buffer
			ctx := runx.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				if tc.cancelAfter > 0 {
					go func() { time.Sleep(tc.cancelAfter); cancel() }()
				}
				err := r.ResolveTicket(ctx, ticket, nil, "controller", nil)
				if tc.permanent || tc.cancelAfter > 0 {
					wantErr := oc.failure
					if tc.cancelAfter > 0 {
						wantErr = context.Canceled
					}
					if !errors.Is(err, wantErr) || time.Since(start) != tc.cancelAfter || len(oc.calls) != 1 {
						t.Fatalf("unexpected retry: err=%v elapsed=%s calls=%d", err, time.Since(start), len(oc.calls))
					}
					if len(oc.stopped) != 0 || len(oc.released) != 0 {
						t.Fatalf("unexpected cleanup: stopped=%v released=%v", oc.stopped, oc.released)
					}
					return
				}
				if tc.failures == 2 {
					if err != nil {
						t.Fatalf("supervisor exited during transient failure: %v", err)
					}
					if len(oc.calls) != 3 {
						t.Fatalf("task reads = %d, want 3", len(oc.calls))
					}
					first, second := oc.calls[1].Sub(oc.calls[0]), oc.calls[2].Sub(oc.calls[1])
					if first <= 0 || second <= first {
						t.Fatalf("retry delays = %s, %s, want increasing backoff", first, second)
					}
					p, err := readResolution(ledger.issueComments)
					if err != nil || p == nil || p.Phase != "result" || p.Outcome != "succeeded" || p.Result != "resolver proposal" {
						t.Fatalf("result = %+v, err = %v", p, err)
					}
					if len(oc.stopped) != 0 || len(oc.released) != 1 {
						t.Fatalf("recovered worker cleanup: stopped=%v released=%v", oc.stopped, oc.released)
					}
					return
				}
				if !errors.Is(err, transient) {
					t.Fatalf("exit error = %v, want original transient error", err)
				}
				if elapsed := time.Since(start); elapsed != 5*time.Minute {
					t.Errorf("retry window = %s, want 5m", elapsed)
				}
				if len(oc.stopped) != 1 || oc.stopped[0] != "ctx_1" {
					t.Errorf("worker not stopped before exit: %v", oc.stopped)
				}
				if len(oc.released) != 1 || oc.released[0] != "ctx_1" {
					t.Errorf("worker not released before exit: %v", oc.released)
				}
				if tc.stopErr != nil && !strings.Contains(logs.String(), "stop unavailable") {
					t.Errorf("stop failure missing from logs: %s", logs.String())
				}
			})
		})
	}
}

func TestResolverSupervisorRetriesTransientOrcaFailureRecordedReceipt(t *testing.T) {
	var workerEnvelope struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(recorded(t, "orca-worker-start-reuse-terminal.json"), &workerEnvelope); err != nil {
		t.Fatal(err)
	}
	var runEnvelope struct {
		Result struct {
			RunID json.RawMessage `json:"runId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorded(t, "orca-worker-start-reuse-terminal.json"), &runEnvelope); err != nil {
		t.Fatal(err)
	}
	if len(runEnvelope.Result.RunID) == 0 {
		t.Fatal("recorded worker-start receipt has no result.runId")
	}

	t.Run("running recorded receipt cleanup", func(t *testing.T) {
		repo := t.TempDir()
		roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
		if err := os.MkdirAll(roles, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve the ticket"), 0600); err != nil {
			t.Fatal(err)
		}
		comments := evidenceComments("阶段: Agent处置\n归属: 处置Agent term=controller")
		ledger := &fakeIssues{issueComments: comments}
		transient := &infra.Transient{Err: errors.New("Orca connection reset")}
		oc := &recordedResolverSupervisorOrca{
			resolverSupervisorOrca: resolverSupervisorOrca{
				failures: -1,
				failure:  fmt.Errorf("task list: %w", transient),
			},
			runID:         runEnvelope.Result.RunID,
			workerReceipt: workerEnvelope.Result,
		}
		r := &Runtime{Clients: map[string]issues.Store{repo: ledger}, Repositories: map[string]config.Repository{repo: {Path: repo}}, Orca: oc, StateRoot: t.TempDir()}
		ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "ENG-1", Repo: repo}, comments)

		synctest.Test(t, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- r.ResolveTicket(context.Background(), ticket, nil, "controller", nil) }()
			synctest.Wait()
			time.Sleep(5*time.Minute + 15*time.Second)
			synctest.Wait()
			err := <-done
			if len(oc.calls) == 0 {
				t.Fatal("recorded worker-start receipt never reached task polling")
			}
			if !errors.Is(err, transient) {
				t.Fatalf("exit error = %v, want original transient error", err)
			}
			if len(oc.stopped) != 1 || oc.stopped[0] != "ctx_6ad0f5744b10" {
				t.Fatalf("worker not stopped before exit: %v", oc.stopped)
			}
			if len(oc.released) != 1 || oc.released[0] != "ctx_6ad0f5744b10" {
				t.Fatalf("worker not released before exit: %v", oc.released)
			}
			p, err := readResolution(ledger.issueComments)
			if err != nil || p == nil || p.Phase != "running" || p.Dispatch != "ctx_6ad0f5744b10" {
				t.Fatalf("latest recorded resolution = %+v, err = %v", p, err)
			}
		})
	})

	t.Run("ended recorded receipt is not cleaned up", func(t *testing.T) {
		var recording struct{ Comments []task.Comment }
		if err := json.Unmarshal(recorded(t, "ledger-comments-resolver-requeue.json"), &recording); err != nil {
			t.Fatal(err)
		}
		ledger := &fakeIssues{issueComments: recording.Comments}
		repo := t.TempDir()
		r := &Runtime{Clients: map[string]issues.Store{repo: ledger}}
		ticket := Ticket{ID: "id", Identifier: "ENG-1", Repo: repo}
		p, err := readResolution(ledger.issueComments)
		if err != nil || p == nil || p.Phase != "result" || p.Dispatch != "ctx_ce8b1432b8a0" {
			t.Fatalf("recorded resolution = %+v, err = %v", p, err)
		}
		oc := &resolverSupervisorOrca{}
		c := resolver.Controller{Store: resolutionStore{r, ticket}, Orca: oc, Ticket: ticket.Identifier, Repo: repo, Terminal: "controller"}
		stopResolverAfterTransientFailure(context.Background(), &c)
		if len(oc.stopped) != 0 || len(oc.released) != 0 {
			t.Fatalf("ended resolver round was cleaned up: stopped=%v released=%v", oc.stopped, oc.released)
		}
	})
}

func TestTopResolverUsesFallbackWhenPrimaryUnavailable(t *testing.T) {
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded(t, "ledger-comments-top-resolver-tier.json"), &recording); err != nil {
		t.Fatal(err)
	}
	waiting := recording.Comments[0].Body
	primary := modelhealth.Ref{Agent: "codex", Model: "gpt-6.2-astra", Effort: "max"}
	for _, tc := range []struct {
		name        string
		unavailable bool
		want        modelhealth.Ref
	}{
		{"primary unavailable", true, modelhealth.Ref{Agent: "claude", Model: "claude-opus-5-5", Effort: "max"}},
		{"primary available", false, primary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, state := t.TempDir(), t.TempDir()
			roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
			if err := os.MkdirAll(roles, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve"), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.unavailable {
				registry, err := modelhealth.Open(state)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				if err = registry.Record(modelhealth.Status{Ref: primary, State: modelhealth.Unavailable, Reason: "usage_limit", Until: now.Add(time.Hour), CheckedAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			store := &fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", waiting)}
			probe := &resolverTierProbe{stop: errors.New("stop after worker start")}
			env := map[string]string{"AGENT_ROLE_RESOLVER_TOP": primary.String(), "AGENT_ROLE_RESOLVER_TOP_FALLBACK": "claude:claude-opus-5-5:max codex:gpt-6-astra:xhigh"}
			r := &Runtime{Clients: map[string]issues.Store{repo: store}, Repositories: map[string]config.Repository{repo: {Env: env}}, Orca: probe, StateRoot: state}
			ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-593", Repo: repo}, store.issueComments)
			if err := r.ResolveTicket(context.Background(), ticket, nil, "controller", nil); !errors.Is(err, probe.stop) {
				t.Fatalf("expected worker start probe stop, got %v", err)
			}
			if len(probe.starts) != 1 {
				t.Fatalf("worker starts = %d, want 1", len(probe.starts))
			}
			if got := (modelhealth.Ref{Agent: probe.starts[0].Agent, Model: probe.starts[0].Model, Effort: probe.starts[0].Effort}); got != tc.want {
				t.Errorf("worker tier = %s, want %s", got, tc.want)
			}
			after := fromEvidence(t, ticket, store.issueComments).Resolution
			if after == nil {
				t.Fatal("no resolver record persisted")
			}
			if got := (modelhealth.Ref{Agent: after.Agent, Model: after.Model, Effort: after.Effort}); got != tc.want {
				t.Errorf("recorded tier = %s, want %s", got, tc.want)
			}
		})
	}
}

type labelIssues struct {
	*fakeIssues
	added, removed        [][]string
	commentErr, updateErr error
	unconfirmedErr        error
	rereadErr             error
	// commentsAtUpdate is len(comments) when each UpdateIssue ran.
	commentsAtUpdate []int
	// duringUpdate runs inside the next UpdateIssue, as a concurrent writer.
	duringUpdate func()
}

func (l *labelIssues) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (task.Issue, error) {
	l.added, l.removed = append(l.added, add), append(l.removed, remove)
	l.commentsAtUpdate = append(l.commentsAtUpdate, len(l.comments))
	if l.updateErr != nil {
		return task.Issue{}, l.updateErr
	}
	if during := l.duringUpdate; during != nil {
		l.duringUpdate = nil
		during()
	}
	i := slices.IndexFunc(l.issues, func(issue task.Issue) bool { return issue.ID == id })
	if i < 0 {
		return l.fakeIssues.UpdateIssue(ctx, id, state, add, remove)
	}
	ledger := mergeIssues{issue: l.issues[i], labels: true}
	issue, err := ledger.UpdateIssue(ctx, id, state, add, remove)
	if err != nil {
		return task.Issue{}, err
	}
	l.issues[i] = issue
	if l.unconfirmedErr != nil {
		err, l.unconfirmedErr = l.unconfirmedErr, nil
		return task.Issue{}, err
	}
	return issue, nil
}

func (l *labelIssues) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	if l.commentErr != nil {
		return task.Comment{}, l.commentErr
	}
	return l.fakeIssues.CreateComment(ctx, id, body)
}

func (l *labelIssues) ListComments(ctx context.Context, id string) ([]task.Comment, error) {
	if len(l.added) > 0 && l.rereadErr != nil {
		return nil, l.rereadErr
	}
	return l.fakeIssues.ListComments(ctx, id)
}

func TestModelParkResumeWritesRecoveryBeforeLabels(t *testing.T) {
	for _, fail := range []string{"comment", "labels", "labels and restore", "labels and marker", "reread"} {
		t.Run(fail, func(t *testing.T) {
			reason := "模型不可用：codex:gpt-6.2-astra:max 至 2026-11-20T09:00:00Z"
			marker := "搁置：" + reason
			issue := task.Issue{ID: "id", Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: parkedLabel}}}}
			store := &labelIssues{fakeIssues: &fakeIssues{issues: []task.Issue{issue}, issueComments: evidenceComments(marker)}}
			failure := errors.New("ledger write failed")
			markerFailure := errors.New("marker write failed")
			switch fail {
			case "comment":
				store.commentErr = failure
			case "labels":
				store.unconfirmedErr = failure
			case "labels and restore":
				store.updateErr = failure
			case "labels and marker":
				store.unconfirmedErr = failure
				store.duringUpdate = func() { store.commentErr = markerFailure }
			case "reread":
				store.rereadErr = failure
			}
			r := &Runtime{Clients: map[string]issues.Store{"a": store}, StateRoot: t.TempDir()}
			ticket := Ticket{ID: "id", Identifier: "OF-616", Repo: "a", Parked: reason}
			err := r.ResumeModelParks(context.Background(), []Ticket{ticket})
			if !errors.Is(err, failure) {
				t.Fatalf("resume error=%v, want write failure", err)
			}
			switch fail {
			case "comment":
				if len(store.added) != 0 || len(store.removed) != 0 || len(store.comments) != 0 {
					t.Errorf("failed recovery comment changed ticket: added=%v removed=%v comments=%q", store.added, store.removed, store.comments)
				}
			case "labels":
				if !reflect.DeepEqual(store.added, [][]string{{"ready-for-agent"}, {parkedLabel}}) || !reflect.DeepEqual(store.removed, [][]string{{parkedLabel}, {"ready-for-agent"}}) {
					t.Errorf("label updates: added=%v removed=%v", store.added, store.removed)
				}
				if len(store.comments) != 2 || !strings.HasPrefix(store.comments[0], "恢复：") || store.comments[1] != marker {
					t.Errorf("failed label update did not restore original marker: comments=%q", store.comments)
				}
				if !slices.Equal(store.commentsAtUpdate, []int{1, 1}) {
					t.Errorf("comments at label updates=%v, want [1 1]", store.commentsAtUpdate)
				}
			case "labels and restore", "labels and marker":
				if !strings.Contains(err.Error(), "manual recovery required") {
					t.Errorf("missing manual recovery instruction: %v", err)
				}
				if fail == "labels and marker" && !errors.Is(err, markerFailure) {
					t.Errorf("missing marker failure: %v", err)
				}
				if len(store.comments) != 1 || !strings.HasPrefix(store.comments[0], "恢复：") {
					t.Errorf("want only the recovery comment, got %q", store.comments)
				}
				return
			case "reread":
				if len(store.comments) != 1 || !strings.HasPrefix(store.comments[0], "恢复：") {
					t.Errorf("failed reread overwrote an unknown park: comments=%q", store.comments)
				}
				if len(store.added) != 2 || !slices.Equal(store.added[1], []string{parkedLabel}) || !slices.Equal(store.removed[1], []string{"ready-for-agent"}) {
					t.Errorf("failed reread did not restore parked labels: added=%v removed=%v", store.added, store.removed)
				}
				return
			}
			fresh, err := store.FreshIssue(context.Background(), ticket.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(fresh.Labels.Nodes, []task.NamedID{{Name: parkedLabel}}) {
				t.Errorf("labels after failed resume=%v, want only parked", fresh.Labels.Nodes)
			}
			protocol := protocolComments(store.issueComments)
			if current, _ := parkedState(fresh, protocol); current != reason || !modelParkMarked(protocol, reason) {
				t.Errorf("failed resume lost model park: reason=%q comments=%v", current, store.issueComments)
			}
		})
	}
	t.Run("operator recovery ends model park", func(t *testing.T) {
		reason := "模型不可用：codex:gpt-6.2-astra:max 至 2026-11-20T09:00:00Z"
		comments := evidenceComments("搁置："+reason, "恢复：人工恢复模型搁置")
		comments[1].Source = task.SourceOperator
		issue := task.Issue{ID: "id", Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: parkedLabel}}}}
		store := &labelIssues{fakeIssues: &fakeIssues{issues: []task.Issue{issue}, issueComments: comments}}
		r := &Runtime{Clients: map[string]issues.Store{"a": store}, StateRoot: t.TempDir()}
		if err := r.ResumeModelParks(context.Background(), []Ticket{{ID: "id", Repo: "a", Parked: reason}}); err != nil {
			t.Fatal(err)
		}
		if len(store.added) != 0 || len(store.comments) != 0 {
			t.Fatalf("operator recovery left old model marker active: labels=%v comments=%q", store.added, store.comments)
		}
	})
}

func TestModelUnavailableParkRecoversWhenCandidateAvailable(t *testing.T) {
	repo, state := t.TempDir(), t.TempDir()
	roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
	if err := os.MkdirAll(roles, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve"), 0600); err != nil {
		t.Fatal(err)
	}
	primary := modelhealth.Ref{Agent: "claude", Model: "claude-opus-5-5", Effort: "high"}
	fallback := modelhealth.Ref{Agent: "codex", Model: "gpt-6.2-astra", Effort: "high"}
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	registry, err := modelhealth.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	for i, ref := range []modelhealth.Ref{primary, fallback} {
		if err := registry.Record(modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", Until: now.Add(time.Duration(i+1) * time.Hour), CheckedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	line := func(r resolver.Record) string {
		raw, _ := json.Marshal(r)
		return resolver.Prefix + string(raw)
	}
	failed := resolver.Record{Version: 1, Attempt: 1, Phase: "waiting", Outcome: "failed", Result: "worker failed", Spec: "Resolve", Agent: primary.Agent}
	limited := resolver.Record{Version: 1, Attempt: 2, Phase: "waiting", Outcome: resolver.ModelUnavailable, RetryAt: now, Result: "model unavailable", Spec: "Resolve", Agent: fallback.Agent, Model: fallback.Model, Effort: fallback.Effort}
	store := &labelIssues{fakeIssues: &fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", line(failed), line(limited))}}
	probe := &resolverTierProbe{stop: errors.New("dispatched while every candidate is unavailable")}
	env := map[string]string{"AGENT_ROLE_RESOLVER": primary.String(), "AGENT_ROLE_RESOLVER_FALLBACK": fallback.String()}
	r := &Runtime{Clients: map[string]issues.Store{repo: store}, Repositories: map[string]config.Repository{repo: {Env: env}}, Orca: probe, StateRoot: state, Now: func() time.Time { return now }}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-594", Repo: repo}, store.issueComments)
	if ticket.Resolution.Failures != 1 {
		t.Fatalf("model-unavailable round counted as failure: failures=%d", ticket.Resolution.Failures)
	}
	if err := r.ResolveTicket(context.Background(), ticket, nil, "controller", nil); err != nil {
		t.Fatal(err)
	}
	if len(probe.starts) != 0 || len(store.added) != 1 || !slices.Contains(store.added[0], parkedLabel) {
		t.Fatalf("not parked: starts=%d added=%v", len(probe.starts), store.added)
	}
	parked := fromEvidence(t, ticket, store.issueComments)
	parked.Parked, _ = parkedState(task.Issue{Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: parkedLabel}}}}, store.issueComments)
	if !strings.HasPrefix(parked.Parked, "模型不可用") || !strings.Contains(parked.Parked, primary.String()) || !strings.Contains(parked.Parked, fallback.String()) {
		t.Fatalf("park reason=%q", parked.Parked)
	}
	other := parked
	other.ID, other.Parked = "other", "处置连续失败 3 次: worker failed"
	store.issues = []task.Issue{{ID: "id", Identifier: "OF-594", Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: parkedLabel}}}}}
	if err := r.ResumeModelParks(context.Background(), []Ticket{parked, other}); err != nil || len(store.added) != 1 {
		t.Fatalf("resumed while every candidate is unavailable: added=%v err=%v", store.added, err)
	}
	now = now.Add(time.Hour)
	// A 搁置 written after the snapshot, for lane 3, still holds the ticket.
	stale := store.issueComments
	store.issueComments = append(slices.Clone(stale), task.Comment{ID: "lane3", Body: "搁置：lane 3 path .env\n阶段: 待领\n归属: 无", CreatedAt: time.Now().Add(time.Hour)})
	if err := r.ResumeModelParks(context.Background(), []Ticket{parked}); err != nil || len(store.added) != 1 {
		t.Fatalf("stale snapshot lifted a newer park: added=%v err=%v", store.added, err)
	}
	// A 搁置 landing between the reread and the label write puts the park back.
	store.issueComments = slices.Clone(stale)
	store.duringUpdate = func() {
		store.issueComments = append(store.issueComments, task.Comment{ID: "lane3-late", Body: "搁置：lane 3 path .env\n阶段: 待领\n归属: 无", CreatedAt: time.Now().Add(time.Hour)})
	}
	if err := r.ResumeModelParks(context.Background(), []Ticket{parked}); err != nil || len(store.added) != 3 || !slices.Equal(store.added[2], []string{parkedLabel}) || !slices.Equal(store.removed[2], []string{"ready-for-agent"}) {
		t.Fatalf("concurrent park not restored: added=%v removed=%v err=%v", store.added, store.removed, err)
	}
	store.added, store.removed = store.added[:1], store.removed[:1]
	store.issueComments = stale
	if err := r.ResumeModelParks(context.Background(), []Ticket{parked, other}); err != nil {
		t.Fatal(err)
	}
	if len(store.added) != 2 || !slices.Equal(store.added[1], []string{"ready-for-agent"}) || !slices.Equal(store.removed[1], []string{parkedLabel}) {
		t.Fatalf("not resumed once: added=%v removed=%v", store.added, store.removed)
	}
}

// The 搁置 line keeps every candidate of a long fallback list, which the
// recorded ledger read returns as written and ResumeModelParks reads back
// (testdata/recorded/ledger-issue-parked-resume.json).
func TestRecordedModelParkKeepsEveryCandidate(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	registry, err := modelhealth.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []modelhealth.Status
	for i := range 8 {
		ref := modelhealth.Ref{Agent: "codex", Model: fmt.Sprintf("gpt-6.%d-astra-preview", i), Effort: "xhigh"}
		status := modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", Until: now.Add(time.Hour), CheckedAt: now}
		if i == 7 {
			status.Until = now.Add(time.Minute)
		}
		if err := registry.Record(status); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, status)
	}
	reason := config.ModelParkReason(statuses)
	if len([]rune(reason)) <= 300 {
		t.Fatalf("reason fits the ledger line cap: %d runes", len([]rune(reason)))
	}
	parked, _ := recordedPark(t)
	store := &labelIssues{fakeIssues: &fakeIssues{issues: []task.Issue{parked.Issue}, issueComments: parked.Comments.Nodes[:1]}}
	r := &Runtime{Clients: map[string]issues.Store{"repo": store}, Repositories: map[string]config.Repository{"repo": {}}, StateRoot: state, Now: func() time.Time { return now }}
	ticket := Ticket{ID: parked.ID, Identifier: parked.Identifier, Repo: "repo"}
	// An escalation reason copying the model format parks for a person, never for the model.
	if err := r.parkTicket(context.Background(), ticket, reason, "处置转人", ""); err != nil {
		t.Fatal(err)
	}
	forged := ticket
	forged.Parked, _ = parkedState(parked.Issue, protocolComments(store.issueComments))
	if strings.HasPrefix(forged.Parked, config.ModelParked) || len(store.comments) != 1 {
		t.Fatalf("agent reason passed for a model park: %q comments=%d", forged.Parked, len(store.comments))
	}
	store.comments, store.commentsAtUpdate, store.added, store.removed = nil, nil, nil, nil
	if err := r.park(context.Background(), ticket, reason, "worker 派工已停止", "", true); err != nil {
		t.Fatal(err)
	}
	if len(store.comments) != 2 || store.comments[0] != "搁置："+reason || store.commentsAtUpdate[0] != 1 {
		t.Fatalf("model reason not written before the labels: comments=%q at update=%v", store.comments, store.commentsAtUpdate)
	}
	marker, full := parked.Comments.Nodes[1], parked.Comments.Nodes[1]
	marker.ID, marker.Body, full.Body = "marker", store.comments[0], store.comments[1]
	ticket.Parked, _ = parkedState(parked.Issue, []task.Comment{parked.Comments.Nodes[0], full})
	if ticket.Parked != reason {
		t.Fatalf("park line=%q want %q", ticket.Parked, reason)
	}
	now = now.Add(2 * time.Minute)
	// A full 搁置 alone, as an older escalation reason in the model format, is no model park.
	store.issueComments = []task.Comment{parked.Comments.Nodes[0], full}
	if err := r.ResumeModelParks(context.Background(), []Ticket{ticket}); err != nil || len(store.added) != 1 {
		t.Fatalf("unmarked park resumed: added=%v err=%v", store.added, err)
	}
	// A park lifted by 恢复 and parked again by a person keeps its old marker only.
	resumed := task.Comment{ID: "resumed", Body: "恢复：模型候选已恢复可用，自动解除「模型不可用」搁置", CreatedAt: full.CreatedAt.Add(time.Minute)}
	store.issueComments = []task.Comment{parked.Comments.Nodes[0], marker, full, resumed}
	if err := r.ResumeModelParks(context.Background(), []Ticket{ticket}); err != nil || len(store.added) != 1 {
		t.Fatalf("park after a resume lifted: added=%v err=%v", store.added, err)
	}
	store.issueComments = []task.Comment{parked.Comments.Nodes[0], marker, full}
	now = now.Add(-2 * time.Minute)
	if err := r.ResumeModelParks(context.Background(), []Ticket{ticket}); err != nil || len(store.added) != 1 {
		t.Fatalf("resumed before any candidate recovered: added=%v err=%v", store.added, err)
	}
	now = now.Add(2 * time.Minute)
	if err := r.ResumeModelParks(context.Background(), []Ticket{ticket}); err != nil || len(store.added) != 2 || !slices.Equal(store.added[1], []string{"ready-for-agent"}) {
		t.Fatalf("last candidate's recovery did not resume: added=%v err=%v", store.added, err)
	}
}

// The ledger returns a model park marker and its 恢复 as written
// (testdata/recorded/ledger-comments-model-park.json): the marker names every
// candidate, and the 恢复 after it ends the marker.
func TestRecordedLedgerModelParkMarker(t *testing.T) {
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded(t, "ledger-comments-model-park.json"), &recording); err != nil {
		t.Fatal(err)
	}
	if len(recording.Comments) != 2 {
		t.Fatalf("recorded comments=%d", len(recording.Comments))
	}
	labeled := task.Issue{Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: parkedLabel}}}}
	marker := protocolComments(recording.Comments[:1])
	reason, _ := parkedState(labeled, marker)
	if !modelParkMarked(marker, reason) {
		t.Fatalf("recorded marker not a model park: %q", reason)
	}
	candidates, err := modelParkCandidates(reason)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ref := range candidates {
		names = append(names, ref.String())
	}
	if want := []string{"codex:gpt-6.2-astra:max", "claude:claude-opus-5-5:max", "codex:gpt-6-astra:xhigh"}; !slices.Equal(names, want) {
		t.Fatalf("candidates=%v want %v", names, want)
	}
	if modelParkMarked(protocolComments(recording.Comments), reason) {
		t.Fatal("recorded 恢复 left the marker in force")
	}
}

// A worker parked for its model that stopped before releasing its claim is
// not restored as a dead executor while the park holds.
func TestModelParkedExecutorClaimIsNotRestored(t *testing.T) {
	f := &fakeBackend{absent: true, inactive: true}
	s := setup(f)
	parked := Ticket{ID: "ticket", Repo: "a", Owner: machine.OwnerExecutor, Attempt: 1, UpdatedAt: s.Now(), Parked: config.ModelParked + "：claude:claude-fable-5-2:xhigh 至 2026-11-21T01:00:00Z"}
	f.tickets = []Ticket{parked}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 0 || len(f.actions) != 0 {
		t.Fatalf("model-parked claim restored: launched=%v actions=%v", f.launched, f.actions)
	}
	parked.Parked = ""
	f.tickets = []Ticket{parked}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 1 {
		t.Fatalf("lifted park not restored: launched=%v", f.launched)
	}
}
