package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// unconfirmedIssues stores the first comment but reports an error, like a backend
// write that was applied but not confirmed; a second write would duplicate the comment.
type unconfirmedIssues struct {
	fakeIssues
	failed bool
}

func (f *unconfirmedIssues) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	c, _ := f.fakeIssues.CreateComment(ctx, id, body)
	if !f.failed {
		f.failed = true
		return c, &infra.Transient{Err: errors.New("backend may have applied the write, but could not confirm it")}
	}
	return c, nil
}

func TestCommentReadsBackUnconfirmedWrite(t *testing.T) {
	l := &unconfirmedIssues{}
	e := &Executor{Issues: l, Sleep: func(context.Context, time.Duration) error { return nil }}
	if err := e.comment(context.Background(), "issue_one", "阶段: gate"); err != nil {
		t.Fatal(err)
	}
	if len(l.comments) != 1 {
		t.Fatalf("comments=%q, want exactly one", l.comments)
	}
}
