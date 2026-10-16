package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// handoffReadFailure fails comment reads once passes earlier reads succeeded.
type handoffReadFailure struct {
	*fakeIssues
	failure error
	passes  int
}

func (l *handoffReadFailure) ListComments(ctx context.Context, id string) ([]task.Comment, error) {
	if l.failure != nil && l.passes == 0 {
		return nil, l.failure
	}
	if l.failure != nil {
		l.passes--
	}
	return l.fakeIssues.ListComments(ctx, id)
}

type handoffBudgetTools struct {
	*fakeTools
	failed bool
}

func (f *handoffBudgetTools) Head(ctx context.Context, repo, branch string) (string, error) {
	if !f.failed {
		f.failed = true
		return "", &infra.Ticket{Err: errors.New("temporary read")}
	}
	return f.fakeTools.Head(ctx, repo, branch)
}

type handoffFailureAfterPR struct {
	*fakeGitHub
	tracker *handoffReadFailure
	failure error
}

func (g *handoffFailureAfterPR) CreatePR(ctx context.Context, owner, repo string, input github.CreatePRRequest) (github.PullRequest, error) {
	pr, err := g.fakeGitHub.CreatePR(ctx, owner, repo, input)
	g.tracker.failure = g.failure
	return pr, err
}

func TestMergeHandoffReadFailureKeepsSafeClassAndBudget(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, kind := range []string{"global", "canceled", "deadline"} {
			t.Run(fmt.Sprintf("existing=%v/%s", existing, kind), func(t *testing.T) {
				e, _, l, g, tools, ticket := fixture(t, strings.Repeat("b", 40))
				if existing {
					r, err := e.Execute(context.Background(), ticket)
					if err != nil {
						t.Fatal(err)
					}
					g.existing = &r.PR
					g.existing.Head.SHA = tools.head
				}
				cause := error(&infra.Global{Dep: infra.GitHub, Err: fmt.Errorf("secret-token: %w", context.Canceled)})
				if kind == "canceled" {
					cause = fmt.Errorf("secret-token: %w", context.Canceled)
				}
				if kind == "deadline" {
					cause = fmt.Errorf("secret-token: %w", context.DeadlineExceeded)
				}
				failing := &handoffReadFailure{fakeIssues: l}
				e.Issues = failing
				if existing {
					// The merge lane re-enters; its fresh ownership read precedes the handoff read.
					e.Config.Merging = true
					failing.failure, failing.passes = cause, 1
				} else {
					e.GitHub = &handoffFailureAfterPR{fakeGitHub: g, tracker: failing, failure: cause}
				}
				e.Tools = &handoffBudgetTools{fakeTools: tools}
				sleeps := 0
				e.Sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
				result, err := e.Start(context.Background(), ticket)
				if err == nil {
					t.Fatal("missing read failure")
				}
				if kind == "global" {
					var global *infra.Global
					if !errors.As(err, &global) || global.Dep != infra.GitHub {
						t.Fatalf("lost global class: %v", err)
					}
				}
				want := context.Canceled
				if kind == "deadline" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) {
					t.Fatalf("lost context identity: %v", err)
				}
				for nested := err; nested != nil; nested = errors.Unwrap(nested) {
					if strings.Contains(nested.Error(), "secret-token") {
						t.Fatal("unsafe error payload escaped")
					}
				}
				if result.State.Infra != 1 || result.State.Corrections != 0 || sleeps != 1 {
					t.Fatalf("lost/spent budget: %+v sleeps=%d", result.State, sleeps)
				}
			})
		}
	}
}
func TestResumeMergeHandoffRetainsEarlierReadBudget(t *testing.T) {
	e, _, _, g, tools, ticket := fixture(t, strings.Repeat("b", 40))
	r, err := e.Execute(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	g.existing = &r.PR
	g.existing.Head.SHA = tools.head
	e.Tools = &handoffBudgetTools{fakeTools: tools}
	e.Sleep = func(context.Context, time.Duration) error { return nil }
	e.Config.Merging = true
	r, err = e.Start(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if r.State.Infra != 1 {
		t.Fatalf("resume reset earlier read budget: %+v", r.State)
	}
}
