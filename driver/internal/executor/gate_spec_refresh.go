package executor

import (
	"context"
	"errors"

	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// errGateSpecRefresh stops the executor before a gate run so the scheduler
// resumes the stage and the replacement rewrites gate.spec from the ticket face.
var errGateSpecRefresh = errors.New("gate spec refresh requested; executor exits for replay")

func (e *Executor) gateSpecRefresh(ctx context.Context, t Ticket, result *Result) (task.GateSpecRefresh, error) {
	var refresh task.GateSpecRefresh
	err := e.retryRead(ctx, t, result, "gate spec refresh lookup", func() error {
		comments, readErr := e.Issues.ListComments(ctx, t.ID)
		if readErr != nil {
			return safeDependencyError(readErr)
		}
		refresh = task.ReadGateSpecRefresh(comments)
		return nil
	})
	return refresh, err
}

// honorGateSpecRefresh writes the audit receipt and stops the executor when a
// directive is pending and this attempt has not refreshed yet.
func (e *Executor) honorGateSpecRefresh(ctx context.Context, t Ticket, result *Result) error {
	refresh, err := e.gateSpecRefresh(ctx, t, result)
	if err != nil || !refresh.Pending || refresh.Done[result.State.Attempt] {
		return err
	}
	if err := e.comment(ctx, t.ID, task.GateSpecRefreshReceipt(result.State.Attempt, e.Config.Terminal)); err != nil {
		return err
	}
	return errGateSpecRefresh
}

// reportFrozenGateSpecRefresh tells the owner that a directive still pending at
// handoff did not run; the handoff itself is unchanged.
func (e *Executor) reportFrozenGateSpecRefresh(ctx context.Context, t Ticket, result *Result) error {
	refresh, err := e.gateSpecRefresh(ctx, t, result)
	if err != nil || !refresh.Pending {
		return err
	}
	if err := e.comment(ctx, t.ID, task.GateSpecRefreshFrozen); err != nil {
		e.warn("%s gate spec refresh notice failed: %v", t.Identifier, err)
	}
	return nil
}
