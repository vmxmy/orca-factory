package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type refusedChildOrca struct {
	*recoveryOrca
	refused bool
}

func (o *refusedChildOrca) WorkerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	if opts.Task == "child_task" && !o.refused {
		o.refused = true
		return nil, fmt.Errorf("worker start: %w", maintenance.ErrRefused)
	}
	return o.recoveryOrca.WorkerStart(ctx, request, opts)
}
func (o *refusedChildOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	if o.refused && len(o.starts) == 1 {
		return []orca.Task{{ID: "child_task", RunID: "run_one", ParentID: "task_one", Status: "blocked"}}, nil
	}
	return o.recoveryOrca.TaskList(ctx, run)
}

// A correction dispatch the maintenance gate refused never reached Orca: the
// executor records a refusal instead of a failure, and the replacement replays
// the already-created child once.
func TestCorrectionMaintenanceRefusedReplaysChild(t *testing.T) {
	e, base, tools, s, l, ticket := recoveryFixture(t)
	tools.gateReds = 1
	o := &refusedChildOrca{recoveryOrca: base}
	e.Orca = o
	if _, err := e.Execute(context.Background(), ticket); !errors.Is(err, maintenance.ErrRefused) {
		t.Fatalf("refusal not reported: %v", err)
	}
	if last := l.comments[len(l.comments)-1]; !strings.HasPrefix(last, maintenance.RefusedLine) {
		t.Fatalf("refusal not recorded: %q", last)
	}
	l.comments = append(l.comments, "阶段: 写 correction\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1")
	e.Config.Terminal = "replacement_terminal"
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("replay failed: %+v err=%v", result, err)
	}
	if s.children != 1 || len(o.starts) != 2 || o.starts[1].Task != "child_task" {
		t.Fatalf("child not replayed once: children=%d starts=%+v", s.children, o.starts)
	}
}
