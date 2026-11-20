// Package resolver supervises the agent that owns exceptional pipeline work.
// Its durable record belongs to the ticket ledger, not to a local side store.
package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

const Prefix = "resolver-state "

// StartUnobservedWindow bounds a dispatch whose turn Orca never saw start, as
// when the brief stays in the agent's input box: polling it without end held
// the maintenance session and blocked deploys (OF-160).
const StartUnobservedWindow = 10 * time.Minute

// NoProgressWindow bounds a started resolver round with no terminal output.
const NoProgressWindow = 15 * time.Minute

// ModelCheckDelay is how long a round's terminal stays quiet before its screen
// is read for a model limit or unsupported-model error (OF-594).
const ModelCheckDelay = time.Minute

// modelScreenLines bounds the rendered screen rows read for a model error, so
// ticket text the agent printed earlier cannot match.
const modelScreenLines = 20

type Record struct {
	TopLaunchAccepted bool      `json:"top_launch_accepted,omitempty"`
	TopModelAccepted  bool      `json:"top_model_accepted,omitempty"`
	TopLaunchError    string    `json:"top_launch_error,omitempty"`
	Top               bool      `json:"top,omitempty"`
	TopUsed           bool      `json:"top_used,omitempty"`
	TopReason         string    `json:"top_reason,omitempty"`
	TopClass          string    `json:"top_class,omitempty"`
	SourceRun         string    `json:"source_run,omitempty"`
	Creator           string    `json:"creator,omitempty"`
	Version           int       `json:"version"`
	Attempt           int       `json:"attempt"`
	Phase             string    `json:"phase"`
	Run               string    `json:"run,omitempty"`
	Task              string    `json:"task,omitempty"`
	Dispatch          string    `json:"dispatch,omitempty"`
	Spec              string    `json:"spec"`
	Agent             string    `json:"agent"`
	Model             string    `json:"model,omitempty"`
	Effort            string    `json:"effort,omitempty"`
	Result            string    `json:"result,omitempty"`
	Outcome           string    `json:"outcome,omitempty"`
	RetryAt           time.Time `json:"retry_at,omitempty"`
	// UnobservedSince is when this dispatch was first seen pending without a
	// started turn; the ledger keeps it so a restart does not reopen the window.
	UnobservedSince time.Time `json:"unobserved_since,omitzero"`
	ProgressSince   time.Time `json:"progress_since,omitzero"`
	LastOutputAt    int64     `json:"last_output_at,omitempty"`
	// Failures counts consecutive failed attempts before this one. It is derived
	// from the ledger history on read so receipts written before it existed count.
	Failures int `json:"-"`
}

// Undispatched marks a waiting round that ended before any dispatch reached
// Orca; it neither counts as a failure nor resets the failure count.
const Undispatched = "undispatched"

// ModelUnavailable marks a waiting round its agent's model ended: the model is
// recorded unavailable and the round counts as no failure of the ticket.
const ModelUnavailable = "model-unavailable"

type ModelRecorder interface {
	Record(modelhealth.Status) error
}

