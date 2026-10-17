package scheduler

import (
	"context"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// An executor that exited on a maintenance refusal resumes at the same attempt
// instead of counting toward "executor failed twice".
func TestMaintenanceRefusedExitResumesWithoutFailure(t *testing.T) {
	f := &fakeBackend{absent: true, inactive: true}
	s := setup(f)
	ticket := Ticket{ID: "ticket", Repo: "a", UpdatedAt: s.Now()}
	comments := []task.Comment{
		{Body: "阶段: 写 correction\n归属: 执行器 term=- run=- attempt=2 scheduler-attempt=2", CreatedAt: s.Now().Add(-3)},
		{Body: "阶段: 写 correction\n归属: 执行器 term=executor_terminal run=run_one attempt=2", CreatedAt: s.Now().Add(-2)},
		{Body: maintenance.RefusedLine + "term=executor_terminal", CreatedAt: s.Now().Add(-1)},
	}
	if err := readOwnership(&ticket, comments); err != nil {
		t.Fatal(err)
	}
	f.tickets = []Ticket{ticket}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 1 || f.actions[0].Kind != Resume || f.actions[0].Attempt != 2 || f.actions[0].Phase != machine.PhaseCorrection {
		t.Fatalf("refused exit counted as failure: %+v", f.actions)
	}
}
