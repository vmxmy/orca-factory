package executor

import (
	"context"
	"errors"

	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// errGateSpecRefresh stops the executor before a gate run so the scheduler
// resumes the stage and the replacement rewrites gate.spec from the ticket face.
var errGateSpecRefresh = errors.New("gate spec refresh requested; executor exits for replay")

func (e *Executor) gateSpecRefreshRequested(ctx context.Context, t Ticket) (bool, error) {
	comments, err := e.Issues.ListComments(ctx, t.ID)
	if err != nil {
		return false, safeDependencyError(err)
	}
	return task.GateSpecRefreshRequested(comments), nil
}
