// Package resolver supervises the agent that owns exceptional pipeline work.
// Its durable record belongs to the ticket ledger, not to a local side store.
package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

const Prefix = "resolver-state "

// StartUnobservedWindow bounds a dispatch whose turn Orca never saw start, as
// when the brief stays in the agent's input box: polling it without end held
// the maintenance session and blocked deploys (OF-160).
const StartUnobservedWindow = 10 * time.Minute

type Record struct {
	SourceRun string    `json:"source_run,omitempty"`
	Creator   string    `json:"creator,omitempty"`
	Version   int       `json:"version"`
	Attempt   int       `json:"attempt"`
	Phase     string    `json:"phase"`
	Run       string    `json:"run,omitempty"`
	Task      string    `json:"task,omitempty"`
	Dispatch  string    `json:"dispatch,omitempty"`
	Spec      string    `json:"spec"`
	Agent     string    `json:"agent"`
	Model     string    `json:"model,omitempty"`
	Effort    string    `json:"effort,omitempty"`
	Result    string    `json:"result,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
	RetryAt   time.Time `json:"retry_at,omitempty"`
	// UnobservedSince is when this dispatch was first seen pending without a
	// started turn; the ledger keeps it so a restart does not reopen the window.
	UnobservedSince time.Time `json:"unobserved_since,omitzero"`
	// Failures counts consecutive failed attempts before this one. It is derived
	// from the ledger history on read so receipts written before it existed count.
	Failures int `json:"-"`
}

// Undispatched marks a waiting round that ended before any dispatch reached
// Orca; it neither counts as a failure nor resets the failure count.
const Undispatched = "undispatched"

type Store interface {
	Load(context.Context) (*Record, error)
	Save(context.Context, Record) error
}
type Orca interface {
	RunCreate(context.Context, string, string) (orca.Receipt, error)
	RunUse(context.Context, string, string) (orca.Receipt, error)
	WorkerStart(context.Context, string, orca.StartOptions) (orca.Receipt, error)
	TaskList(context.Context, string) ([]orca.Task, error)
	WorkerRelease(context.Context, string, string) (orca.Receipt, error)
	WorkerList(context.Context, orca.ListOptions) (orca.Receipt, error)
	WorkerStop(context.Context, string, string) (orca.Receipt, error)
}
type Controller struct {
	Store                                              Store
	Orca                                               Orca
	Ticket, Repo, Spec, Agent, Model, Effort, Terminal string
	SourceRun                                          string
	// Board, when set, receives the resolver worktree once Orca places it.
	Board func(ctx context.Context, worktree string, attempt int)
	Now   func() time.Time
	// Heartbeat records the resolver's own Run for the scheduler; nil writes none.
	Heartbeat *orca.Heartbeat
}

// Step performs one recoverable observation/transition. Mutations reuse frozen
// request identities; an unavailable read cannot authorize another dispatch.
func (c *Controller) Step(ctx context.Context) (*Record, error) {
	if c.Store == nil || c.Orca == nil || c.Ticket == "" || c.Repo == "" || c.Spec == "" || c.Agent == "" || c.Terminal == "" {
		return nil, errors.New("invalid resolver configuration")
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	p, err := c.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	attempt := 1
	sourceRun := c.SourceRun
	if p != nil && p.SourceRun != "" {
		sourceRun = p.SourceRun
	}
	if p != nil {
		if p.Version != 1 || p.Attempt < 1 {
			return nil, errors.New("invalid resolver record")
		}
		switch p.Phase {
		case "waiting":
			if now.Before(p.RetryAt) {
				return p, nil
			}
			attempt = p.Attempt + 1
			p = nil
		case "result", "applied":
			return p, nil
		case "pending", "running", "settled":
		default:
			return nil, errors.New("unknown resolver phase")
		}
	}
	if p == nil {
		p = &Record{SourceRun: sourceRun, Creator: c.Terminal, Version: 1, Attempt: attempt, Phase: "pending", Spec: c.Spec, Agent: c.Agent, Model: c.Model, Effort: c.Effort}
		if p.Model == "" {
			p.Effort = ""
		}
		if err = c.Store.Save(ctx, *p); err != nil {
			return nil, err
		}
	}
	request := func(step string) string {
		return orca.RequestID(c.Repo+"/"+c.Ticket, "resolver", step+"-"+c.Terminal, p.Attempt)
	}
	if p.Run == "" {
		// No worker can have started: saving Run precedes the dispatch boundary.
		// A replacement may create an empty Run with its own caller-bound request.
		if p.Creator != c.Terminal {
			p.Creator = c.Terminal
			if err = c.Store.Save(ctx, *p); err != nil {
				return nil, err
			}
		}
		receipt, err := c.Orca.RunCreate(ctx, request("run"), "Resolver "+c.Ticket)
		if err != nil {
			return nil, err
		}
		p.Run = receiptID(receipt, "run")
		if p.Run == "" {
			return nil, errors.New("resolver run receipt missing identity")
		}
		if err = c.Store.Save(ctx, *p); err != nil {
			return nil, err
		}
	}
	c.Heartbeat.SetRun(p.Run)
	if _, err = c.Orca.RunUse(ctx, request("bind"), p.Run); err != nil {
		return nil, err
	}
	if p.Phase == "pending" && p.Creator != c.Terminal {
		if err = c.recoverStart(ctx, p); err != nil {
			return nil, err
		}
		if p.Phase == "waiting" {
			return p, nil
		}
	}
	if p.Phase == "pending" {
		receipt, err := c.Orca.WorkerStart(ctx, request("dispatch"), orca.StartOptions{Run: p.Run, Spec: p.Spec, Worktree: "new-top-level", Repo: "path:" + c.Repo, BaseBranch: "origin/main", Name: fmt.Sprintf("%s-resolver-%d", c.Ticket, p.Attempt), Setup: "skip", Agent: p.Agent, Model: p.Model, Effort: p.Effort})
		callErr := err
		if errors.Is(callErr, maintenance.ErrRefused) {
			// Nothing reached Orca: end the round so the next one starts a fresh
			// Run. Clearing only the Run left a record equal to the first pending
			// one, which a stale pre-write read could swallow (ZCP-141).
			p.Phase, p.Outcome, p.Result = "waiting", Undispatched, "maintenance refused dispatch before it reached Orca"
			c.Heartbeat.SetRun("")
			return nil, errors.Join(callErr, c.Store.Save(ctx, *p))
		}
		if path := orca.ReceiptWorktree(receipt); path != "" && c.Board != nil {
			c.Board(ctx, path, p.Attempt)
		}
		p.Task, p.Dispatch = receiptID(receipt, "task"), receiptID(receipt, "dispatch")
		if p.Task == "" || p.Dispatch == "" {
			if err = c.Store.Save(ctx, *p); err != nil {
				return nil, err
			}
			return nil, errors.Join(callErr, errors.New("resolver dispatch receipt missing identity"))
		}
		p.Phase = "running"
		if err = c.Store.Save(ctx, *p); err != nil {
			return nil, err
		}
		return p, callErr
	}
	if p.Phase == "running" {
		tasks, err := c.Orca.TaskList(ctx, p.Run)
		if err != nil {
			return nil, err
		}
		listed, status := false, ""
		if slices.ContainsFunc(tasks, func(t orca.Task) bool { return t.ID == p.Task && AwaitsDispatch(t) }) {
			row, err := c.dispatchRow(ctx, p)
			if err != nil {
				return nil, err
			}
			if listed = row != nil; listed {
				status = row.Status
			}
			// The dispatch may report between the two reads; only a task read
			// after the worker read can confirm loss.
			if tasks, err = c.Orca.TaskList(ctx, p.Run); err != nil {
				return nil, err
			}
		}
		found := false
		for _, task := range tasks {
			if task.ID != p.Task {
				continue
			}
			if found || (task.RunID != "" && task.RunID != p.Run) || (task.DispatchID != "" && task.DispatchID != p.Dispatch) {
				return nil, errors.New("resolver task identity mismatch")
			}
			found = true
			report, err := task.WorkerReport()
			if err != nil {
				return nil, err
			}
			if report == nil && AwaitsDispatch(task) {
				if reason, lost := LostDispatch(task, p.Dispatch, listed, status); lost {
					// Nothing of this round remains to release; the scheduler's
					// failure rules open the next round with a fresh request.
					p.Result, p.Outcome, p.Phase = reason, "failed", "result"
				}
				continue
			}
			if report == nil {
				switch task.Status {
				case "failed", "cancelled", "canceled":
					p.Result, p.Outcome, p.Phase = "task ended without a successful worker report", "failed", "settled"
				case "blocked":
					// An abandoned worker returns its task to blocked without a
					// dispatch or report (orca-task-list-abandoned.json).
					worker, err := c.worker(ctx, p)
					if err != nil {
						return nil, err
					}
					if worker.State == "abandoned" {
						p.Result, p.Outcome, p.Phase = "worker abandoned without a worker report", "failed", "settled"
					}
				}
				continue
			}
			if (task.Status != "completed" && task.Status != "failed") || (report.Outcome != "succeeded" && report.Outcome != "failed") {
				return nil, errors.New("resolver report is not settled")
			}
			p.Result, p.Outcome, p.Phase = report.Body, report.Outcome, "settled"
		}
		if !found {
			return nil, errors.New("resolver task absent from complete read")
		}
		if p.Phase == "running" {
			return p, c.awaitTurnStart(ctx, p, now, request("stop"))
		}
		if err = c.Store.Save(ctx, *p); err != nil {
			return nil, err
		}
	}
	if p.Phase == "settled" {
		released := false
		if p.Creator != "" && p.Creator != c.Terminal {
			worker, readErr := c.worker(ctx, p)
			if readErr != nil {
				return nil, readErr
			}
			released = worker.Resource.ReleaseState == "released"
		}
		if !released {
			_, err = c.Orca.WorkerRelease(ctx, request("release"), p.Dispatch)
			// The report is already persisted; an unprovable release only
			// leaves a terminal Orca can no longer resolve.
			if orca.IsReleaseUnknown(err) {
				runx.LoggerFrom(ctx).Warn("resolver worker release unknown", "dispatch", p.Dispatch, "reason", "release_unknown")
			} else if err != nil {
				return nil, err
			}
		}
		p.Phase = "result"
		if err = c.Store.Save(ctx, *p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// awaitTurnStart stops a dispatch whose turn stayed unobserved for the whole
// window and ends the round as waiting, so the scheduler dispatches afresh.
// Only a worker read after the stop showing the dispatch ended confirms it.
func (c *Controller) awaitTurnStart(ctx context.Context, p *Record, now time.Time, stopRequest string) error {
	row, err := c.dispatchRow(ctx, p)
	if err != nil || row == nil || !StartUnobserved(row.Status, row.Projection.Stage.Detail) {
		return err
	}
	if p.UnobservedSince.IsZero() {
		p.UnobservedSince = now
		return c.Store.Save(ctx, *p)
	}
	if now.Sub(p.UnobservedSince) < StartUnobservedWindow {
		return nil
	}
	_, stopErr := c.Orca.WorkerStop(ctx, stopRequest, p.Dispatch)
	if row, err = c.dispatchRow(ctx, p); err != nil {
		return errors.Join(stopErr, err)
	}
	if row == nil || !DispatchEnded(row.Status) {
		return errors.Join(stopErr, errors.New("resolver stop of unstarted dispatch unconfirmed"))
	}
	p.Phase, p.Outcome, p.RetryAt = "waiting", "failed", now
	p.Result = fmt.Sprintf("worker turn did not start within %s; dispatch %s stopped", StartUnobservedWindow, p.Dispatch)
	return c.Store.Save(ctx, *p)
}

// AwaitsDispatch reports a task Orca holds without a dispatch. That alone never
// proves loss: a dispatch queued behind a busy terminal looks the same.
func AwaitsDispatch(task orca.Task) bool {
	return (task.Status == "pending" || task.Status == "ready") && task.DispatchID == ""
}

// LostDispatch needs positive evidence from complete reads: the task awaits a
// dispatch and the recorded dispatch is absent from the workers or ended, as
// after an Orca restart drops it.
func LostDispatch(task orca.Task, dispatch string, listed bool, status string) (string, bool) {
	if !AwaitsDispatch(task) || (listed && !DispatchEnded(status)) {
		return "", false
	}
	return fmt.Sprintf("dispatch lost: task %s is %s without dispatch %s", task.ID, task.Status, dispatch), true
}

// StartUnobserved reports a dispatch Orca accepted whose turn it never saw
// start (ZCP-142: the brief stayed in the agent's input box).
func StartUnobserved(status, detail string) bool {
	return status == "pending" && detail == "turn_start_unobserved"
}

func DispatchEnded(status string) bool {
	switch status {
	case "completed", "failed", "abandoned", "cancelled", "canceled":
		return true
	}
	return false
}

func receiptID(receipt orca.Receipt, key string) string {
	var row struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(receipt[key], &row) == nil && row.ID != "" {
		return row.ID
	}
	var value string
	if json.Unmarshal(receipt[key+"Id"], &value) == nil {
		return value
	}
	return ""
}
