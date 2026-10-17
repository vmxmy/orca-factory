package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// Only the executor's first refresh receipt for an attempt exempts its exit; a
// bare directive, a quoted receipt or a repeated receipt count as a failure.
func TestGateSpecRefreshResumesWithoutFailure(t *testing.T) {
	const receipt = "gate-spec-refresh 已执行 attempt=1 term=executor_terminal"
	ownership := "阶段: gate\n归属: 执行器 term=executor_terminal run=run_one attempt=1"
	for _, tc := range []struct {
		name    string
		bodies  []string
		attempt int
	}{
		{"receipt", []string{ownership, "gate-spec-refresh: 已改正 CHECK", receipt}, 1},
		{"directive only", []string{ownership, "gate-spec-refresh: 已改正 CHECK"}, 2},
		{"quoted receipt", []string{ownership, "EVIDENCE gate 输出\n" + receipt}, 2},
		{"repeated receipt", []string{receipt, ownership, receipt}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBackend{absent: true, inactive: true}
			s := setup(f)
			ticket := Ticket{ID: "ticket", Repo: "a", UpdatedAt: s.Now()}
			comments := []task.Comment{{Body: "阶段: gate\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1", CreatedAt: s.Now().Add(-10)}}
			for i, body := range tc.bodies {
				comments = append(comments, task.Comment{Body: body, CreatedAt: s.Now().Add(time.Duration(i - 9))})
			}
			if err := readOwnership(&ticket, comments); err != nil {
				t.Fatal(err)
			}
			f.tickets = []Ticket{ticket}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.actions) != 1 || f.actions[0].Kind != Resume || f.actions[0].Attempt != tc.attempt || f.actions[0].Phase != machine.PhaseGate {
				t.Fatalf("actions=%+v", f.actions)
			}
		})
	}
}
