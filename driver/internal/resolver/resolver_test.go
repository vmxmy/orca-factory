package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
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
	stopped                    orca.Receipt
	terminals                  []orca.Terminal
	terminalErr                error
	terminalReport, stopReport *orca.WorkerReport
	// legacyTasks and uncountedTasks shape the metadata TaskListing reports.
	legacyTasks, uncountedTasks bool
	screen                      orca.Receipt
	screenReads                 int
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
func (o *fakeOrca) TaskListing(ctx context.Context, run string) (orca.TaskListing, error) {
	tasks, err := o.TaskList(ctx, run)
	return orca.TaskListing{Tasks: tasks, LegacyReadOnly: o.legacyTasks, Counted: !o.uncountedTasks}, err
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
	if s.record.Phase != "waiting" || s.record.Outcome != "undispatched" || beatRun() != "" {
		t.Fatalf("refused dispatch did not end the round: %+v beat run=%q", s.record, beatRun())
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
	if o.stopReport != nil {
		o.report = o.stopReport
	}
	if o.stopped != nil {
		o.workers = o.stopped
	}
	return orca.Receipt{}, nil
}

func workerRows(run, rows string) orca.Receipt {
	return orca.Receipt{"scope": json.RawMessage(`{"run":"` + run + `"}`), "page": json.RawMessage(`{"hasMore":false}`), "workers": json.RawMessage(rows)}
}

func (o *fakeOrca) Workers(ctx context.Context, opts orca.ListOptions) ([]orca.Worker, error) {
	receipt, err := o.WorkerList(ctx, opts)
	if err != nil {
		return nil, err
	}
	var rows []orca.Worker
	err = json.Unmarshal(receipt["workers"], &rows)
	return rows, err
}

func (o *fakeOrca) TerminalList(context.Context) ([]orca.Terminal, error) {
	if o.terminalReport != nil {
		o.report = o.terminalReport
	}
	return o.terminals, o.terminalErr
}

func TestResolverProgressEvidence(t *testing.T) {
	for _, scenario := range []string{"output advances", "unknown output", "read error", "unconfirmed stop", "report before stop", "report during stop"} {
		t.Run(scenario, func(t *testing.T) {
			c, s, o := setupController()
			now := c.Now()
			c.Now = func() time.Time { return now }
			if _, err := c.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			o.workers = workerRows("run_1", `[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","dispatchStatus":"dispatched","agentTerminalHandle":"term_worker"}]`)
			o.stopped = workerRows("run_1", `[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","dispatchStatus":"failed"}]`)
			o.terminals = []orca.Terminal{{Handle: "term_worker", LastOutputAt: now.UnixMilli()}}
			if _, err := c.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			now = now.Add(15 * time.Minute)
			report := &orca.WorkerReport{Provenance: "worker_report", Outcome: "succeeded", Body: "finished"}
			switch scenario {
			case "output advances":
				o.terminals[0].LastOutputAt = now.UnixMilli()
			case "unknown output":
				o.terminals[0].LastOutputAt = 0
			case "read error":
				o.terminalErr = errors.New("read unavailable")
			case "unconfirmed stop":
				o.stopped = nil
			case "report before stop":
				o.terminalReport = report
			case "report during stop":
				o.stopReport = report
			}
			p, err := c.Step(context.Background())
			wantError := scenario == "read error" || scenario == "unconfirmed stop"
			if (err != nil) != wantError {
				t.Fatalf("err=%v wantError=%v", err, wantError)
			}
			if strings.HasPrefix(scenario, "report") {
				if p.Phase != "result" || p.Outcome != "succeeded" || p.Result != "finished" || o.releases != 1 {
					t.Fatalf("late report lost: p=%+v releases=%d", p, o.releases)
				}
			} else if s.record.Phase != "running" || o.releases != 0 {
				t.Fatalf("unproven failure settled: p=%+v releases=%d", s.record, o.releases)
			}
			wantStops := 0
			if scenario == "unconfirmed stop" || scenario == "report during stop" {
				wantStops = 1
			}
			if o.stops != wantStops {
				t.Fatalf("stops=%d want=%d", o.stops, wantStops)
			}
			if scenario == "output advances" || scenario == "unknown output" {
				o.terminals[0].LastOutputAt = now.UnixMilli()
				if _, err := c.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
				now = now.Add(15*time.Minute - time.Second)
				if p, err := c.Step(context.Background()); err != nil || p.Phase != "running" || o.stops != 0 {
					t.Fatalf("fresh output window not respected: p=%+v stops=%d err=%v", p, o.stops, err)
				}
				now = now.Add(time.Second)
				if p, err := c.Step(context.Background()); err != nil || p.Phase != "result" || p.Outcome != "failed" || o.stops != 1 {
					t.Fatalf("fresh window did not expire: p=%+v stops=%d err=%v", p, o.stops, err)
				}
			}
		})
	}
}

func recordedProgress(t *testing.T) (orca.Receipt, []orca.Terminal) {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/recorded/orca-resolver-progress-worker-list.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("../../testdata/recorded/orca-resolver-progress-terminal-list.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat <<'RECORDED'\n"+string(raw)+"\nRECORDED\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := &orca.Client{Path: path}
	terminals, err := client.TerminalList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return envelope.Result, terminals
}

func TestResolverRoundWithoutProgressFailsAfterWindow(t *testing.T) {
	const (
		run      = "run_8e97bfed638e"
		task     = "task_5379745a42bf"
		dispatch = "ctx_89446d008d30"
		terminal = "term_540930fb-3f80-4e35-ae0d-68080b6abad8"
		output   = 1794988230860
	)
	workers, terminals := recordedProgress(t)
	for _, top := range []bool{false, true} {
		for _, external := range []bool{false, true} {
			for _, screened := range []bool{false, true} {
				t.Run(fmt.Sprintf("top=%v/external-stop=%v/model-screen=%v", top, external, screened), func(t *testing.T) {
					c, s, o := setupController()
					now := time.UnixMilli(output)
					c.Now = func() time.Time { return now }
					if screened {
						// OF-594: a screen without a model error keeps the ordinary window.
						c.Models = &modelLog{}
						c.Probe = func(context.Context, modelhealth.Ref) modelhealth.Status {
							panic("probed a screen without a model error")
						}
						o.screen = recordedScreen(t, []string{"• Running make test", "", "› "})
					}
					s.record = &Record{Version: 1, Attempt: 1, Phase: "running", Creator: c.Terminal, Run: run, Task: task, Dispatch: dispatch, Spec: c.Spec, Agent: c.Agent, Top: top, TopLaunchAccepted: top}
					o.tasks = []orca.Task{{ID: task, RunID: run, DispatchID: dispatch, Status: "dispatched"}}
					o.workers, o.terminals = workers, terminals
					// The stopped row is synthetic; its field shape comes from orca-worker-list-liveness.json.
					o.stopped = workerRows(run, fmt.Sprintf(`[{"runId":%q,"taskId":%q,"dispatchId":%q,"dispatchStatus":"failed","workerState":"stopped"}]`, run, task, dispatch))
					for _, elapsed := range []time.Duration{0, 15*time.Minute - time.Second} {
						now = now.Add(elapsed)
						p, err := c.Step(context.Background())
						if err != nil || p.Phase != "running" || o.stops != 0 {
							t.Fatalf("round ended before window: p=%+v stops=%d err=%v", p, o.stops, err)
						}
						if s.record.LastOutputAt != output {
							t.Fatalf("last output=%d, want recorded terminal %s output %d", s.record.LastOutputAt, terminal, output)
						}
					}
					if external {
						o.workers = o.stopped
					} else {
						now = now.Add(2 * time.Second)
					}
					// Recreate the controller to exercise the persisted observation window.
					restarted := *c
					p, err := restarted.Step(context.Background())
					if err != nil || p.Phase != "result" || p.Outcome != "failed" || s.record.Phase != "result" || o.releases != 1 {
						t.Fatalf("round did not fail: p=%+v releases=%d err=%v", p, o.releases, err)
					}
					wantStops := 1
					if external {
						wantStops = 0
					}
					if o.stops != wantStops || o.startCalls != 0 {
						t.Fatalf("stops=%d starts=%d", o.stops, o.startCalls)
					}
					if screened && (o.screenReads == 0 || len(c.Models.(*modelLog).statuses) != 0) {
						t.Fatalf("screen reads=%d registry writes=%+v", o.screenReads, c.Models)
					}
					if top {
						var result Resolution
						if err := json.Unmarshal([]byte(p.Result), &result); err != nil || result.Action != "escalate" || result.Reason != "终审模型未获响应，不更换模型" || p.TopModelAccepted {
							t.Fatalf("missing top-model failure: result=%s accepted=%v err=%v", p.Result, p.TopModelAccepted, err)
						}
					}
				})
			}
		}
	}
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
	if err != nil || p.Phase != "waiting" || s.record.Phase != "waiting" || s.record.Outcome != "undispatched" || o.startCalls != 0 {
		t.Fatalf("empty run not settled as never dispatched: record=%+v starts=%d err=%v", s.record, o.startCalls, err)
	}

	c, s, o = setupController()
	s.record, o.tasks = pending(c), tasks.Result.Tasks
	o.workers = orca.Receipt{"scope": json.RawMessage(`{"run":"` + run + `"}`), "page": json.RawMessage(`{"hasMore":true}`), "workers": json.RawMessage(`[]`)}
	if _, err = c.Step(context.Background()); err == nil || s.record.Phase != "pending" || o.startCalls != 0 {
		t.Fatalf("incomplete worker read ended the round: record=%+v starts=%d err=%v", s.record, o.startCalls, err)
	}

	c, s, o = setupController()
	s.record, o.tasks, o.workers = pending(c), tasks.Result.Tasks, workers.Result
	o.settled = []orca.Task{{ID: "task_1", RunID: run, Status: "pending", Spec: c.Spec}}
	if _, err = c.Step(context.Background()); err == nil || s.record.Phase != "pending" || o.startCalls != 0 {
		t.Fatalf("task created between reads ended the round: record=%+v starts=%d err=%v", s.record, o.startCalls, err)
	}
}

// An empty task list proves no dispatch only when the read is current and
// complete: a legacy read-only run or an empty list without a count is not.
func TestResolverEmptyRunRejectsLegacyOrUncountedTasks(t *testing.T) {
	for _, scenario := range []string{"legacy", "uncounted"} {
		t.Run(scenario, func(t *testing.T) {
			c, s, o := setupController()
			s.record = &Record{Version: 1, Attempt: 1, Phase: "pending", Creator: "old-terminal", Run: "run_1", Spec: c.Spec, Agent: c.Agent}
			o.tasks = []orca.Task{}
			o.workers = orca.Receipt{"scope": json.RawMessage(`{"run":"run_1"}`), "page": json.RawMessage(`{"hasMore":false}`), "workers": json.RawMessage(`[]`)}
			o.legacyTasks, o.uncountedTasks = scenario == "legacy", scenario == "uncounted"
			if _, err := c.Step(context.Background()); err == nil || s.record.Phase != "pending" || o.startCalls != 0 {
				t.Fatalf("incomplete task evidence ended the round: record=%+v starts=%d err=%v", s.record, o.startCalls, err)
			}
		})
	}
}

type modelLog struct{ statuses []modelhealth.Status }

func (m *modelLog) Record(s modelhealth.Status) error {
	m.statuses = append(m.statuses, s)
	return nil
}

// recordedScreen returns the recorded worker-read receipt with its rendered
// screen replaced by lines, as Orca returns while the agent shows them.
func recordedScreen(t *testing.T, lines []string) orca.Receipt {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/recorded/orca-worker-read-terminal.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var terminal map[string]any
	if err := json.Unmarshal(envelope.Result["terminal"], &terminal); err != nil || terminal["source"] != "screen" {
		t.Fatalf("recorded terminal=%v err=%v", terminal, err)
	}
	terminal["tail"] = lines
	envelope.Result["terminal"], _ = json.Marshal(terminal)
	return envelope.Result
}

func (o *fakeOrca) WorkerRead(_ context.Context, opts orca.ReadOptions) (orca.Receipt, error) {
	o.screenReads++
	if opts.Dispatch == "" || opts.Source != "terminal" {
		return nil, errors.New("unexpected worker read")
	}
	return o.screen, nil
}

// OF-555, 2026-11-17: the final resolver sat at Claude's quota menu for 61
// minutes and the round counted as the ticket's failure.
func TestResolverRoundEndsOnModelLimitWithoutFailure(t *testing.T) {
	limit, err := os.ReadFile("../../testdata/recorded/claude-model-limit.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, top := range []bool{false, true} {
		t.Run(fmt.Sprintf("top=%v", top), func(t *testing.T) {
			c, s, o := setupController()
			models := &modelLog{}
			c.Models = models
			c.Agent, c.Model, c.Effort = "claude", "claude-fable-5-2", "xhigh"
			now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
			c.Now = func() time.Time { return now }
			c.Probe = func(_ context.Context, ref modelhealth.Ref) modelhealth.Status {
				return modelhealth.Classify(ref, string(limit), errors.New("exit status 1"), now)
			}
			if _, err := c.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			s.record.Top, s.record.TopLaunchAccepted = top, top
			o.workers = workerRows("run_1", `[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","dispatchStatus":"dispatched","agentTerminalHandle":"term_worker"}]`)
			o.stopped = workerRows("run_1", `[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","dispatchStatus":"failed"}]`)
			o.terminals = []orca.Terminal{{Handle: "term_worker", LastOutputAt: now.UnixMilli()}}
			o.screen = recordedScreen(t, []string{"● Reading the ticket", "", strings.TrimSpace(string(limit)), "", "  1. Stop and wait for limit to reset"})
			if p, err := c.Step(context.Background()); err != nil || p.Phase != "running" || o.screenReads != 0 {
				t.Fatalf("fresh output read the screen: p=%+v reads=%d err=%v", p, o.screenReads, err)
			}
			now = now.Add(ModelCheckDelay)
			p, err := c.Step(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if p.Phase != "waiting" || p.Outcome != ModelUnavailable || !p.RetryAt.Equal(now) || o.stops != 1 || o.releases != 1 || o.startCalls != 1 {
				t.Fatalf("round did not end as model-unavailable: p=%+v stops=%d releases=%d starts=%d", p, o.stops, o.releases, o.startCalls)
			}
			if strings.Contains(p.Result, "终审模型未获响应") {
				t.Fatalf("model limit reported as top-model failure: %s", p.Result)
			}
			want := time.Date(2026, 11, 21, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
			if len(models.statuses) != 1 || models.statuses[0].State != modelhealth.Unavailable || models.statuses[0].Reason != "usage_limit" || models.statuses[0].Ref.Key() != "claude:claude-fable-5-2" || !models.statuses[0].Until.Equal(want) {
				t.Fatalf("registry writes=%+v", models.statuses)
			}
		})
	}
}

func TestResolverRoundEndedWithModelErrorIsNoFailure(t *testing.T) {
	unsupported, err := os.ReadFile("../../testdata/recorded/codex-model-unsupported.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost=%v", lost), func(t *testing.T) {
			c, s, o := setupController()
			models := &modelLog{}
			c.Models = models
			c.Agent, c.Model, c.Effort = "codex", "gpt-6.2-astra", "max"
			c.Probe = func(_ context.Context, ref modelhealth.Ref) modelhealth.Status {
				return modelhealth.Classify(ref, string(unsupported), errors.New("exit status 1"), time.Now())
			}
			if _, err := c.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			s.record.Top, s.record.TopLaunchAccepted = true, true
			o.workers = workerRows("run_1", `[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","dispatchStatus":"failed"}]`)
			wantReleases := 1
			if lost {
				o.tasks = []orca.Task{{ID: "task_1", RunID: "run_1", Status: "ready", Spec: "Act as resolver"}}
				wantReleases = 0
			}
			o.screen = recordedScreen(t, []string{strings.TrimSpace(string(unsupported))})
			p, err := c.Step(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if p.Phase != "waiting" || p.Outcome != ModelUnavailable || o.releases != wantReleases || strings.Contains(p.Result, "终审模型未获响应") {
				t.Fatalf("ended round with model error counted as failure: p=%+v releases=%d", p, o.releases)
			}
			if len(models.statuses) != 1 || models.statuses[0].Ref.Key() != "codex:gpt-6.2-astra" {
				t.Fatalf("registry writes=%+v", models.statuses)
			}
		})
	}
}

// A working agent whose screen only quotes a model error, as a ticket about
// that error does, keeps its round. An ended turn's screen only nominates the
// model; the caller's probe confirms it (TestResolverQuotedModelErrorNeedsProbe).
func TestScreenModelIgnoresQuotedErrorWhileTurnRuns(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/orca-worker-read-terminal-busy.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var terminal struct{ Tail []string }
	if err := json.Unmarshal(envelope.Result["terminal"], &terminal); err != nil {
		t.Fatal(err)
	}
	limit, err := os.ReadFile("../../testdata/recorded/claude-model-limit.txt")
	if err != nil {
		t.Fatal(err)
	}
	unsupported, err := os.ReadFile("../../testdata/recorded/codex-model-unsupported.txt")
	if err != nil {
		t.Fatal(err)
	}
	quoted := append([]string{strings.TrimSpace(string(limit)), strings.TrimSpace(string(unsupported))}, terminal.Tail[2:]...)
	now := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	for _, ref := range []modelhealth.Ref{{Agent: "claude", Model: "claude-fable-5-2", Effort: "xhigh"}, {Agent: "codex", Model: "gpt-6.2-astra", Effort: "max"}} {
		busy := &fakeOrca{screen: recordedScreen(t, quoted)}
		status, err := ScreenModel(context.Background(), busy, "ctx_1", ref, now)
		if err != nil || status.State == modelhealth.Unavailable {
			t.Fatalf("%s: running turn classified as model error: %+v err=%v", ref, status, err)
		}
		idle := &fakeOrca{screen: recordedScreen(t, quoted[:2])}
		if status, err = ScreenModel(context.Background(), idle, "ctx_1", ref, now); err != nil || status.State != modelhealth.Unavailable {
			t.Fatalf("%s: ended turn not classified: %+v err=%v", ref, status, err)
		}
	}
}

// A finished turn that only quotes a model error keeps its round: the probe of
// the model answers, so nothing is stopped or recorded.
func TestResolverQuotedModelErrorNeedsProbe(t *testing.T) {
	unsupported, err := os.ReadFile("../../testdata/recorded/codex-model-unsupported.txt")
	if err != nil {
		t.Fatal(err)
	}
	c, s, o := setupController()
	models := &modelLog{}
	c.Models = models
	c.Agent, c.Model, c.Effort = "codex", "gpt-6.2-astra", "max"
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	probes := 0
	c.Probe = func(_ context.Context, ref modelhealth.Ref) modelhealth.Status {
		probes++
		return modelhealth.Classify(ref, "ok", nil, now)
	}
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.workers = workerRows("run_1", `[{"runId":"run_1","taskId":"task_1","dispatchId":"ctx_1","dispatchStatus":"dispatched","agentTerminalHandle":"term_worker"}]`)
	o.terminals = []orca.Terminal{{Handle: "term_worker", LastOutputAt: now.UnixMilli()}}
	o.screen = recordedScreen(t, []string{"• 票面现状：codex 返回 " + strings.TrimSpace(string(unsupported)), "", "› "})
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(ModelCheckDelay)
	p, err := c.Step(context.Background())
	if err != nil || p.Phase != "running" || o.stops != 0 || len(models.statuses) != 0 || probes != 1 || s.record.Outcome != "" {
		t.Fatalf("quoted error ended the round: p=%+v stops=%d records=%+v probes=%d err=%v", p, o.stops, models.statuses, probes, err)
	}
}
