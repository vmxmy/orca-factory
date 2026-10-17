package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type memoryStore struct {
	record      *Record
	failRunning bool
}

func (s *memoryStore) Load(context.Context) (*Record, error) {
	if s.record == nil {
		return nil, nil
	}
	r := *s.record
	return &r, nil
}
func (s *memoryStore) Save(_ context.Context, r Record) error {
	if s.failRunning && r.Phase == "running" {
		s.failRunning = false
		return errors.New("write uncertain")
	}
	s.record = &r
	return nil
}

type fakeOrca struct {
	workers    orca.Receipt
	tasks      []orca.Task
	startCalls int
	store      *memoryStore
	starts     map[string]orca.StartOptions
	report     *orca.WorkerReport
	releases   int
	releaseErr error
	readErr    error
	status     string
	startErr   error
	// settled replaces tasks once a worker read happens, as when the dispatch
	// reports between the controller's task read and worker read.
	settled []orca.Task
	stops   int
	// stopped replaces workers once WorkerStop is called.
	stopped orca.Receipt
}

func (o *fakeOrca) RunCreate(context.Context, string, string) (orca.Receipt, error) {
	return orca.Receipt{"run": json.RawMessage(`{"id":"run_1"}`)}, nil
}
func (o *fakeOrca) RunUse(context.Context, string, string) (orca.Receipt, error) {
	return orca.Receipt{}, nil
}
func (o *fakeOrca) WorkerStart(_ context.Context, id string, opts orca.StartOptions) (orca.Receipt, error) {
	o.startCalls++
	if err := o.startErr; err != nil {
		o.startErr = nil
		return nil, err
	}
	if o.store.record == nil || o.store.record.Phase != "pending" {
		return nil, errors.New("dispatch preceded durable intent")
	}
	if o.starts == nil {
		o.starts = map[string]orca.StartOptions{}
	}
	if old, ok := o.starts[id]; ok && old != opts {
		return nil, errors.New("request replay changed payload")
	}
	o.starts[id] = opts
	return orca.Receipt{"task": json.RawMessage(`{"id":"task_1"}`), "dispatch": json.RawMessage(`{"id":"ctx_1"}`)}, nil
}
func (o *fakeOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	if o.tasks != nil {
		return o.tasks, nil
	}
	if o.readErr != nil {
		return nil, o.readErr
	}
	if o.report == nil {
		state := o.status
		if state == "" {
			state = "running"
		}
		return []orca.Task{{ID: "task_1", RunID: "run_1", DispatchID: "ctx_1", Status: state, Spec: "Act as resolver"}}, nil
	}
	b, _ := json.Marshal(o.report)
	return []orca.Task{{ID: "task_1", RunID: "run_1", DispatchID: "ctx_1", Status: "completed", Result: b}}, nil
}
func (o *fakeOrca) WorkerRelease(context.Context, string, string) (orca.Receipt, error) {
	o.releases++
	return orca.Receipt{}, o.releaseErr
}
func setupController() (*Controller, *memoryStore, *fakeOrca) {
	s := &memoryStore{}
	o := &fakeOrca{store: s}
	return &Controller{Store: s, Orca: o, Ticket: "ENG-1", Repo: "/repo", Spec: "Act as resolver", Terminal: "first", Agent: "codex", Model: "model", Effort: "medium", Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}, s, o
}

