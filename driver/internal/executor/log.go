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
)

// Each invocation owns its associations; logging never changes durable state or IDs.
type executionLog struct {
	ticket, terminal, run, task, dispatch string
	unappliedEvent                        machine.Event
	unappliedTo                           machine.Phase
}

func (e *Executor) withLog(t Ticket) *Executor {
	local := *e
	local.logContext = &executionLog{ticket: t.Identifier, terminal: e.Config.Terminal}
	return &local
}

func (e *Executor) logger() *slog.Logger {
	w := e.Stderr
	if w == nil {
		w = os.Stderr
	}
	logger := logx.New("executor", w)
	if c := e.logContext; c != nil {
		logger = logger.With("ticket", c.ticket, "terminal", c.terminal)
		if c.run != "" {
			logger = logger.With("run", c.run)
		}
		if c.task != "" {
			logger = logger.With("task", c.task)
		}
		if c.dispatch != "" {
			logger = logger.With("dispatch", c.dispatch)
		}
	}
	return logger
}

func (e *Executor) association(run, task, dispatch string) {
	if c := e.logContext; c != nil {
		c.run, c.task, c.dispatch = run, task, dispatch
	}
}

// Step records a decision, before any action can authorize applying its result.
func (e *Executor) step(state machine.State, ev machine.Event) (machine.State, []machine.Effect, error) {
	return e.stepDecision(state, ev, "execution", state.Phase)
}
func (e *Executor) stepDecision(state machine.State, ev machine.Event, kind string, observed machine.Phase) (machine.State, []machine.Effect, error) {
	next, effects, err := machine.Step(state, ev, machine.DefaultLimits)
	loggedEffects := append([]machine.Effect{}, effects...)
	e.logger().Info("transition", "record_kind", "decision", "decision_kind", kind, "allowed", err == nil, "applied", false, "actual_phase", observed, "from", state.Phase, "to", next.Phase, "event", ev, "effects", loggedEffects, "error", safeError(err))
	return next, effects, err
}

func (e *Executor) unapplied(ev machine.Event, next machine.Phase) {
	if c := e.logContext; c != nil {
		c.unappliedEvent, c.unappliedTo = ev, next
	}
}
func (e *Executor) logError(phase machine.Phase, err error, attrs ...any) {
	if err == nil {
		return
	}
	fields := []any{"phase", phase, "error", safeError(err)}
	if c := e.logContext; c != nil && c.unappliedEvent != "" {
		fields = append(fields, "event", c.unappliedEvent, "decision_to", c.unappliedTo, "transition_applied", false)
	}
	e.logger().Error("executor failed", append(fields, attrs...)...)
}

// Final errors are executor summaries. Strip wrapped dependency details and bound
// them; external call errors use an explicit summary rather than remote text.
func safeError(err error) any {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return "context canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context deadline exceeded"
	}
	summary, _, _ := strings.Cut(err.Error(), "\n")
	summary, _, _ = strings.Cut(summary, ": ")
	return truncateFail(summary, 160)
}
func callError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("dependency call failed")
}

// A monotonic clock includes validation and parsing, independently of claim time.
func (e *Executor) external(step string) func(error, ...any) {
	start := time.Now()
	e.logger().Info("external.start", "step", step)
	return func(err error, attrs ...any) {
		result := "ok"
		level := slog.LevelInfo
		if err != nil {
			result = "error"
			level = slog.LevelError
		}
		fields := []any{"step", step, "duration_ms", float64(time.Since(start)) / float64(time.Millisecond), "result", result, "error", safeError(err)}
		e.logger().Log(context.Background(), level, "external.end", append(fields, attrs...)...)
	}
}

func (e *Executor) workerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	e.association(opts.Run, opts.Task, "")
	done := e.external("worker-start")
	receipt, err := e.Orca.WorkerStart(ctx, request, opts)
	task, dispatch := receiptString(receipt, "taskId"), receiptString(receipt, "dispatchId")
	if task == "" {
		task = opts.Task
	}
	e.association(opts.Run, task, dispatch)
	validationErr := error(nil)
	if err == nil && (task == "" || dispatch == "") {
		validationErr = errors.New("worker receipt missing task or dispatch")
	}
	command := "ok"
	if err != nil {
		command = "error"
	}
	done(errors.Join(callError(err), validationErr), "command_result", command, "call_error", safeError(callError(err)), "receipt_error", safeError(validationErr))
	// Callers keep their original receipt checks and error/stop behavior.
	return receipt, err
}
func (e *Executor) taskList(ctx context.Context, run string) ([]orca.Task, error) {
	if c := e.logContext; c != nil {
		c.run = run
	}
	done := e.external("task-list")
	tasks, err := e.Orca.TaskList(ctx, run)
	done(callError(err), "count", len(tasks))
	return tasks, err
}

// Only the checkpoint's association is evidence here; omit local paths and extras.
func checkpointEvidence(cp *checkpoint) any {
	if cp == nil {
		return nil
	}
	return map[string]any{"Run": cp.Run, "Task": cp.Task, "Dispatch": cp.Dispatch, "Pending": cp.Pending, "Completed": cp.Completed, "Settled": cp.Settled, "Corrections": cp.Corrections}
}
func (e *Executor) recoveryPath(cp *checkpoint, path, basis string) {
	if cp != nil {
		e.association(cp.Run, cp.Task, cp.Dispatch)
	}
	e.logger().Info("recovery.path", "path", path, "basis", basis, "checkpoint", checkpointEvidence(cp), "linear_used_for_selection", false)
}
func (e *Executor) recoverySnapshot(ctx context.Context, t Ticket, cp *checkpoint, checkpointErr error) {
	if cp != nil {
		e.association(cp.Run, cp.Task, cp.Dispatch)
	}
	// Capture raw checkpoint evidence before any reconciliation changes it.
	evidence := checkpointEvidence(cp)
	comments, err := e.Linear.ListComments(ctx, t.ID)
	if err != nil {
		e.logger().Info("recovery.snapshot", "checkpoint", evidence, "checkpoint_error", safeError(checkpointErr), "linear_read", "error", "linear_used_for_selection", false, "error", "Linear recovery comments read failed")
		return
	}
	// Sorting a copy preserves an adapter's cached slice for other observations.
	comments = append(comments[:0:0], comments...)
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
	e.logger().Info("recovery.snapshot", "checkpoint", evidence, "checkpoint_error", safeError(checkpointErr), "linear_read", "ok", "linear_phase", phase, "linear_owner", owner, "linear_used_for_selection", false, "error", nil)
}
