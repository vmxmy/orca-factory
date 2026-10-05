package executor

import (
	"context"
	"testing"
)

// A ticket re-claimed by a new executor runs in a new Orca terminal. Orca rejects a
// reused mutation request with different input (request_mismatch), so the run
// request must be scoped to the executor terminal.
func TestRunRequestScopedToTerminal(t *testing.T) {
	ids := []string{}
	for _, term := range []string{"term_a", "term_b"} {
		e, o, _, _, _, ticket := fixture(t, "new-sha")
		e.Config.Terminal = term
		if _, err := e.Execute(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		if len(o.runRequests) != 1 {
			t.Fatalf("run requests=%v", o.runRequests)
		}
		ids = append(ids, o.runRequests[0])
	}
	if ids[0] == ids[1] {
		t.Fatalf("both terminals used run request %s", ids[0])
	}
}

func TestRestoredRunIsBoundWithRunUse(t *testing.T) {
	e, o, _, _, _, ticket := fixture(t, "new-sha")
	if _, err := e.execute(context.Background(), ticket, &checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one"}); err != nil {
		t.Fatal(err)
	}
	if len(o.runRequests) != 0 || len(o.used) != 1 || o.used[0] != "run_one" {
		t.Fatalf("run-create=%v run-use=%v", o.runRequests, o.used)
	}
}

func TestWorkerRequestScopedToTerminal(t *testing.T) {
	ids := []string{}
	for _, term := range []string{"term_a", "term_b"} {
		e, o, _, _, _, ticket := fixture(t, "new-sha")
		e.Config.Terminal = term
		if _, err := e.Execute(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		if len(o.workerRequests) != 1 {
			t.Fatalf("worker requests=%v", o.workerRequests)
		}
		ids = append(ids, o.workerRequests[0])
	}
	if ids[0] == ids[1] {
		t.Fatalf("both terminals used worker request %s", ids[0])
	}
}

func TestStartReconcilesPendingWorkerWithoutTask(t *testing.T) {
	e, o, _, _, _, ticket := fixture(t, "new-sha")
	// A previous executor saved Pending before worker-start and died; the run has no task for this branch.
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_old", Pending: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Start(context.Background(), ticket); err != nil {
		t.Fatalf("pending worker start without a task must restart, got %v", err)
	}
	if len(o.workerRequests) != 1 {
		t.Fatalf("worker requests=%v", o.workerRequests)
	}
}
