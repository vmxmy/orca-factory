package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type livenessOrca struct {
	fakeOrca
	worker                                 orca.Worker
	polls, stops, terminalCalls, taskCalls int
	report                                 bool
	reportAt                               int
}

func (o *livenessOrca) WorkerStart(ctx context.Context, req string, opts orca.StartOptions) (orca.Receipt, error) {
	r, err := o.fakeOrca.WorkerStart(ctx, req, opts)
	delete(r, "worktreePath")
	return r, err
}
func (o *livenessOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	o.taskCalls++
	if o.reportAt > 0 && o.taskCalls >= o.reportAt {
		o.report = true
	}
	return idleTask(o.report), nil
}
func (o *livenessOrca) Workers(_ context.Context, opts orca.ListOptions) ([]orca.Worker, error) {
	if opts.Run != "run_one" || !opts.IncludeRemote {
		panic("wrong worker list scope")
	}
	o.polls++
	return []orca.Worker{o.worker}, nil
}
func (o *livenessOrca) TerminalList(context.Context) ([]orca.Terminal, error) {
	o.terminalCalls++
	return nil, nil
}
func (o *livenessOrca) WorkerStop(_ context.Context, request, dispatch string) (orca.Receipt, error) {
	want := scopedRequest(Ticket{Identifier: "ZII-861"}, Config{Unit: "executor", Terminal: "term_exec"}, "stop", 1)
	if request != want || dispatch != "ctx_one" {
		panic("wrong stop request or target")
	}
	o.stops++
	return nil, nil
}

