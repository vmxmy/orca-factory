package executor

import (
	"context"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

// comment writes one Linear comment. Linear often applies a write it cannot
// confirm, so a failed write is read back before it is replayed: an existing
// identical body counts as success and is never posted twice.
func (e *Executor) comment(ctx context.Context, issue, body string) error {
	return infra.Retry(ctx, infra.Options{
		Sleep: e.Sleep,
		ReadBack: func(ctx context.Context) (bool, error) {
			comments, err := e.Linear.ListComments(ctx, issue)
			if err != nil {
				return false, err
			}
			for _, c := range comments {
				if c.Body == body {
					return true, nil
				}
			}
			return false, nil
		},
	}, func(ctx context.Context) error {
		_, err := e.Linear.CreateComment(ctx, issue, body)
		return err
	})
}
