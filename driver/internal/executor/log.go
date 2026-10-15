package executor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Each invocation owns its logger; logging never changes durable state or IDs.
func (e *Executor) withLog(ctx context.Context) *Executor {
	local := *e
	local.logContext = e.logger()
	if logger, ok := runx.LookupLogger(ctx); ok {
		local.logContext = logger
	}
	return &local
}

func (e *Executor) logger() *slog.Logger {
	if e.logContext != nil {
		return e.logContext
	}
	w := e.Stderr
	if w == nil {
		w = os.Stderr
	}
	return logx.New("executor", w)
}

func (e *Executor) step(state machine.State, ev machine.Event) (machine.State, []machine.Effect, error) {
	next, effects, err := machine.Step(state, ev, machine.DefaultLimits)
	// This is the machine decision; its effects may still fail before application.
	e.logger().Info("transition", "from", state.Phase, "to", next.Phase, "event", ev, "effects", append([]machine.Effect{}, effects...))
	return next, effects, err
}

func (e *Executor) logError(phase machine.Phase, err error) {
	if err != nil {
		e.logger().Error("executor failed", "phase", phase, "error", logErrorSummary(err))
	}
}

// Never use external error text, including wrapped causes, as log attributes.
func logErrorSummary(err error) string {
	if errors.Is(err, context.Canceled) {
		return "context canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context deadline exceeded"
	}
	return "executor operation failed"
}

func (e *Executor) external(step string) func(error, ...any) {
	start := time.Now()
	e.logger().Info("external.start", "step", step)
	return func(err error, attrs ...any) {
		result := "ok"
		if err != nil {
			result = "error"
		}
		fields := []any{"step", step, "duration_ms", float64(time.Since(start)) / float64(time.Millisecond), "result", result}
		level := slog.LevelInfo
		if err != nil {
			level = slog.LevelError
		}
		e.logger().Log(context.Background(), level, "external.end", append(fields, attrs...)...)
	}
}

func (e *Executor) workerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	done := e.external("worker-start")
	receipt, err := e.Orca.WorkerStart(ctx, request, opts)
	validationErr := err
	task := receiptID(receipt, "task")
	if task == "" {
		task = opts.Task
	}
	if err == nil && (task == "" || receiptID(receipt, "dispatch") == "") {
		validationErr = errors.New("worker receipt missing task or dispatch")
	}
	done(validationErr)
	return receipt, err
}

func (e *Executor) taskList(ctx context.Context, run string) ([]orca.Task, error) {
	done := e.external("task-list")
	tasks, err := e.Orca.TaskList(ctx, run)
	done(err)
	return tasks, err
}

func checkpointEvidence(cp *checkpoint) any {
	if cp == nil {
		return nil
	}
	return map[string]any{"Run": cp.Run, "Task": cp.Task, "Dispatch": cp.Dispatch, "Pending": cp.Pending, "Completed": cp.Completed}
}

func recoveryWorkerPath(cp *checkpoint) string {
	if cp.MergeCause != "" {
		return "merge-correction"
	}
	if cp.Completed {
		return "completed-worker"
	}
	return "active-worker"
}

func (e *Executor) recoveryPath(cp *checkpoint, path string) {
	e.logger().Info("recovery.path", "path", path, "checkpoint", checkpointEvidence(cp))
}

func (e *Executor) recoverySnapshot(ctx context.Context, t Ticket, cp *checkpoint) {
	comments, err := e.Issues.ListComments(ctx, t.ID)
	phase, owner := "unknown", "unknown"
	if err == nil {
		comments = append(comments[:0:0], comments...)
		sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
		for _, c := range comments {
			// Legacy handoff comments omit explicit phase and ownership lines.
			if strings.HasPrefix(c.Body, "转人: ") {
				phase, owner = string(machine.PhaseHuman), string(machine.OwnerHuman)
			}
			if strings.HasPrefix(c.Body, "Agent处置: ") {
				phase, owner = string(machine.PhaseResolver), string(machine.OwnerResolver)
			}
			for _, line := range strings.Split(c.Body, "\n") {
				if value, ok := strings.CutPrefix(line, "阶段: "); ok && value != "" {
					phase = value
				}
				if value, ok := strings.CutPrefix(line, "归属: "); ok && value != "" {
					owner = value
				}
			}
		}
	}
	e.logger().Info("recovery.snapshot", "checkpoint", checkpointEvidence(cp), "ticket_phase", phase, "ticket_owner", owner)
}
