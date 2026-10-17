package executor

import (
	"context"
	"encoding/json"
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
