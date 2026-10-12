package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

func (e *Executor) backoff(ctx context.Context, n int) error {
	if n < 1 || n >= machine.DefaultLimits.Infra {
		return nil
	}
	wait := e.Sleep
	if wait == nil {
		wait = pause
	}
	delay := time.Minute
	if n == 2 {
		delay = 5 * time.Minute
	}
	return wait(ctx, delay)
}
func (e *Executor) handoff(ctx context.Context, t Ticket, state machine.State, evidence, run string, cp *checkpoint) error {
	status.Push(e.Now(), func(view *status.View) {
		status.SetTicket(view, status.Ticket{Repo: e.Config.Repo, Identifier: t.Identifier}, false)
	})
	if _, err := e.Linear.UpdateIssue(ctx, t.ID, "In Progress", []string{"auto-impl", "auto-failed"}, []string{"ready-for-agent"}); err != nil {
		return errors.New("human handoff labels failed")
	}
	if run == "" {
		cp, err := readCheckpoint(e.Config, t)
		if err != nil {
			return err
		}
		if cp != nil {
			run = cp.Run
		}
		if run == "" {
			run = "-"
		}
	}
	e.writeRunLedger(ctx, t, "escalated", cp, state)
	body := "Agent处置: " + state.HumanReason + "\n证据: " + evidence + "\n处置要求: 检查保留的 worker 分支并完成处置" + fmt.Sprintf("\n阶段: %s\n归属: %s term=%s run=%s attempt=%d", machine.PhaseResolver, machine.OwnerResolver, e.Config.Terminal, run, state.Attempt)
	if err := e.comment(ctx, t.ID, body); err != nil {
		return errors.New("human handoff comment failed")
	}
	worktree := ""
	if cp != nil {
		worktree = cp.Worktree
	} else if saved, err := readCheckpoint(e.Config, t); err != nil {
		e.warn("%s Orca board checkpoint unavailable: %v", t.Identifier, err)
	} else if saved != nil {
		worktree = saved.Worktree
	}
	e.board(ctx, t, worktree, machine.PhaseResolver, state.HumanReason, 0)
	return nil
}

// writeRunLedger appends one ledger row for a finished run without blocking
// the handoff. Usage collection failure only omits the usage field.
func (e *Executor) writeRunLedger(ctx context.Context, t Ticket, outcome string, cp *checkpoint, state machine.State) {
	if cp == nil || e.Linear == nil {
		return
	}
	started, _ := time.Parse(time.RFC3339, cp.StartedAt)
	w := &LedgerWriter{
		Create: func(ctx context.Context, body string) error { return e.comment(ctx, t.ID, body) },
		List: func(ctx context.Context) ([]string, error) {
			comments, err := e.Linear.ListComments(ctx, t.ID)
			if err != nil {
				return nil, err
			}
			bodies := make([]string, 0, len(comments))
			for _, c := range comments {
				bodies = append(bodies, c.Body)
			}
			return bodies, nil
		},
		Collect: func(ctx context.Context, ticket string) (*usage.TicketUsage, error) {
			usageCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			u, err := usage.Collect(usageCtx, ticket, []string{cp.Worktree})
			if err != nil {
				return nil, err
			}
			return &u, nil
		},
		Now: e.Now,
	}
	w.Finish(ctx, LedgerRow{
		Ticket: t.Identifier, Lane: t.Lane, Tier: e.Config.Tier, Outcome: outcome,
		Corrections: state.Corrections, Minutes: e.Now().Sub(started).Minutes(),
		StartedAt: started, FinishedAt: e.Now().UTC(),
	})
}

// retryRead retries only observations. An uncertain worker-start, child creation,
// or PR mutation must be reconciled, never blindly replayed by this budget.
func (e *Executor) retryRead(ctx context.Context, t Ticket, result *Result, step string, read func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := read()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil
		}
		if stop := dependencyStop(ctx, err); stop != nil {
			return stop
		}
		next, _, stepErr := e.step(result.State, machine.EvInfraFailure)
		if stepErr != nil {
			return stepErr
		}

		if next.Owner == machine.OwnerResolver {
			next.HumanReason = "infra budget exhausted: " + step
			result.State = next
			if err := e.handoff(ctx, t, next, step, result.RunID, nil); err != nil {
				return err
			}
			return errBudgetExhausted
		}
		result.State = next
		if err := e.backoff(ctx, next.Infra); err != nil {
			return err
		}
	}
}

var errBudgetExhausted = errors.New("ticket error budget exhausted")

func dependencyStop(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var global *infra.Global
	if errors.As(err, &global) {
		return safeDependencyError(err)
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return nil
}

// safeDependencyError retains only classifications callers can safely inspect.
// External messages and arbitrary error payloads never cross this boundary.
func safeDependencyError(err error) error {
	if err == nil {
		return nil
	}
	var global *infra.Global
	if errors.As(err, &global) {
		return &infra.Global{Dep: global.Dep, Err: safeDependencyError(global.Err)}
	}
	var ticket *infra.Ticket
	if errors.As(err, &ticket) {
		return &infra.Ticket{Err: safeDependencyError(ticket.Err)}
	}
	var transient *infra.Transient
	if errors.As(err, &transient) {
		return &infra.Transient{Err: safeDependencyError(transient.Err)}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, infra.ErrWriteUnconfirmed) {
		return infra.ErrWriteUnconfirmed
	}
	var http infra.HTTPError
	if errors.As(err, &http) {
		return http
	}
	var httpPtr *infra.HTTPError
	if errors.As(err, &httpPtr) {
		return &infra.HTTPError{StatusCode: httpPtr.StatusCode}
	}
	return errors.New("dependency operation failed")
}
