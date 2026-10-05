package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

// unconfirmedLinear stores the first comment but reports an error, like Linear's
// linear_write_unconfirmed; a second write would duplicate the comment.
type unconfirmedLinear struct {
	fakeLinear
	failed bool
}

func (f *unconfirmedLinear) CreateComment(ctx context.Context, id, body string) (linear.Comment, error) {
	c, _ := f.fakeLinear.CreateComment(ctx, id, body)
	if !f.failed {
		f.failed = true
		return c, &infra.Transient{Err: errors.New("Linear may have applied the write, but could not confirm it")}
	}
	return c, nil
}

func TestCommentReadsBackUnconfirmedWrite(t *testing.T) {
	l := &unconfirmedLinear{}
	e := &Executor{Linear: l, Sleep: func(context.Context, time.Duration) error { return nil }}
	if err := e.comment(context.Background(), "issue_one", "阶段: gate"); err != nil {
		t.Fatal(err)
	}
	if len(l.comments) != 1 {
		t.Fatalf("comments=%q, want exactly one", l.comments)
	}
}
