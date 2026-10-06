package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"os"
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
	readErr    error
	status     string
}

func (o *fakeOrca) RunCreate(context.Context, string, string) (orca.Receipt, error) {
	return orca.Receipt{"run": json.RawMessage(`{"id":"run_1"}`)}, nil
}
func (o *fakeOrca) RunUse(context.Context, string, string) (orca.Receipt, error) {
	return orca.Receipt{}, nil
}
func (o *fakeOrca) WorkerStart(_ context.Context, id string, opts orca.StartOptions) (orca.Receipt, error) {
	o.startCalls++
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
	return orca.Receipt{}, nil
}
func setupController() (*Controller, *memoryStore, *fakeOrca) {
	s := &memoryStore{}
	o := &fakeOrca{store: s}
	return &Controller{Store: s, Orca: o, Ticket: "ENG-1", Repo: "/repo", Spec: "Act as resolver", Terminal: "first", Agent: "codex", Model: "model", Effort: "medium", Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}, s, o
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
func TestFailedAgentWaitsBeforeNewAttempt(t *testing.T) {
	c, s, o := setupController()
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.report = &orca.WorkerReport{Provenance: "worker_report", Outcome: "failed", Body: "dependency unavailable"}
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.record.Phase != "waiting" || !s.record.RetryAt.After(c.Now()) || o.releases != 1 {
		t.Fatalf("failure did not retain timed agent ownership: %+v", s.record)
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
	if s.record.Phase != "waiting" || o.releases != 1 {
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
