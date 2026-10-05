package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	stopErr                                error
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
	return nil, o.stopErr
}

// Use the Client's typed parsing path, then change dispatchId to match the executor.
// Recorded liveness tests keep all other fields; the turn-start timeout test overrides them explicitly.
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

func TestWaitStopsWorkerThatNeverStarted(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	o := livenessFixture(t, "unverifiable", "")
	// 录制 Run 没有 turn_start_unobserved，detail 为合成值
	o.worker.Projection.Stage.Detail = "turn_start_unobserved"
	e.Orca = o
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &livenessRecovery{cancel: cancel}
	e.Recovery = r
	now := e.Now()
	e.Now = func() time.Time { return now }
	sleeps := 0
	e.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		switch sleeps {
		case 1:
			if o.stops != 0 {
				t.Fatalf("stop before 4m59s: %d", o.stops)
			}
			now = now.Add(4*time.Minute + 59*time.Second)
			o.worker.Projection.Liveness.Verdict = ""
		case 2:
			if o.stops != 0 {
				t.Fatalf("stop at 4m59s: %d", o.stops)
			}
			now = now.Add(time.Second)
			o.worker.Projection.Liveness.Verdict = "unverifiable"
		default:
			t.Fatal("worker did not stop after five minutes")
		}
		return nil
	}
	result, err := e.Execute(ctx, ticket)
	if !errors.Is(err, context.Canceled) || o.stops != 1 || result.State.Phase != machine.PhaseCorrection || len(r.requests) != 1 || !strings.Contains(r.requests[0].Cause, "worker never started its turn") {
		t.Fatalf("state=%+v err=%v stops=%d corrections=%+v", result.State, err, o.stops, r.requests)
	}
	if o.releases != 0 {
		t.Fatalf("release without report: %d", o.releases)
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

func TestWaitStopFailureIsNotRetried(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	o := livenessFixture(t, "unverifiable", "")
	o.worker.Projection.Stage.Detail = "turn_start_unobserved"
	o.stopErr = errors.New("worker-stop failed")
	e.Orca = o
	now := e.Now()
	e.Now = func() time.Time { return now }
	e.Sleep = func(context.Context, time.Duration) error {
		now = now.Add(6 * time.Minute)
		return nil
	}
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerHuman || o.polls < 3 {
		t.Fatalf("read retries were not exercised: state=%+v err=%v polls=%d", result.State, err, o.polls)
	}
	if o.stops != 1 || result.State.Phase == machine.PhaseCorrection || result.State.Corrections != 0 {
		t.Fatalf("state=%+v stops=%d; failed stop must not be replayed or cause correction", result.State, o.stops)
	}
}
