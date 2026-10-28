package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

const (
	mergedGreen = "UNIT executor\nVERDICT green\nSTEP all\nREASON checked\nEVIDENCE test\nSCOPE feature shared\nLANE 1"
	mergedRed   = "UNIT executor\nVERDICT red\nSTEP all\nREASON checked\nEVIDENCE test\nSCOPE feature shared\nLANE 1"
)

// blockingGate stands in for the merged-result gate process: each run reports
// the main it validates and then waits for a verdict or cancellation.
type blockingGate struct {
	started chan string
	verdict chan string
}

func blockGate(f *mergeFixture) *blockingGate {
	g := &blockingGate{started: make(chan string, 4), verdict: make(chan string, 4)}
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path != "bash" || len(s.Args) != 3 {
			return runx.Run(ctx, s)
		}
		for _, entry := range s.Env {
			if base, ok := strings.CutPrefix(entry, "GATE_BASE="); ok {
				g.started <- base
			}
		}
		select {
		case v := <-g.verdict:
			switch v {
			case "red":
				return []byte(mergedRed), gateExit{}
			case "infra":
				return nil, errors.New("cannot launch gate")
			}
			return []byte(mergedGreen), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g
}

func (g *blockingGate) awaitStart(t *testing.T) string {
	t.Helper()
	select {
	case main := <-g.started:
		return main
	case <-time.After(10 * time.Second):
		t.Fatal("merged-result gate did not start")
		return ""
	}
}

func (g *blockingGate) noRerun(t *testing.T) {
	t.Helper()
	select {
	case main := <-g.started:
		t.Fatalf("merged-result gate reran on %s", main)
	default:
	}
}

// tickAlone fails when Tick waits for a merged-result gate that never exits.
func tickAlone(t *testing.T, s *Scheduler) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.Tick(ctx)
	if ctx.Err() != nil {
		t.Fatalf("tick waited for the merged-result gate: %v", err)
	}
	return err
}

func closeMergeGates(t *testing.T, r *Runtime) {
	t.Cleanup(func() {
		if err := r.Close(10 * time.Second); err != nil {
			t.Error(err)
		}
	})
}