// A dispatch the maintenance gate refused never reached Orca, so a later
// controller starts a fresh run instead of awaiting a task that cannot exist.
func TestRefusedDispatchStartsFreshRun(t *testing.T) {
	c, s, o := setupController()
	path := filepath.Join(t.TempDir(), "resolver.heartbeat.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hb, err := orca.StartHeartbeat(ctx, path, "first", time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Heartbeat = hb
	beatRun := func() string {
		beat, found, err := orca.ReadBeat(path)
		if err != nil || !found {
			t.Fatalf("beat found=%v err=%v", found, err)
		}
		return beat.Run
	}
	o.startErr = fmt.Errorf("worker start: %w", maintenance.ErrRefused)
	if _, err := c.Step(context.Background()); !errors.Is(err, maintenance.ErrRefused) {
		t.Fatalf("refusal not reported: %v", err)
	}
	if s.record.Phase != "pending" || s.record.Run != "" || beatRun() != "" {
		t.Fatalf("refused dispatch kept run: %+v beat run=%q", s.record, beatRun())
	}
	o.tasks = []orca.Task{}
	c.Terminal = "second"
	p, err := c.Step(context.Background())
	if err != nil || p.Phase != "running" || o.startCalls != 2 {
		t.Fatalf("replacement did not dispatch: phase=%v starts=%d err=%v", p, o.startCalls, err)
	}
	if beatRun() != "run_1" {
		t.Fatalf("beat run=%q, want the resolver Run", beatRun())
	}
}

func TestLostDispatchCheckpointReconcilesSameRequest(t *testing.T) {
	c, s, o := setupController()
	s.failRunning = true
	if _, err := c.Step(context.Background()); err == nil {
		t.Fatal("expected unconfirmed checkpoint")
	}
	c.Spec = "updated role on restart" // An in-flight request must retain its original payload.
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(o.starts) != 1 || s.record.Phase != "running" {
		t.Fatalf("duplicate/missing resolver: starts=%v state=%+v", o.starts, s.record)
	}
	for _, opts := range o.starts {
		if opts.Spec != "Act as resolver" || opts.Agent != "codex" || opts.Model != "model" || opts.Worktree != "new-top-level" {
			t.Fatalf("wrong launch %+v", opts)
		}
	}
}
func TestUnknownTaskReadNeverReleasesOrStartsAgain(t *testing.T) {
	c, _, o := setupController()
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.readErr = errors.New("unavailable")
	if _, err := c.Step(context.Background()); err == nil {
		t.Fatal("expected read error")
	}
	if o.releases != 0 || len(o.starts) != 1 {
		t.Fatal("unknown liveness authorized mutation")
	}
}
func TestReportIsDurableBeforeRelease(t *testing.T) {
	c, s, o := setupController()
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.report = &orca.WorkerReport{Provenance: "worker_report", Outcome: "succeeded", Body: `RESOLUTION {"action":"done","pr":12,"evidence":"gate and checks green"}`}
	r, err := c.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != "result" || s.record.Result != o.report.Body || o.releases != 1 {
		t.Fatalf("report not settled: %+v releases=%d", r, o.releases)
	}
	if _, err = c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o.releases != 1 || len(o.starts) != 1 {
		t.Fatal("completed resolver replayed")
	}
}
func TestFailedAgentReportIsHandedToScheduler(t *testing.T) {
	c, s, o := setupController()
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.report = &orca.WorkerReport{Provenance: "worker_report", Outcome: "failed", Body: "dependency unavailable"}
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.record.Phase != "result" || s.record.Outcome != "failed" || s.record.Result != "dependency unavailable" || !s.record.RetryAt.IsZero() || o.releases != 1 {
		t.Fatalf("failure not handed to scheduler: %+v", s.record)
	}
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(o.starts) != 1 {
		t.Fatal("failure immediately redispatched")
	}
}
func TestFailedTaskWithoutReportRetriesInsteadOfWaitingForever(t *testing.T) {
	c, s, o := setupController()
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.status = "failed"
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.record.Phase != "result" || s.record.Outcome != "failed" || o.releases != 1 {
		t.Fatalf("failed task stuck: %+v", s.record)
	}
}

func TestRecordedTaskWithoutDispatchIsAccepted(t *testing.T) {
	c, s, o := setupController()
	raw, err := os.ReadFile("../../testdata/recorded/orca-task-list.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result struct {
			Tasks []orca.Task `json:"tasks"`
		} `json:"result"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	task := envelope.Result.Tasks[0]
	if task.DispatchID != "" {
		t.Fatal("recording must exercise absent dispatch identity")
	}
	s.record = &Record{Version: 1, Attempt: 1, Phase: "running", Run: task.RunID, Task: task.ID, Dispatch: "known-from-start", Spec: c.Spec, Agent: c.Agent}
	o.tasks = envelope.Result.Tasks
	if _, err = c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.record.Phase != "result" {
		t.Fatalf("real report ignored: %+v", s.record)
	}
}
func TestReplacementControllerReconcilesLostWorkerReceipt(t *testing.T) {
	c, s, o := setupController()
	s.failRunning = true
	if _, err := c.Step(context.Background()); err == nil {
		t.Fatal("expected lost save")
	}
	c.Terminal = "replacement"
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o.startCalls != 1 || s.record.Phase != "running" {
		t.Fatalf("replacement replayed caller-bound mutation: starts=%d record=%+v", o.startCalls, s.record)
	}
}

func (o *fakeOrca) WorkerList(_ context.Context, opts orca.ListOptions) (orca.Receipt, error) {
	if o.settled != nil {
		o.tasks = o.settled
	}
	if o.workers != nil {
		return o.workers, nil
	}
	return orca.Receipt{"scope": json.RawMessage(`{"run":"run_1"}`), "page": json.RawMessage(`{"hasMore":false}`), "workers": json.RawMessage(`[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","resource":{"releaseState":"active"}}]`)}, nil
}

func TestRecordedWorkerAssociationRecoversReplacement(t *testing.T) {
	c, s, o := setupController()
	raw, err := os.ReadFile("../../testdata/recorded/orca-resolver-worker-list.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result orca.Receipt `json:"result"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	o.workers = envelope.Result
	var rows []workerIdentity
	if err = json.Unmarshal(o.workers["workers"], &rows); err != nil {
		t.Fatal(err)
	}
	row := rows[0]
	s.record = &Record{Version: 1, Attempt: 1, Phase: "pending", Creator: "old-terminal", Run: row.Run, Spec: c.Spec, Agent: c.Agent}
	o.tasks = []orca.Task{{ID: row.Task, RunID: row.Run, Spec: c.Spec, Status: "running"}}
	if _, err = c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.record.Dispatch != row.Dispatch || o.startCalls != 0 {
		t.Fatalf("recorded association ignored: %+v starts=%d", s.record, o.startCalls)
	}
}

// An Orca restart drops the dispatch and returns the task to ready; the round
// ends as failed for the scheduler's retry rules instead of polling forever.
func TestLostDispatchEndsRoundAsFailed(t *testing.T) {
	for _, workers := range []string{`[]`, `[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","dispatchStatus":"failed"}]`} {
		c, s, o := setupController()
		if _, err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		o.tasks = []orca.Task{{ID: "task_1", RunID: "run_1", Status: "ready", Spec: "Act as resolver"}}
		o.workers = orca.Receipt{"scope": json.RawMessage(`{"run":"run_1"}`), "page": json.RawMessage(`{"hasMore":false}`), "workers": json.RawMessage(workers)}
		p, err := c.Step(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if p.Phase != "result" || s.record.Phase != "result" || s.record.Outcome != "failed" || s.record.Result != "dispatch lost: task task_1 is ready without dispatch ctx_1" {
			t.Fatalf("lost dispatch not settled with workers %s: %+v", workers, s.record)
		}
		if o.releases != 0 || o.startCalls != 1 {
			t.Fatalf("lost dispatch released or replayed: releases=%d starts=%d", o.releases, o.startCalls)
		}
	}
}

func TestCanceledDispatchIsLost(t *testing.T) {
	c, s, o := setupController()
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.tasks = []orca.Task{{ID: "task_1", RunID: "run_1", Status: "ready", Spec: "Act as resolver"}}
	o.workers = orca.Receipt{"scope": json.RawMessage(`{"run":"run_1"}`), "page": json.RawMessage(`{"hasMore":false}`), "workers": json.RawMessage(`[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","dispatchStatus":"canceled"}]`)}
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.record.Outcome != "failed" || s.record.Result != "dispatch lost: task task_1 is ready without dispatch ctx_1" {
		t.Fatalf("canceled dispatch not judged lost: %+v", s.record)
	}
}

func TestReportBetweenReadsIsNotLost(t *testing.T) {
	c, s, o := setupController()
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.tasks = []orca.Task{{ID: "task_1", RunID: "run_1", Status: "ready", Spec: "Act as resolver"}}
	o.settled = []orca.Task{{ID: "task_1", RunID: "run_1", DispatchID: "ctx_1", Status: "completed", Result: json.RawMessage(`{"provenance":"worker_report","outcome":"succeeded","body":"fixed"}`)}}
	o.workers = orca.Receipt{"scope": json.RawMessage(`{"run":"run_1"}`), "page": json.RawMessage(`{"hasMore":false}`), "workers": json.RawMessage(`[]`)}
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.record.Outcome != "succeeded" || s.record.Result != "fixed" {
		t.Fatalf("dispatch that reported between reads judged lost: %+v", s.record)
	}
}

// A dispatch queued behind a busy terminal is also ready without a task
// dispatch_id (recorded receipt); its listed worker keeps the round waiting.
func TestRecordedQueuedReadyDispatchIsNotLost(t *testing.T) {
	c, s, o := setupController()
	raw, err := os.ReadFile("../../testdata/recorded/orca-worker-start-reuse-busy-queued.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result struct {
			Run      string `json:"runId"`
			Task     string `json:"taskId"`
			Dispatch string `json:"dispatchId"`
			State    string `json:"state"`
		} `json:"result"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	r := envelope.Result
	if r.State != "ready" {
		t.Fatalf("recorded queued dispatch is %q", r.State)
	}
	s.record = &Record{Version: 1, Attempt: 1, Phase: "running", Creator: c.Terminal, Run: r.Run, Task: r.Task, Dispatch: r.Dispatch, Spec: c.Spec, Agent: c.Agent}
	o.tasks = []orca.Task{{ID: r.Task, RunID: r.Run, Status: r.State, Spec: c.Spec}}
	row, err := json.Marshal([]workerIdentity{{Task: r.Task, Run: r.Run, Dispatch: r.Dispatch}})
	if err != nil {
		t.Fatal(err)
	}
	o.workers = orca.Receipt{"scope": json.RawMessage(`{"run":"` + r.Run + `"}`), "page": json.RawMessage(`{"hasMore":false}`), "workers": row}
	if p, err := c.Step(context.Background()); err != nil || p.Phase != "running" {
		t.Fatalf("queued dispatch judged lost: %+v err=%v", p, err)
	}
	if s.record.Phase != "running" || s.record.Outcome != "" || o.releases != 0 || o.startCalls != 0 {
		t.Fatalf("queued dispatch changed the round: %+v", s.record)
	}
}

func TestIncompleteReadNeverJudgesDispatchLost(t *testing.T) {
	c, s, o := setupController()
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.readErr = errors.New("incomplete task list")
	if _, err := c.Step(context.Background()); err == nil {
		t.Fatal("incomplete task read accepted")
	}
	o.readErr = nil
	o.tasks = []orca.Task{{ID: "task_1", RunID: "run_1", Status: "ready", Spec: "Act as resolver"}}
	o.workers = orca.Receipt{"scope": json.RawMessage(`{"run":"run_1"}`), "page": json.RawMessage(`{"hasMore":true}`), "workers": json.RawMessage(`[]`)}
	if _, err := c.Step(context.Background()); err == nil {
		t.Fatal("incomplete worker read accepted")
	}
	if s.record.Phase != "running" || s.record.Outcome != "" || o.releases != 0 || o.startCalls != 1 {
		t.Fatalf("incomplete evidence changed the round: %+v", s.record)
	}
}

func TestReplacementCannotRedispatchWhenOriginalRequestIsUnknown(t *testing.T) {
	c, s, o := setupController()
	s.record = &Record{Version: 1, Attempt: 1, Phase: "pending", Creator: "old-terminal", Run: "run_1", Spec: c.Spec, Agent: c.Agent}
	o.tasks = []orca.Task{}
	if _, err := c.Step(context.Background()); err == nil {
		t.Fatal("unknown old request authorized mutation")
	}
	if o.startCalls != 0 || o.releases != 0 {
		t.Fatal("unconfirmed request replayed")
	}
}

// The scheduler abandoned a dead resolver's worker: Orca returns the task to
// blocked with no dispatch or report and lists the worker abandoned. The
// replacement controller settles it as failed instead of polling forever.
func TestReplacementControllerSettlesAbandonedWorker(t *testing.T) {
	c, s, o := setupController()
	var tasks struct {
		Result struct {
			Tasks []orca.Task `json:"tasks"`
		} `json:"result"`
	}
	var workers struct {
		Result orca.Receipt `json:"result"`
	}
	for sample, into := range map[string]any{"orca-task-list-abandoned.json": &tasks, "orca-worker-list-abandoned.json": &workers} {
		raw, err := os.ReadFile("../../testdata/recorded/" + sample)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, into); err != nil {
			t.Fatal(err)
		}
	}
	task := tasks.Result.Tasks[0]
	if task.Status != "blocked" || task.DispatchID != "" {
		t.Fatalf("recording must exercise an abandoned task: %+v", task)
	}
	s.record = &Record{Version: 1, Attempt: 1, Phase: "running", Run: task.RunID, Task: task.ID, Dispatch: "ctx_ee6da2ada658", Spec: c.Spec, Agent: c.Agent}
	o.tasks, o.workers = tasks.Result.Tasks, workers.Result
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.record.Phase != "result" || s.record.Outcome != "failed" || o.releases != 1 {
		t.Fatalf("abandoned worker not settled: %+v releases=%d", s.record, o.releases)
	}
}

// recordedReleaseUnknown replays the real OF-22 worker-release receipt (ok:true,
// exit 1) through the Orca client, so the error has its production shape.
func recordedReleaseUnknown(t *testing.T) error {
	t.Helper()
	fixture, err := filepath.Abs("../../testdata/recorded/orca-worker-release-unknown.json")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat '"+fixture+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = (&orca.Client{Path: script}).WorkerRelease(context.Background(), orca.RequestID("ENG-1", "resolver", "release", 1), "ctx_1")
	return err
}

// After the report is persisted, release_unknown only leaves an unprovable
// terminal behind: the controller warns and advances. Other release errors
// still hold the settled phase.
func TestSettledReleaseUnknownAdvancesToResult(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		phase string
	}{
		{"release_unknown", recordedReleaseUnknown(t), "result"},
		{"rejected", &orca.RejectedError{Code: "invalid_argument"}, "settled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s, o := setupController()
			s.record = &Record{Version: 1, Attempt: 1, Phase: "settled", Run: "run_1", Task: "task_1", Dispatch: "ctx_1", Spec: c.Spec, Agent: c.Agent, Result: "done", Outcome: "succeeded"}
			o.releaseErr = tc.err
			var logs bytes.Buffer
			ctx := runx.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
			_, err := c.Step(ctx)
			if s.record.Phase != tc.phase || o.releases != 1 || (err == nil) != (tc.phase == "result") {
				t.Fatalf("phase=%s releases=%d err=%v", s.record.Phase, o.releases, err)
			}
			if tc.phase == "result" && (!strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "dispatch=ctx_1") || !strings.Contains(logs.String(), "reason=release_unknown")) {
				t.Fatalf("logs=%q", logs.String())
			}
		})
	}
}

func (o *fakeOrca) WorkerStop(context.Context, string, string) (orca.Receipt, error) {
	o.stops++
	if o.stopped != nil {
		o.workers = o.stopped
	}
	return orca.Receipt{}, nil
}

func workerRows(run, rows string) orca.Receipt {
	return orca.Receipt{"scope": json.RawMessage(`{"run":"` + run + `"}`), "page": json.RawMessage(`{"hasMore":false}`), "workers": json.RawMessage(rows)}
}

// ZCP-142: the brief stayed in the agent's input box, so Orca never saw the
// turn start and the resolver held its maintenance session for 78 minutes.
// The unobserved row is synthetic (values from that incident's worker-list);
// the started row is recorded.
func TestStartUnobservedWorkerIsStoppedAfterWindow(t *testing.T) {
	unobserved := workerRows("run_1", `[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","workerState":"start_unknown","dispatchStatus":"pending","projection":{"stage":{"detail":"turn_start_unobserved"},"liveness":{"verdict":"unverifiable"}}}]`)
	stopped := workerRows("run_1", `[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","workerState":"stopped","dispatchStatus":"failed","projection":{"stage":{"detail":"process_stopped"},"liveness":{"verdict":"exited"}}}]`)
	steps := func(t *testing.T, c *Controller, now *time.Time) {
		t.Helper()
		for _, elapsed := range []time.Duration{0, StartUnobservedWindow - time.Second, time.Second} {
			*now = now.Add(elapsed)
			if _, err := c.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("stops and waits once the window passes", func(t *testing.T) {
		c, s, o := setupController()
		now := c.Now()
		c.Now = func() time.Time { return now }
		if _, err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		o.workers, o.stopped = unobserved, stopped
		steps(t, c, &now)
		if o.stops != 1 || s.record.Phase != "waiting" || s.record.Outcome != "failed" || !s.record.RetryAt.Equal(now) || o.releases != 0 || o.startCalls != 1 {
			t.Fatalf("unstarted dispatch not stopped into waiting: stops=%d record=%+v", o.stops, s.record)
		}
	})
	t.Run("keeps the round when the stop is not confirmed", func(t *testing.T) {
		c, s, o := setupController()
		now := c.Now()
		c.Now = func() time.Time { return now }
		if _, err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		o.workers = unobserved
		now = now.Add(StartUnobservedWindow)
		for range 2 {
			if _, err := c.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		now = now.Add(StartUnobservedWindow)
		if _, err := c.Step(context.Background()); err == nil || o.stops != 1 || s.record.Phase != "running" {
			t.Fatalf("unconfirmed stop ended the round: err=%v stops=%d record=%+v", err, o.stops, s.record)
		}
	})
	t.Run("a started turn keeps waiting", func(t *testing.T) {
		c, s, o := setupController()
		raw, err := os.ReadFile("../../testdata/recorded/orca-worker-list-turn-started.json")
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Result orca.Receipt `json:"result"`
		}
		if err = json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		var rows []workerIdentity
		if err = json.Unmarshal(envelope.Result["workers"], &rows); err != nil {
			t.Fatal(err)
		}
		row := rows[0]
		o.workers = envelope.Result
		s.record = &Record{Version: 1, Attempt: 1, Phase: "running", Creator: c.Terminal, Run: row.Run, Task: row.Task, Dispatch: row.Dispatch, Spec: c.Spec, Agent: c.Agent}
		o.tasks = []orca.Task{{ID: row.Task, RunID: row.Run, DispatchID: row.Dispatch, Status: "dispatched"}}
		now := c.Now()
		c.Now = func() time.Time { return now }
		steps(t, c, &now)
		if o.stops != 0 || s.record.Phase != "running" {
			t.Fatalf("started turn stopped: stops=%d record=%+v", o.stops, s.record)
		}
	})
}

// ZCP-141: maintenance refused the dispatch after run-create, leaving a Run
// with no task or worker. Complete empty reads prove nothing was dispatched,
// so the round ends as waiting instead of awaiting evidence forever.
func TestResolverEmptyRunIsNotUncertain(t *testing.T) {
	var tasks struct {
		Result struct {
			RunID string      `json:"runId"`
			Tasks []orca.Task `json:"tasks"`
		} `json:"result"`
	}
	var workers struct {
		Result orca.Receipt `json:"result"`
	}
	for path, out := range map[string]any{"orca-task-list-empty-run.json": &tasks, "orca-worker-list-empty-run.json": &workers} {
		raw, err := os.ReadFile("../../testdata/recorded/" + path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
	run := tasks.Result.RunID
	pending := func(c *Controller) *Record {
		return &Record{Version: 1, Attempt: 1, Phase: "pending", Creator: "old-terminal", Run: run, Spec: c.Spec, Agent: c.Agent}
	}

	c, s, o := setupController()
	s.record, o.tasks, o.workers = pending(c), tasks.Result.Tasks, workers.Result
	p, err := c.Step(context.Background())
	if err != nil || p.Phase != "waiting" || s.record.Phase != "waiting" || o.startCalls != 0 {
		t.Fatalf("empty run not settled as never dispatched: record=%+v starts=%d err=%v", s.record, o.startCalls, err)
	}

	c, s, o = setupController()
	s.record, o.tasks = pending(c), tasks.Result.Tasks
	o.workers = orca.Receipt{"scope": json.RawMessage(`{"run":"` + run + `"}`), "page": json.RawMessage(`{"hasMore":true}`), "workers": json.RawMessage(`[]`)}
	if _, err = c.Step(context.Background()); err == nil || s.record.Phase != "pending" || o.startCalls != 0 {
		t.Fatalf("incomplete worker read ended the round: record=%+v starts=%d err=%v", s.record, o.startCalls, err)
	}
}