// Use the Client's typed parsing path, then change dispatchId to match the executor.
// Recorded liveness tests keep all other fields; unobserved-turn tests override the stage detail explicitly.
func livenessFixture(t *testing.T, verdict, detail string) *livenessOrca {
	t.Helper()
	path, err := filepath.Abs("../../testdata/recorded/orca-worker-list-liveness.json")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat '"+path+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &orca.Client{Path: script}
	workers, err := client.Workers(context.Background(), orca.ListOptions{Run: "run_9714ea9b8576", IncludeRemote: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, worker := range workers {
		if (verdict == "" || worker.Projection.Liveness.Verdict == verdict) && (detail == "" || worker.Projection.Stage.Detail == detail) {
			worker.DispatchID = "ctx_one"
			return &livenessOrca{worker: worker}
		}
	}
	t.Fatalf("recorded Run run_9714ea9b8576 has no worker with verdict=%q detail=%q", verdict, detail)
	return nil
}

type livenessRecovery struct {
	recoveryService
	cancel context.CancelFunc
}

func (r *livenessRecovery) Correction(_ context.Context, req CorrectionRequest) error {
	r.requests = append(r.requests, req)
	r.cancel()
	return nil
}

func TestWaitFailsExitedWorker(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	o := livenessFixture(t, "exited", "")
	e.Orca = o
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &livenessRecovery{cancel: cancel}
	e.Recovery = r
	result, err := e.Execute(ctx, ticket)
	if !errors.Is(err, context.Canceled) || result.State.Phase != machine.PhaseCorrection || len(r.requests) != 1 || !strings.Contains(r.requests[0].Cause, "worker exited without worker_done") {
		t.Fatalf("state=%+v err=%v corrections=%+v", result.State, err, r.requests)
	}
	if o.stops != 0 || o.releases != 0 {
		t.Fatalf("stop=%d release=%d without worker report", o.stops, o.releases)
	}
	if o.taskCalls < 2 {
		t.Fatalf("task-list read %d times before exited failure, want at least two", o.taskCalls)
	}
	cp, err := readCheckpoint(e.Config, ticket)
	if err != nil || cp == nil || cp.Worktree != "/worktrees/ZII-886-executor" {
		t.Fatalf("checkpoint=%+v err=%v", cp, err)
	}
}

func TestWaitUnobservedPreservesCheckpointAcrossCancellation(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	e.Config.PollInterval = time.Second
	o := livenessFixture(t, "unverifiable", "")
	o.worker.Projection.Stage.Detail = "turn_start_unobserved"
	e.Orca = o
	now := e.Now()
	e.Now = func() time.Time { return now }
	sleeps := 0
	var original checkpoint
	e.Sleep = func(_ context.Context, interval time.Duration) error {
		if interval != e.Config.PollInterval {
			t.Fatalf("poll interval=%v", interval)
		}
		cp, err := readCheckpoint(e.Config, ticket)
		if err != nil || cp == nil {
			t.Fatalf("checkpoint=%+v err=%v", cp, err)
		}
		if sleeps == 0 {
			original = *cp
		} else if *cp != original {
			t.Fatalf("checkpoint changed: before=%+v after=%+v", original, cp)
		}
		sleeps++
		now = now.Add(6 * time.Minute)
		if sleeps >= 3 {
			return context.Canceled
		}
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := e.Start(context.Background(), ticket)
		if !errors.Is(err, context.Canceled) || o.stops != 0 || o.releases != 0 || result.State.Corrections != 0 {
			t.Fatalf("state=%+v err=%v stops=%d releases=%d", result.State, err, o.stops, o.releases)
		}
	}
	cp, err := readCheckpoint(e.Config, ticket)
	if err != nil || cp == nil || *cp != original || len(o.workerRequests) != 1 || len(o.runRequests) != 2 || o.runRequests[0] != o.runRequests[1] || o.polls != 4 {
		t.Fatalf("checkpoint=%+v err=%v starts=%v runs=%v polls=%d", cp, err, o.workerRequests, o.runRequests, o.polls)
	}
}

func TestWaitExitedAfterLateReport(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	o := livenessFixture(t, "exited", "")
	o.reportAt = 2
	e.Orca = o
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &livenessRecovery{cancel: cancel}
	e.Recovery = r
	result, err := e.Execute(ctx, ticket)
	if err != nil || result.State.Phase != machine.PhaseMergeQueue || result.State.Corrections != 0 || len(r.requests) != 0 || o.stops != 0 || o.taskCalls != 2 || o.polls != 1 {
		t.Fatalf("state=%+v err=%v corrections=%+v stops=%d task-list=%d worker-list=%d", result.State, err, r.requests, o.stops, o.taskCalls, o.polls)
	}
}

func TestWaitIgnoresUnverifiable(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	o := livenessFixture(t, "unverifiable", "")
	e.Orca = o
	now := e.Now()
	e.Now = func() time.Time { return now }
	e.Sleep = func(context.Context, time.Duration) error {
		now = now.Add(6 * time.Minute)
		if o.stops != 0 || o.releases != 0 {
			t.Fatalf("stop=%d release=%d before report", o.stops, o.releases)
		}
		if o.polls >= 3 {
			o.report = true
		}
		return nil
	}
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Phase != machine.PhaseMergeQueue || result.State.Corrections != 0 || result.State.Infra != 0 || o.stops != 0 || o.polls != 3 || o.releases != 1 {
		t.Fatalf("state=%+v err=%v polls=%d stops=%d releases=%d", result.State, err, o.polls, o.stops, o.releases)
	}
	if o.terminalCalls == 0 {
		t.Fatal("idle observation was not resumed with recovered checkpoint worktree")
	}
}

func TestWaitNeverStartedAfterLateReport(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	o := livenessFixture(t, "unverifiable", "")
	o.worker.Projection.Stage.Detail = "turn_start_unobserved"
	// A late report must settle the original dispatch after prolonged uncertainty.
	o.reportAt = 3
	e.Orca = o
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &livenessRecovery{cancel: cancel}
	e.Recovery = r
	now := e.Now()
	e.Now = func() time.Time { return now }
	e.Sleep = func(context.Context, time.Duration) error {
		now = now.Add(5 * time.Minute)
		return nil
	}
	result, err := e.Execute(ctx, ticket)
	if err != nil || result.State.Phase != machine.PhaseMergeQueue || result.State.Corrections != 0 || len(r.requests) != 0 || o.stops != 0 {
		t.Fatalf("state=%+v err=%v corrections=%+v stops=%d", result.State, err, r.requests, o.stops)
	}
}

// The scheduler reads this file as process-level liveness evidence: it must
// carry this process's pid, the Orca Run once known, and a fresh time.
func TestExecutorHeartbeatLiveness(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	e.Orca = livenessFixture(t, "exited", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Recovery = &livenessRecovery{cancel: cancel}
	path := orca.HeartbeatPath(e.Config.Root, e.Config.Repository, ticket.Identifier, "executor")
	start := time.Now()
	var ticks atomic.Int64
	// Every beat is one second later, so only a new write can advance At.
	now := func() time.Time { return start.Add(time.Duration(ticks.Add(1)) * time.Second) }
	hb, err := orca.StartHeartbeat(ctx, path, e.Config.Terminal, 10*time.Millisecond, now)
	if err != nil {
		t.Fatal(err)
	}
	e.Heartbeat = hb
	first, found, err := orca.ReadBeat(path)
	if err != nil || !found || first.PID != os.Getpid() || first.Run != "" || first.Terminal != e.Config.Terminal {
		t.Fatalf("first beat=%+v found=%v err=%v", first, found, err)
	}
	// Before any Run exists, only the periodic writer can replace the beat.
	for deadline := time.Now().Add(2 * time.Second); ; {
		beat, found, err := orca.ReadBeat(path)
		if err == nil && found && beat.Run == "" && beat.At.After(first.At) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no periodic beat after %+v: beat=%+v found=%v err=%v", first, beat, found, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	result, _ := e.Execute(ctx, ticket)
	beat, found, err := orca.ReadBeat(path)
	if err != nil || !found || result.RunID == "" || beat.Run != result.RunID || beat.PID != os.Getpid() || !beat.At.After(first.At) {
		t.Fatalf("beat=%+v found=%v err=%v run=%q", beat, found, err, result.RunID)
	}
	if beat.Judge(time.Now(), orca.DefaultHeartbeatTimeout, orca.PIDAlive) != orca.LivenessAlive {
		t.Fatalf("own beat not alive: %+v", beat)
	}
}

type checkpointRunOrca struct {
	livenessOrca
	path   string
	cancel context.CancelFunc
	seen   string
}

func (o *checkpointRunOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	if beat, found, err := orca.ReadBeat(o.path); err == nil && found {
		o.seen = beat.Run
	}
	o.cancel()
	return nil, ctx.Err()
}

// A process killed while recovering a checkpoint must already name the
// checkpoint Run, or the scheduler cannot fence that Run's dispatches.
func TestExecutorHeartbeatNamesCheckpointRun(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_ckpt", Task: "task_ckpt"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := orca.HeartbeatPath(e.Config.Root, e.Config.Repository, ticket.Identifier, "executor")
	hb, err := orca.StartHeartbeat(ctx, path, e.Config.Terminal, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Heartbeat = hb
	o := &checkpointRunOrca{path: path, cancel: cancel}
	e.Orca = o
	if _, err := e.Start(ctx, ticket); err == nil {
		t.Fatal("cancelled recovery succeeded")
	}
	if o.seen != "run_ckpt" {
		t.Fatalf("beat run during checkpoint task lookup = %q, want run_ckpt", o.seen)
	}
}

// recordedWorkers parses a recorded worker-list through the Client's typed path.
func recordedWorkers(t *testing.T, sample, run string) []orca.Worker {
	t.Helper()
	path, err := filepath.Abs("../../testdata/recorded/" + sample)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat '"+path+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workers, err := (&orca.Client{Path: script}).Workers(context.Background(), orca.ListOptions{Run: run, IncludeRemote: true})
	if err != nil {
		t.Fatal(err)
	}
	return workers
}

// The scheduler abandoned the dead executor's worker: Orca returns the task to
// blocked without a dispatch or report (orca-task-list-abandoned.json) and
// lists the worker abandoned (orca-worker-list-abandoned.json).
type abandonedOrca struct {
	fakeOrca
	worker orca.Worker
}

func (o *abandonedOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	return []orca.Task{{ID: "task_one", RunID: "run_one", Status: "blocked", Spec: "branch agent/ZII-861/executor"}}, nil
}
func (o *abandonedOrca) Workers(_ context.Context, opts orca.ListOptions) ([]orca.Worker, error) {
	if opts.Run != "run_one" || !opts.IncludeRemote {
		panic("wrong worker list scope")
	}
	return []orca.Worker{o.worker}, nil
}

// A restart after the reap resolves an uncertain worker start from the
// worker-list and sends the abandoned worker to correction instead of waiting.
func TestExecutorHeartbeatRecoversAbandonedPendingWorker(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	workers := recordedWorkers(t, "orca-worker-list-abandoned.json", "run_4eb9fdf9ecdc")
	if len(workers) != 1 || workers[0].TaskID != "task_467434b620d4" || workers[0].DispatchID != "ctx_ee6da2ada658" || workers[0].Projection.Stage.Worker != "abandoned" {
		t.Fatalf("recorded abandoned worker=%+v", workers)
	}
	worker := workers[0]
	worker.TaskID, worker.DispatchID = "task_one", "ctx_one"
	o := &abandonedOrca{worker: worker}
	e.Orca = o
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Pending: true, Worktree: "/worktrees/ZII-861-executor"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &livenessRecovery{cancel: cancel}
	e.Recovery = r
	e.Sleep = func(context.Context, time.Duration) error { t.Fatal("waited on an abandoned worker"); return nil }
	if _, err := e.Start(ctx, ticket); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if len(r.requests) != 1 || !strings.Contains(r.requests[0].Cause, "worker abandoned without worker_done") {
		t.Fatalf("corrections=%+v", r.requests)
	}
}

// A start the ledger rejects must not overwrite the beat of the executor that
// owns the reservation.
func TestExecutorHeartbeatStartsAfterAcceptedStart(t *testing.T) {
	e, _, l, _, _, ticket := fixture(t, "new-sha")
	l.comments = []string{"归属: 执行器 term=term_old run=run_old attempt=1"}
	e.HeartbeatPath = orca.HeartbeatPath(e.Config.Root, e.Config.Repository, ticket.Identifier, "executor")
	if _, err := e.Start(context.Background(), ticket); err == nil || !strings.Contains(err.Error(), "another terminal") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(e.HeartbeatPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected start wrote a heartbeat: %v", err)
	}
	l.comments = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Orca = livenessFixture(t, "exited", "")
	e.Recovery = &livenessRecovery{cancel: cancel}
	_, _ = e.Start(ctx, ticket)
	if beat, found, err := orca.ReadBeat(e.HeartbeatPath); err != nil || !found || beat.PID != os.Getpid() || beat.Terminal != e.Config.Terminal {
		t.Fatalf("accepted start beat=%+v found=%v err=%v", beat, found, err)
	}
}