type Store interface {
	Load(context.Context) (*Record, error)
	Save(context.Context, Record) error
}
type Orca interface {
	RunCreate(context.Context, string, string) (orca.Receipt, error)
	RunUse(context.Context, string, string) (orca.Receipt, error)
	WorkerStart(context.Context, string, orca.StartOptions) (orca.Receipt, error)
	TaskList(context.Context, string) ([]orca.Task, error)
	TaskListing(context.Context, string) (orca.TaskListing, error)
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
	// Models receives a model the round's screen shows unavailable; nil skips the screen read.
	Models ModelRecorder
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
	top, topUsed, topReason, topClass := false, false, "", ""
	if p != nil {
		top, topUsed, topReason, topClass = p.Top, p.TopUsed, p.TopReason, p.TopClass
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
		p = &Record{Top: top, TopUsed: topUsed, TopReason: topReason, TopClass: topClass, SourceRun: sourceRun, Creator: c.Terminal, Version: 1, Attempt: attempt, Phase: "pending", Spec: c.Spec, Agent: c.Agent, Model: c.Model, Effort: c.Effort}
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
		if p.Top {
			var launch struct {
				Effective struct{ Agent, Model, Effort string }
			}
			if json.Unmarshal(receipt["launch"], &launch) == nil && launch.Effective.Agent == p.Agent && launch.Effective.Model == p.Model && launch.Effective.Effort == p.Effort {
				p.TopLaunchAccepted = true
			} else {
				p.TopLaunchError = "终审模型未被接受或无法核实，不更换模型"
			}
		}
		p.Phase = "running"
		if err = c.Store.Save(ctx, *p); err != nil {
			return nil, err
		}
		return p, callErr
	}
	if p.Phase == "running" && p.Top && !p.TopLaunchAccepted {
		_, stopErr := c.Orca.WorkerStop(ctx, request("stop"), p.Dispatch)
		row, readErr := c.dispatchRow(ctx, p)
		if readErr != nil {
			return nil, errors.Join(stopErr, readErr)
		}
		if row == nil || !DispatchEnded(row.Status) {
			return nil, errors.Join(stopErr, errors.New("终审模型未确认，停止派工未核实"))
		}
		result, _ := json.Marshal(Resolution{Action: "escalate", Reason: "终审模型未被接受或无法核实，不更换模型", Body: "指定档位未获完整接受回执；已停止并核实 dispatch " + p.Dispatch})
		p.Result, p.Outcome, p.Phase = string(result), "failed", "settled"
		if err = c.Store.Save(ctx, *p); err != nil {
			return nil, err
		}
	}
	if p.Phase == "running" {
		task, err := c.readTask(ctx, p)
		if err != nil {
			return nil, err
		}
		report, err := task.WorkerReport()
		if err != nil {
			return nil, err
		}
		var row *workerIdentity
		if report == nil {
			row, err = c.dispatchRow(ctx, p)
			if err != nil {
				return nil, err
			}
			// A report can arrive during the worker read; read the task afterwards.
			task, err = c.readTask(ctx, p)
			if err != nil {
				return nil, err
			}
			report, err = task.WorkerReport()
			if err != nil {
				return nil, err
			}
		}
		status := ""
		if row != nil {
			status = row.Status
		}
		switch {
		case report != nil:
			if err := p.acceptReport(*task, report); err != nil {
				return nil, err
			}
		case AwaitsDispatch(*task):
			if reason, lost := LostDispatch(*task, p.Dispatch, row != nil, status); lost {
				p.Result, p.Outcome, p.Phase = reason, "failed", "result"
			}
		case DispatchEnded(status):
			p.Result, p.Outcome, p.Phase = "dispatch ended without a worker report", "failed", "settled"
		case task.Status == "failed" || task.Status == "cancelled" || task.Status == "canceled":
			p.Result, p.Outcome, p.Phase = "task ended without a successful worker report", "failed", "settled"
		case task.Status == "blocked":
			worker, err := c.worker(ctx, p)
			if err != nil {
				return nil, err
			}
			if worker.State == "abandoned" {
				p.Result, p.Outcome, p.Phase = "worker abandoned without a worker report", "failed", "settled"
			}
		}
		if p.Phase == "running" {
			if err := c.awaitTurnStart(ctx, p, now, request("stop")); err != nil || p.Phase == "waiting" {
				return p, err
			}
			if err := c.awaitProgress(ctx, p, now, request("stop")); err != nil || p.Phase == "running" || p.Phase == "waiting" {
				return p, err
			}
		}
		if p.Top && !p.TopModelAccepted {
			result, _ := json.Marshal(Resolution{Action: "escalate", Reason: "终审模型未获响应，不更换模型", Body: p.Result})
			p.Result, p.Outcome = string(result), "failed"
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

func (c *Controller) awaitProgress(ctx context.Context, p *Record, now time.Time, stopRequest string) error {
	observer, ok := c.Orca.(interface {
		Workers(context.Context, orca.ListOptions) ([]orca.Worker, error)
		TerminalList(context.Context) ([]orca.Terminal, error)
	})
	if !ok {
		return errors.New("resolver progress observation unavailable")
	}
	workers, err := observer.Workers(ctx, orca.ListOptions{Run: p.Run, IncludeRemote: true})
	if err != nil {
		return err
	}
	terminal := ""
	for _, worker := range workers {
		if worker.DispatchID == p.Dispatch && worker.TaskID == p.Task && worker.DispatchStatus == "dispatched" {
			terminal = worker.AgentTerminalHandle
		}
	}
	if terminal == "" {
		return nil
	}
	terminals, err := observer.TerminalList(ctx)
	if err != nil {
		return err
	}
	var output int64
	for _, row := range terminals {
		if row.Handle == terminal {
			output = row.LastOutputAt
		}
	}
	if output <= 0 || time.UnixMilli(output).After(now) {
		if p.ProgressSince.IsZero() {
			return nil
		}
		p.ProgressSince, p.LastOutputAt = time.Time{}, 0
		return c.Store.Save(ctx, *p)
	}
	if p.ProgressSince.IsZero() || output != p.LastOutputAt {
		p.ProgressSince, p.LastOutputAt = now, output
		return c.Store.Save(ctx, *p)
	}
	if c.Models != nil && now.Sub(p.ProgressSince) >= ModelCheckDelay {
		status, err := c.screenModel(ctx, p, now)
		if err != nil || status.State != modelhealth.Unavailable {
			return err
		}
		if err = c.Models.Record(status); err != nil {
			return err
		}
		if stopped, err := c.stopQuiet(ctx, p, stopRequest); err != nil || !stopped {
			return err
		}
		p.Phase, p.Outcome, p.RetryAt = "waiting", ModelUnavailable, now
		p.Result = fmt.Sprintf("model unavailable: %s %s until %s; dispatch %s stopped", status.Ref, status.Reason, status.Until.UTC().Format(time.RFC3339), p.Dispatch)
		return c.Store.Save(ctx, *p)
	}
	if now.Sub(p.ProgressSince) < NoProgressWindow {
		return nil
	}
	if stopped, err := c.stopQuiet(ctx, p, stopRequest); err != nil || !stopped {
		return err
	}
	p.Result = fmt.Sprintf("worker made no progress for %s; dispatch %s stopped; last output %s", NoProgressWindow, p.Dispatch, time.UnixMilli(output).UTC().Format(time.RFC3339))
	p.Outcome, p.Phase = "failed", "settled"
	return nil
}

// stopQuiet stops a quiet dispatch unless a report settles the round first.
// It reports true only when a worker read after the stop shows it ended.
func (c *Controller) stopQuiet(ctx context.Context, p *Record, stopRequest string) (bool, error) {
	if reported, err := c.readReport(ctx, p); err != nil || reported {
		return false, err
	}
	_, stopErr := c.Orca.WorkerStop(ctx, stopRequest, p.Dispatch)
	row, err := c.dispatchRow(ctx, p)
	if err != nil {
		return false, errors.Join(stopErr, err)
	}
	if row == nil || !DispatchEnded(row.Status) {
		return false, errors.Join(stopErr, errors.New("resolver stop of stalled dispatch unconfirmed"))
	}
	reported, err := c.readReport(ctx, p)
	return !reported && err == nil, err
}

// screenModel classifies the bottom of the round's rendered screen.
func (c *Controller) screenModel(ctx context.Context, p *Record, now time.Time) (modelhealth.Status, error) {
	reader, ok := c.Orca.(interface {
		WorkerRead(context.Context, orca.ReadOptions) (orca.Receipt, error)
	})
	if !ok {
		return modelhealth.Status{}, errors.New("resolver screen read unavailable")
	}
	receipt, err := reader.WorkerRead(ctx, orca.ReadOptions{Dispatch: p.Dispatch, Source: "terminal", Limit: modelScreenLines})
	if err != nil {
		return modelhealth.Status{}, err
	}
	var screen struct{ Tail []string }
	if err = json.Unmarshal(receipt["terminal"], &screen); err != nil {
		return modelhealth.Status{}, fmt.Errorf("resolver screen read: %w", err)
	}
	lines := screen.Tail[max(0, len(screen.Tail)-modelScreenLines):]
	ref := modelhealth.Ref{Agent: p.Agent, Model: p.Model, Effort: p.Effort}
	return modelhealth.Classify(ref, strings.Join(lines, "\n"), nil, now), nil
}

func (p *Record) acceptReport(task orca.Task, report *orca.WorkerReport) error {
	if (task.Status != "completed" && task.Status != "failed") || (report.Outcome != "succeeded" && report.Outcome != "failed") {
		return errors.New("resolver report is not settled")
	}
	p.Result, p.Outcome, p.Phase = report.Body, report.Outcome, "settled"
	if p.Top && p.TopLaunchAccepted {
		p.TopModelAccepted = true
	}
	return nil
}

func (c *Controller) readReport(ctx context.Context, p *Record) (bool, error) {
	task, err := c.readTask(ctx, p)
	if err != nil {
		return false, err
	}
	report, err := task.WorkerReport()
	if err != nil || report == nil {
		return false, err
	}
	return true, p.acceptReport(*task, report)
}

func (c *Controller) readTask(ctx context.Context, p *Record) (*orca.Task, error) {
	tasks, err := c.Orca.TaskList(ctx, p.Run)
	if err != nil {
		return nil, err
	}
	var found *orca.Task
	for _, task := range tasks {
		if task.ID != p.Task {
			continue
		}
		if found != nil || (task.RunID != "" && task.RunID != p.Run) || (task.DispatchID != "" && task.DispatchID != p.Dispatch) {
			return nil, errors.New("resolver task identity mismatch")
		}
		found = &task
	}
	if found == nil {
		return nil, errors.New("resolver task absent from complete read")
	}
	return found, nil
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
