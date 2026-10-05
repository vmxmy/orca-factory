package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// startDispatch is the write boundary for both first workers and corrections.
// Persist intent before I/O, then preserve every usable association even when
// the command returns an error. Completion belongs only to this dispatch.
func (e *Executor) startDispatch(ctx context.Context, c Config, t Ticket, cp *checkpoint, opts orca.StartOptions) error {
	cp.Task, cp.Dispatch = opts.Task, ""
	cp.PendingParent = ""
	cp.Pending, cp.Completed, cp.Settled = true, false, false
	if err := saveCheckpoint(c, t, *cp); err != nil {
		return err
	}
	receipt, callErr := e.Orca.WorkerStart(ctx, scopedRequest(t, c, "worker", cp.Corrections+1), opts)
	if task := receiptID(receipt, "task"); task != "" {
		cp.Task = task
	}
	cp.Dispatch = receiptID(receipt, "dispatch")
	if path := worktreeReceipt(receipt); path != "" {
		cp.Worktree = path
	}
	cp.Pending = callErr != nil || cp.Task == "" || cp.Dispatch == ""
	if err := saveCheckpoint(c, t, *cp); err != nil {
		return err
	}
	if callErr != nil {
		return fmt.Errorf("worker start uncertain; reconcile dispatch before retry: %w", safeDependencyError(callErr))
	}
	if cp.Pending {
		return errors.New("worker receipt missing task or dispatch")
	}
	return nil
}

func receiptID(receipt orca.Receipt, entity string) string {
	if id := receiptString(receipt, entity+"Id"); id != "" {
		return id
	}
	return nestedString(receipt, entity, "id")
}

// recoverDispatch uses task evidence, never replays a mutation bound to an old
// terminal. The same matching and report rules apply to every dispatch.
func recoverDispatch(tasks []orca.Task, cp *checkpoint, branch string) (*checkpoint, error) {
	cp.Completed, cp.Settled = false, false
	var match *orca.Task
	for i := range tasks {
		task := &tasks[i]
		if task.RunID != "" && task.RunID != cp.Run {
			continue
		}
		switch {
		case cp.PendingParent != "":
			if task.ParentID != cp.PendingParent {
				continue
			}
		case cp.Task != "":
			if task.ID != cp.Task {
				continue
			}
		default:
			if !strings.Contains(task.Spec, branch) {
				continue
			}
		}
		if cp.Dispatch != "" && task.DispatchID != "" && task.DispatchID != cp.Dispatch {
			continue
		}
		if match != nil || task.ID == "" {
			return nil, errors.New("ambiguous recovery tasks; reconcile active dispatch")
		}
		match = task
	}
	if match == nil {
		if cp.Pending {
			return nil, errors.New("unresolved worker start; no task evidence")
		}
		if cp.Task == "" {
			return nil, nil
		}
		return cp, nil
	}
	report, err := match.WorkerReport()
	if err != nil {
		return nil, errors.New("resume task result invalid")
	}
	cp.Task = match.ID
	if match.DispatchID != "" {
		cp.Dispatch = match.DispatchID
	}
	if report != nil {
		if report.Outcome != "succeeded" && report.Outcome != "failed" {
			return nil, errors.New("invalid worker outcome")
		}
		cp.Completed = report.Outcome == "succeeded"
		cp.Settled, cp.Pending = true, false
		cp.PendingParent = ""
	}
	return cp, nil
}

// Child creation is itself an uncertain mutation. Checkpoint its parent before
// the request so a restart cannot mistake the old successful task for this one.
func (e *Executor) startCorrectionDispatch(ctx context.Context, c Config, t Ticket, cp *checkpoint, spec string, opts orca.StartOptions) error {
	parent := cp.Task
	cp.PendingParent = parent
	cp.Task, cp.Dispatch = "", ""
	cp.Pending, cp.Completed, cp.Settled = true, false, false
	if err := saveCheckpoint(c, t, *cp); err != nil {
		return err
	}
	child, err := e.recovery().ChildTask(ctx, cp.Run, parent, spec, scopedRequest(t, c, "child", cp.Corrections+1))
	if err != nil {
		return fmt.Errorf("child task creation uncertain; reconcile request before retry: %w", safeDependencyError(err))
	}
	opts.Task = child
	return e.startDispatch(ctx, c, t, cp, opts)
}

// runRequest scopes Run mutations to the executor terminal: Orca rejects a reused
// request ID whose input differs (request_mismatch), and the caller is part of
// that input, so each executor terminal needs its own request while a retry in
// the same terminal stays an idempotent replay.
func runRequest(t Ticket, c Config, step string) string { return scopedRequest(t, c, step, 1) }

// scopedRequest applies the same terminal scope to every Orca mutation.
func scopedRequest(t Ticket, c Config, step string, n int) string {
	return orca.RequestID(t.Identifier, c.Unit, step+"-"+c.Terminal, n)
}
