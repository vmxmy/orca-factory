package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// Real `orca orchestration task-list --json` tasks carry no dispatch_id field.
const realTaskList = `[{"id":"task_one","run_id":"run_one","status":"completed","result":"{\"provenance\":\"worker_report\",\"outcome\":\"succeeded\",\"body\":\"ok\",\"filesModified\":[]}"}]`

type realShapeOrca struct{ fakeOrca }

func (f *realShapeOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	var tasks []orca.Task
	err := json.Unmarshal([]byte(realTaskList), &tasks)
	return tasks, err
}

func TestWaitAcceptsTaskWithoutDispatchID(t *testing.T) {
	e := &Executor{Orca: &realShapeOrca{}, Sleep: func(context.Context, time.Duration) error {
		t.Fatal("wait polled again instead of reading the settled task")
		return nil
	}}
	report, err := e.wait(context.Background(), "run_one", "task_one", "ctx_one", time.Millisecond, &workerWatch{}, func() error { return nil })
	if err != nil || report == nil || report.Outcome != "succeeded" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestWaitEndsOnLostDispatch(t *testing.T) {
	o := &recordedTasks{tasks: []orca.Task{{ID: "task_one", RunID: "run_one", Status: "ready"}}}
	e := &Executor{Orca: o, Sleep: func(context.Context, time.Duration) error {
		t.Fatal("wait kept polling a task whose dispatch is lost")
		return nil
	}}
	report, err := e.wait(context.Background(), "run_one", "task_one", "ctx_one", time.Millisecond, &workerWatch{}, func() error { return nil })
	if err != nil || report == nil || report.Outcome != "failed" || report.Provenance != provenanceExecutorLiveness || report.Body != "dispatch lost: task task_one is ready without dispatch ctx_one" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

type queuedOrca struct {
	recordedTasks
	workers []orca.Worker
}

func (q *queuedOrca) Workers(context.Context, orca.ListOptions) ([]orca.Worker, error) {
	return q.workers, nil
}

// A dispatch queued behind a busy terminal is also ready without a task
// dispatch_id (recorded receipt); its listed worker keeps the wait polling.
func TestWaitKeepsRecordedQueuedDispatch(t *testing.T) {
	var envelope struct {
		Result struct {
			Run      string `json:"runId"`
			Task     string `json:"taskId"`
			Dispatch string `json:"dispatchId"`
			State    string `json:"state"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorded(t, "orca-worker-start-reuse-busy-queued.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	r := envelope.Result
	o := &queuedOrca{recordedTasks: recordedTasks{tasks: []orca.Task{{ID: r.Task, RunID: r.Run, Status: r.State}}}, workers: []orca.Worker{{DispatchID: r.Dispatch}}}
	polled := errors.New("polled again")
	e := &Executor{Orca: o, Sleep: func(context.Context, time.Duration) error { return polled }}
	report, err := e.wait(context.Background(), r.Run, r.Task, r.Dispatch, time.Millisecond, &workerWatch{}, func() error { return nil })
	if !errors.Is(err, polled) || report != nil {
		t.Fatalf("queued dispatch judged lost: report=%+v err=%v", report, err)
	}
}

type settlesDuringWorkerRead struct {
	recordedTasks
	settled []orca.Task
}

func (s *settlesDuringWorkerRead) Workers(context.Context, orca.ListOptions) ([]orca.Worker, error) {
	s.tasks = s.settled
	return nil, nil
}

// The dispatch may finish and report between the task read and the worker read;
// only a task read after the worker read can confirm loss.
func TestWaitRereadsTasksBeforeJudgingLost(t *testing.T) {
	o := &settlesDuringWorkerRead{
		recordedTasks: recordedTasks{tasks: []orca.Task{{ID: "task_one", RunID: "run_one", Status: "ready"}}},
		settled:       []orca.Task{{ID: "task_one", RunID: "run_one", Status: "completed", Result: json.RawMessage(`{"provenance":"worker_report","outcome":"succeeded","body":"done"}`)}},
	}
	e := &Executor{Orca: o, Sleep: func(context.Context, time.Duration) error { return errors.New("polled again") }}
	report, err := e.wait(context.Background(), "run_one", "task_one", "ctx_one", time.Millisecond, &workerWatch{}, func() error { return nil })
	if err != nil || report == nil || report.Outcome != "succeeded" || report.Body != "done" {
		t.Fatalf("settled dispatch judged lost: report=%+v err=%v", report, err)
	}
}