// awaitMergeGate returns once the fixture ticket's gate has exited, keeping
// its verdict for the next tick.
func awaitMergeGate(t *testing.T, r *Runtime, f *mergeFixture) {
	t.Helper()
	c := merge.Candidate{Request: merge.Request{Repository: "o/r", QueueID: "q1"}, Head: f.head}
	deadline := time.Now().Add(10 * time.Second)
	for !r.mergeValidations().Idle(c) {
		if time.Now().After(deadline) {
			t.Fatal("merged-result gate still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tickPastMergeGate ticks and, when that tick only started the merged-result
// gate, waits for the gate and ticks again.
func tickPastMergeGate(t *testing.T, s *Scheduler, r *Runtime, f *mergeFixture) error {
	t.Helper()
	if err := tickAlone(t, s); err != nil {
		return err
	}
	awaitMergeGate(t, r, f)
	return tickAlone(t, s)
}

func mergeProgress(t *testing.T, bodies []string) []merge.Progress {
	t.Helper()
	var out []merge.Progress
	for _, body := range bodies {
		for _, line := range strings.Split(body, "\n") {
			if raw, ok := strings.CutPrefix(line, merge.ProgressPrefix); ok {
				var p merge.Progress
				if err := json.Unmarshal([]byte(raw), &p); err != nil {
					t.Fatal(err)
				}
				out = append(out, p)
			}
		}
	}
	return out
}

func TestRuntimeMergeGateRunsBesideTick(t *testing.T) {
	f := overlapFixture(t, false)
	gate := blockGate(f)
	closeMergeGates(t, f.runtime)
	candidate := &mergeIssues{labels: true, issue: task.Issue{ID: "cand", Identifier: "ENG-2", Project: task.NamedID{ID: "o/r"}, State: task.NamedID{Name: "Todo"}, Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "ready-for-agent"}}}}}
	f.runtime.Clients[f.repos[0].Path] = &mergeTickets{rows: []*mergeIssues{f.tracker, candidate}}
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
	if err := tickAlone(t, s); err != nil {
		t.Fatal(err)
	}
	if main := gate.awaitStart(t); main != f.main {
		t.Fatalf("gate validated %s, want %s", main, f.main)
	}
	launches := f.runtime.Orca.(*fakeOrca).launches
	if f.puts != 0 || len(f.tracker.bodies) != 0 || len(launches) != 1 || !strings.Contains(launches[0].Command, "'ENG-2'") || candidate.issue.State.Name != "In Progress" {
		t.Fatalf("tick did not claim beside the running gate: puts=%d merge=%v launches=%v candidate=%s", f.puts, f.tracker.bodies, launches, candidate.issue.State.Name)
	}
}

func TestRuntimeMergeGateGreenSquashesOnLaterTickWithoutRerun(t *testing.T) {
	f := overlapFixture(t, false)
	gate := blockGate(f)
	closeMergeGates(t, f.runtime)
	f.rejectPuts = 1
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
	if err := tickAlone(t, s); err != nil {
		t.Fatal(err)
	}
	gate.awaitStart(t)
	if err := tickAlone(t, s); err != nil || f.puts != 0 || len(f.tracker.bodies) != 0 {
		t.Fatalf("running gate produced a decision: err=%v puts=%d bodies=%v", err, f.puts, f.tracker.bodies)
	}
	gate.noRerun(t)
	gate.verdict <- "green"
	awaitMergeGate(t, f.runtime, f)
	if err := tickAlone(t, s); err == nil || f.puts != 1 || f.merged {
		t.Fatalf("first squash after green: err=%v puts=%d merged=%v", err, f.puts, f.merged)
	}
	awaitMergeGate(t, f.runtime, f)
	gate.noRerun(t)
	if err := tickAlone(t, s); err != nil || f.puts != 2 || f.tracker.issue.State.Name != "Done" {
		t.Fatalf("retained green did not squash: err=%v puts=%d state=%s", err, f.puts, f.tracker.issue.State.Name)
	}
	gate.noRerun(t)
}

func TestRuntimeMergeGateRedCorrectsOnValidatedMain(t *testing.T) {
	f := overlapFixture(t, false)
	gate := blockGate(f)
	closeMergeGates(t, f.runtime)
	dir := f.repos[0].Path
	validated := f.main
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
	if err := tickAlone(t, s); err != nil {
		t.Fatal(err)
	}
	gate.awaitStart(t)
	gitMergeTest(t, dir, "commit", "--allow-empty", "-m", "main moves during gate")
	f.main = gitMergeTest(t, dir, "rev-parse", "HEAD")
	gate.verdict <- "red"
	awaitMergeGate(t, f.runtime, f)
	f.tracker.failBody = "归属: 执行器"
	o := f.runtime.Orca.(*fakeOrca)
	if err := tickAlone(t, s); err == nil || len(o.launches) != 0 {
		t.Fatalf("unwritten correction launched: err=%v launches=%d", err, len(o.launches))
	}
	f.tracker.failBody = ""
	awaitMergeGate(t, f.runtime, f)
	gate.noRerun(t)
	if err := tickAlone(t, s); err != nil {
		t.Fatal(err)
	}
	gate.noRerun(t)
	progress := mergeProgress(t, f.tracker.bodies)
	if len(progress) != 1 || progress[0].Phase != "correction" || progress[0].Corrections != 1 || progress[0].Main != validated || f.puts != 0 || len(o.launches) != 1 {
		t.Fatalf("red gate not corrected on its main %s: progress=%+v puts=%d launches=%d", validated, progress, f.puts, len(o.launches))
	}
}

func TestRuntimeMergeGateMainAdvanceCountsOneCatchupRound(t *testing.T) {
	f := overlapFixture(t, false)
	gate := blockGate(f)
	closeMergeGates(t, f.runtime)
	dir := f.repos[0].Path
	advance := func(message string) {
		gitMergeTest(t, dir, "commit", "--allow-empty", "-m", message)
		f.main = gitMergeTest(t, dir, "rev-parse", "HEAD")
	}
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
	if err := tickAlone(t, s); err != nil {
		t.Fatal(err)
	}
	gate.awaitStart(t)
	advance("first move")
	gate.verdict <- "green"
	awaitMergeGate(t, f.runtime, f)
	if err := tickAlone(t, s); err == nil || !strings.Contains(err.Error(), "main advanced; validate again") {
		t.Fatalf("stale green must wait one catch-up round: %v", err)
	}
	if err := tickAlone(t, s); err != nil {
		t.Fatal(err)
	}
	if main := gate.awaitStart(t); main != f.main {
		t.Fatalf("catch-up gate validated %s, want %s", main, f.main)
	}
	advance("second move")
	gate.verdict <- "green"
	awaitMergeGate(t, f.runtime, f)
	if err := tickAlone(t, s); err != nil {
		t.Fatal(err)
	}
	var rounds []int
	for _, p := range mergeProgress(t, f.tracker.bodies) {
		rounds = append(rounds, p.CatchupRounds)
	}
	if f.puts != 0 || len(rounds) != 2 || rounds[0] != 1 || rounds[1] != 2 || !strings.Contains(strings.Join(f.tracker.bodies, "\n"), "归属: 处置Agent") {
		t.Fatalf("catch-up budget: puts=%d rounds=%v bodies=%v", f.puts, rounds, f.tracker.bodies)
	}
}

func TestRuntimeMergeGateRestartRerunsInsteadOfTrustingRunningGate(t *testing.T) {
	f := overlapFixture(t, false)
	gate := blockGate(f)
	restarted := *f.runtime
	closeMergeGates(t, f.runtime)
	closeMergeGates(t, &restarted)
	if err := tickAlone(t, &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}); err != nil {
		t.Fatal(err)
	}
	gate.awaitStart(t)
	if err := f.runtime.Close(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: &restarted}
	if err := tickAlone(t, s); err != nil || f.puts != 0 || len(f.tracker.bodies) != 0 {
		t.Fatalf("restart trusted an unfinished gate: err=%v puts=%d bodies=%v", err, f.puts, f.tracker.bodies)
	}
	if main := gate.awaitStart(t); main != f.main {
		t.Fatalf("rerun validated %s, want %s", main, f.main)
	}
	gate.verdict <- "green"
	awaitMergeGate(t, &restarted, f)
	if err := tickAlone(t, s); err != nil || f.puts != 1 || f.tracker.issue.State.Name != "Done" {
		t.Fatalf("rerun green not merged: err=%v puts=%d state=%s", err, f.puts, f.tracker.issue.State.Name)
	}
}

