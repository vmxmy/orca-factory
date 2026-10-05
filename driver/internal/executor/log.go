package executor

import (
	"context"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

func (e *Executor) logger() *slog.Logger {
	w := e.Stderr
	if w == nil {
		w = os.Stderr
	}
	return logx.New("executor", w)
}

// step records every machine decision, including rejected and self transitions.
func (e *Executor) step(state machine.State, ev machine.Event) (machine.State, []machine.Effect, error) {
	next, effects, err := machine.Step(state, ev, machine.DefaultLimits)
	loggedEffects := append([]machine.Effect{}, effects...)
	e.logger().Info("transition", "from", state.Phase, "to", next.Phase, "event", ev, "effects", loggedEffects, "error", err)
	return next, effects, err
}

func (e *Executor) logError(phase machine.Phase, err error) {
	if err != nil {
		e.logger().Error("executor failed", "phase", phase, "error", err)
	}
}

// A monotonic clock measures actual dependency time independently of the claim clock.
func (e *Executor) external(step string) func(error, ...any) {
	start := time.Now()
	e.logger().Info("external.start", "step", step)
	return func(err error, attrs ...any) {
		result := "ok"
		if err != nil {
			result = "error"
		}
		fields := []any{"step", step, "duration_ms", float64(time.Since(start)) / float64(time.Millisecond), "result", result, "error", err}
		e.logger().Info("external.end", append(fields, attrs...)...)
	}
}

func (e *Executor) workerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	done := e.external("worker-start")
	receipt, err := e.Orca.WorkerStart(ctx, request, opts)
	done(err, "task", receiptString(receipt, "taskId"), "dispatch", receiptString(receipt, "dispatchId"))
	return receipt, err
}
func (e *Executor) taskList(ctx context.Context, run string) ([]orca.Task, error) {
	done := e.external("task-list")
	tasks, err := e.Orca.TaskList(ctx, run)
	done(err, "count", len(tasks))
	return tasks, err
}

func (e *Executor) recoverySnapshot(ctx context.Context, t Ticket, cp *checkpoint, checkpointErr error) {
	comments, err := e.Linear.ListComments(ctx, t.ID)
	// Comments are not ordered by the adapter; only the newest phase/owner count.
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	phase, owner := "unknown", "unknown"
	for _, c := range comments {
		for _, line := range strings.Split(c.Body, "\n") {
			if value, ok := strings.CutPrefix(line, "阶段: "); ok {
				phase = value
			}
			if value, ok := strings.CutPrefix(line, "归属: "); ok {
				owner = value
			}
		}
	}
	e.logger().Info("recovery.snapshot", "checkpoint", cp, "checkpoint_error", checkpointErr, "linear_phase", phase, "linear_owner", owner, "error", err)
}
