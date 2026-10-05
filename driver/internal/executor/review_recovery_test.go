package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type existingPRTaskOrca struct {
	*recoveryOrca
	outcome string
}

func (o *existingPRTaskOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	if len(o.starts) != 0 {
		return o.recoveryOrca.TaskList(ctx, run)
	}
	task := orca.Task{ID: "task_one", RunID: run, Status: "running"}
	if o.outcome != "running" {
		task.Result = json.RawMessage(`{"provenance":"worker_report","outcome":"` + o.outcome + `"}`)
	}
	return []orca.Task{task}, nil
}

func TestStartConfirmedTaskPrecedesExistingPR(t *testing.T) {
	for _, outcome := range []string{"failed", "running", "succeeded"} {
		t.Run(outcome, func(t *testing.T) {
			e, base, tools, service, _, ticket := recoveryFixture(t)
			o := &existingPRTaskOrca{recoveryOrca: base, outcome: outcome}
			e.Orca = o
			service.complete = true
			gh := e.GitHub.(*fakeGitHub)
			gh.existing = &github.PullRequest{Number: 7, HTMLURL: "https://example.test/pr/7"}
			e.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
			if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Worktree: "/original/worktree", Corrections: 1, Pending: false, Completed: true, Settled: true}); err != nil {
				t.Fatal(err)
			}
			result, err := e.Start(context.Background(), ticket)
			if outcome == "running" {
				if !errors.Is(err, context.Canceled) || result.State.Owner == machine.OwnerMerge || len(o.starts) != 0 || o.releases != 0 || tools.gateCalls != 0 {
					t.Fatalf("running task must wait without handoff or release: result=%+v err=%v starts=%d releases=%d gates=%d", result, err, len(o.starts), o.releases, tools.gateCalls)
				}
				return
			}
			wantCorrections, wantSessions := 1, 0
			if outcome == "failed" {
				wantCorrections, wantSessions = 2, 1
			}
			if err != nil || result.State.Owner != machine.OwnerMerge || result.PR.Number != 7 || result.State.Corrections != wantCorrections || service.sessions != wantSessions || len(o.starts) != wantSessions || len(gh.created) != 0 {
				t.Fatalf("current task must govern existing PR recovery: result=%+v err=%v sessions=%d starts=%d", result, err, service.sessions, len(o.starts))
			}
		})
	}
}

// The external adapter reports cancellation after the durable intent was saved,
// while the filesystem refuses the subsequent receipt checkpoint.
type cancelledDispatchError struct{}

func (*cancelledDispatchError) Error() string { return "private-adapter-detail" }
func (*cancelledDispatchError) Unwrap() error { return context.Canceled }

type checkpointFailureOrca struct {
	fakeOrca
	beforeReturn func()
	callErr      error
}

func (o *checkpointFailureOrca) WorkerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	receipt, _ := o.fakeOrca.WorkerStart(ctx, request, opts)
	o.beforeReturn()
	return receipt, o.callErr
}
func TestExecutePreservesCancellationWhenReceiptCheckpointFails(t *testing.T) {
	e, _, _, _, _, ticket := recoveryFixture(t)
	callErr := &cancelledDispatchError{}
	o := &checkpointFailureOrca{callErr: callErr, beforeReturn: func() {
		if err := os.Mkdir(checkpointPath(e.Config, ticket)+".new", 0700); err != nil {
			t.Fatal(err)
		}
	}}
	e.Orca = o
	result, err := e.Execute(context.Background(), ticket)
	var cancellation *cancelledDispatchError
	if !errors.Is(err, context.Canceled) || !errors.Is(err, callErr) || !errors.As(err, &cancellation) || cancellation != callErr {
		t.Fatalf("checkpoint failure must retain cancellation identity/type: result=%+v err=%v", result, err)
	}
	if !strings.Contains(err.Error(), "cannot write recovery state") || strings.Contains(err.Error(), "private-adapter-detail") {
		t.Fatalf("both failures need safe diagnostics: %v", err)
	}
	if len(o.workerRequests) != 1 || o.releases != 0 || result.State.Owner == machine.OwnerMerge {
		t.Fatalf("uncertain write must not replay/release/handoff: result=%+v starts=%d releases=%d", result, len(o.workerRequests), o.releases)
	}
}
