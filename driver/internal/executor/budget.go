package executor

import (
	"context"
	"errors"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
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
func (e *Executor) handoff(ctx context.Context, t Ticket, state machine.State, evidence string) error {
	if _, err := e.Linear.UpdateIssue(ctx, t.ID, "In Progress", []string{"auto-impl", "auto-failed"}, []string{"ready-for-agent"}); err != nil {
		return errors.New("human handoff labels failed")
	}
	body := "转人: " + state.HumanReason + "\n证据: " + evidence + "\n需要你: 检查保留的 worker 分支并重新排队"
	if err := e.comment(ctx, t.ID, body); err != nil {
		return errors.New("human handoff comment failed")
	}
	return nil
}

// retryRead retries only observations. An uncertain worker-start, child creation,
// or PR mutation must be reconciled, never blindly replayed by this budget.
func (e *Executor) retryRead(ctx context.Context, t Ticket, result *Result, step string, read func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := read()
		if err == nil {
			return nil
		}
		if stop := dependencyStop(ctx, err); stop != nil {
			return stop
		}
		// The machine exposes infra accounting through worker/session/review events.
		// Use its worker infra transition, restoring the observed phase unless exhausted.
		phase := result.State.Phase
		shadow := result.State
		shadow.Phase = machine.PhaseWorker
		shadow.ExecAlive = true
		next, _, stepErr := e.stepDecision(shadow, machine.EvWorkerStall, "budget", phase)
		if stepErr != nil {
			return stepErr
		}
		if next.Owner == machine.OwnerHuman {
			next.HumanReason = "infra budget exhausted: " + step
			result.State = next
			if err := e.handoff(ctx, t, next, step); err != nil {
				return err
			}
			return errBudgetExhausted
		}
		result.State.Infra = next.Infra
		result.State.Phase = phase
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
		return errors.New("global dependency unavailable")
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return nil
}
