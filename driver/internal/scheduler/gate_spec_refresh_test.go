package scheduler

import (
	"context"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// An owner's refresh directive resumes the dead executor at the same attempt;
// the same text quoted inside a driver comment is not a directive.
func TestGateSpecRefreshResumesWithoutFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       ActionKind
	}{
		{"directive", "gate-spec-refresh: 负责人已改正 CHECK", Resume},
		{"quoted", "阶段: 写 correction\ngate-spec-refresh: 来自 gate 输出", Resolve},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBackend{absent: true, inactive: true}
			s := setup(f)
			ticket := Ticket{ID: "ticket", Repo: "a", UpdatedAt: s.Now()}
			comments := []task.Comment{
				{Body: "阶段: gate\n归属: 执行器 term=- run=- attempt=2 scheduler-attempt=2", CreatedAt: s.Now().Add(-3)},
				{Body: "阶段: gate\n归属: 执行器 term=executor_terminal run=run_one attempt=2", CreatedAt: s.Now().Add(-2)},
				{Body: tc.body, CreatedAt: s.Now().Add(-1)},
			}
			if err := readOwnership(&ticket, comments); err != nil {
				t.Fatal(err)
			}
			f.tickets = []Ticket{ticket}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.actions) != 1 || f.actions[0].Kind != tc.want || f.actions[0].Attempt != 2 {
				t.Fatalf("actions=%+v", f.actions)
			}
			if tc.want == Resume && f.actions[0].Phase != machine.PhaseGate {
				t.Fatalf("resumed phase %s", f.actions[0].Phase)
			}
		})
	}
}