func TestRuntimeMergeGateInfrastructureRerunsWithoutBudget(t *testing.T) {
	for _, kind := range []string{"launch", "malformed", "install"} {
		t.Run(kind, func(t *testing.T) {
			f := overlapFixture(t, false)
			closeMergeGates(t, f.runtime)
			if kind == "install" {
				f.repos[0].Env["AGENT_INSTALL_CMD"] = "make deps"
			}
			var runs atomic.Int32
			f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if s.Path != "bash" {
					return runx.Run(ctx, s)
				}
				runs.Add(1)
				switch {
				case s.Args[0] == "-c":
					return nil, errors.New("install failed")
				case kind == "malformed":
					return []byte("VERDICT red\n"), gateExit{}
				}
				return nil, errors.New("cannot launch gate")
			}
			s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
			if err := tickAlone(t, s); err != nil {
				t.Fatal(err)
			}
			awaitMergeGate(t, f.runtime, f)
			if err := tickAlone(t, s); err == nil || runs.Load() != 1 {
				t.Fatalf("infrastructure failure hidden: err=%v runs=%d", err, runs.Load())
			}
			if err := tickAlone(t, s); err != nil {
				t.Fatal(err)
			}
			awaitMergeGate(t, f.runtime, f)
			if runs.Load() != 2 || f.puts != 0 || len(f.tracker.bodies) != 0 || len(f.runtime.Orca.(*fakeOrca).launches) != 0 {
				t.Fatalf("infrastructure spent budget or did not rerun: runs=%d puts=%d bodies=%v", runs.Load(), f.puts, f.tracker.bodies)
			}
		})
	}
}
