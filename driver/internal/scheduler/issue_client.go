package scheduler

import (
	"context"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func InspectOwnership(comments []task.Comment) (Ticket, error) {
	var t Ticket
	err := readOwnership(&t, comments)
	return t, err
}

func (r *Runtime) newIssueStore(ctx context.Context, env map[string]string) (issues.Store, error) {
	return issues.New(ctx, env, r.Tuning)
}
