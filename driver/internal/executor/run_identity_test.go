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
