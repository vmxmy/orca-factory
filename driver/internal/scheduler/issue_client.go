package scheduler

import (
	"context"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

func InspectOwnership(comments []linear.Comment) (Ticket, error) {
	var t Ticket
	err := readOwnership(&t, comments)
	return t, err
}

func (r *Runtime) newIssueClient(ctx context.Context, env map[string]string) (IssueClient, error) {
	return issues.New(ctx, env, r.Tuning)
}
