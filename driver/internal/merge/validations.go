package merge

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Candidate is one merged result the lane wants to squash: the queue entry,
// its pinned head and main, and their merge-tree.
type Candidate struct {
	Request          Request
	Head, Main, Tree string
}

func (c Candidate) sameQueue(o Candidate) bool {
	return c.Request.QueueID == o.Request.QueueID && c.Head == o.Head
}

// GateRun runs a prepared merged-result gate in the validation worktree on a
// background goroutine, so it may only use values Prepare copied. nil is green,
// ErrGateRed is red, and anything else is infrastructure.
type GateRun func(ctx context.Context, worktree string) error

// Gate prepares the merged-result gate on the caller's goroutine.
type Gate interface {
	Prepare(context.Context, Candidate) (GateRun, error)
}

type Verdict string

const (
	VerdictRunning Verdict = "running"
	VerdictGreen   Verdict = "green"
	VerdictRed     Verdict = "red"
	// VerdictStale is a green gate on a main that has since moved. It is
	// returned once so the lane counts exactly one catch-up round for it.
	VerdictStale Verdict = "stale"
)

var ErrValidationsClosed = errors.New("merge validations closed")

// Validations runs at most one merged-result gate per repository beside the
// scheduler's inspections. Verdicts live only in this process: a restarted
// serve starts empty, so it reruns a gate and never infers green.
type Validations struct {
	mu     sync.Mutex
	runs   map[string]*validation
	base   context.Context
	stop   context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

type validation struct {
	candidate Candidate
	exited    bool
	err       error // nil green, ErrGateRed red, otherwise infrastructure
	cleanup   bool  // ErrWorktreeCleanup not yet reported
}

func NewValidations() *Validations {
	base, stop := context.WithCancel(context.Background())
	return &Validations{runs: map[string]*validation{}, base: base, stop: stop}
}

// Validate answers for c without waiting on a gate. A gate still running for
// the repository, even for an earlier queue head, answers Running and is never
// canceled: a killed gate.sh leaks its heavy lock. An exited gate for another
// queue entry is discarded and a gate for c starts. Green and red verdicts stay
// until another candidate replaces them, so a failed squash or correction write
// replays the same decision; infrastructure failures and Stale are consumed so
// the next call reruns the gate. A Red verdict reports the main it validated.
// ErrWorktreeCleanup accompanies the first Green or Red it is reported with.
func (v *Validations) Validate(ctx context.Context, c Candidate, git Git, gate Gate) (Verdict, string, error) {
	repo := c.Request.Repository
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return "", "", ErrValidationsClosed
	}
	if run := v.runs[repo]; run != nil {
		if !run.exited {
			v.mu.Unlock()
			return VerdictRunning, "", nil
		}
		if run.candidate.sameQueue(c) {
			verdict, main, err := run.verdict(c)
			if verdict != VerdictGreen && verdict != VerdictRed {
				delete(v.runs, repo)
			}
			v.mu.Unlock()
			return verdict, main, err
		}
		delete(v.runs, repo)
	}
	v.mu.Unlock()
	prepared, err := gate.Prepare(ctx, c)
	if err != nil {
		return "", "", err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return "", "", ErrValidationsClosed
	}
	run := &validation{candidate: c}
	v.runs[repo] = run
	v.wg.Add(1)
	go v.run(runx.WithLogger(v.base, runx.LoggerFrom(ctx)), run, git, prepared)
	return VerdictRunning, "", nil
}

// verdict reads an exited run for the same queue entry.
func (run *validation) verdict(c Candidate) (Verdict, string, error) {
	var cleanup error
	if run.cleanup {
		cleanup = ErrWorktreeCleanup
	}
	switch {
	case errors.Is(run.err, ErrGateRed):
		run.cleanup = false
		return VerdictRed, run.candidate.Main, cleanup
	case run.err != nil:
		return "", "", errors.Join(run.err, cleanup)
	case run.candidate.Main != c.Main:
		return VerdictStale, "", cleanup
	}
	run.cleanup = false
	return VerdictGreen, "", cleanup
}

// Idle reports whether no gate is running for c's repository. It discards an
// exited gate for another queue entry so that entry cannot reuse it later.
func (v *Validations) Idle(c Candidate) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	run := v.runs[c.Request.Repository]
	if run == nil {
		return true
	}
	if !run.exited {
		return false
	}
	if !run.candidate.sameQueue(c) {
		delete(v.runs, c.Request.Repository)
	}
	return true
}

// Forget drops c's exited verdict once the lane has spent it on a catch-up
// round, so the next round validates the new main instead of counting the
// same move again.
func (v *Validations) Forget(c Candidate) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if run := v.runs[c.Request.Repository]; run != nil && run.exited && run.candidate.sameQueue(c) {
		delete(v.runs, c.Request.Repository)
	}
}

func (v *Validations) run(ctx context.Context, run *validation, git Git, gate GateRun) {
	defer v.wg.Done()
	started := time.Now()
	cleanup, err := validateMergedResult(ctx, git, run.candidate, gate)
	result := "green"
	switch {
	case errors.Is(err, ErrGateRed):
		result = "red"
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		result = "canceled"
	case err != nil:
		result = "infrastructure"
	}
	attrs := []any{"ticket", run.candidate.Request.Ticket, "repository", run.candidate.Request.Repository, "queue_id", run.candidate.Request.QueueID, "duration_ms", time.Since(started).Milliseconds(), "result", result, "worktree_cleanup_failed", cleanup}
	if result == "infrastructure" {
		attrs = append(attrs, "err", err)
	}
	runx.LoggerFrom(ctx).Info("merge gate finished", attrs...)
	v.mu.Lock()
	run.exited, run.err, run.cleanup = true, err, cleanup
	v.mu.Unlock()
}

// validateMergedResult runs gate in a worktree holding exactly c's merged
// tree. A canceled context or a worktree that never existed is infrastructure.
func validateMergedResult(ctx context.Context, git Git, c Candidate, gate GateRun) (cleanup bool, err error) {
	gateRan := false
	var gateErr error
	err = git.WithWorktree(ctx, c.Request, c.Tree, c.Head, c.Main, func(ctx context.Context, path string) error {
		gateRan = true
		gateErr = gate(ctx, path)
		return gateErr
	})
	cleanup = errors.Is(err, ErrWorktreeCleanup)
	if ctx.Err() != nil {
		return cleanup, ctx.Err()
	}
	if !gateRan {
		return cleanup, errors.New("validation worktree unavailable")
	}
	return cleanup, gateErr
}

// Close cancels every gate (runx kills each process group), waits up to wait
// for their worktree cleanup, and refuses later validations. A second Close
// waits again.
func (v *Validations) Close(wait time.Duration) error {
	v.mu.Lock()
	v.closed = true
	v.mu.Unlock()
	v.stop()
	done := make(chan struct{})
	go func() {
		v.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return errors.New("merge gates still stopping")
	}
}
