// Package executor drives one ticket through the state machine to merge ownership.
package executor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type Orca interface {
	RunCreate(context.Context, string, string) (orca.Receipt, error)
	RunUse(context.Context, string, string) (orca.Receipt, error)
	WorkerStart(context.Context, string, orca.StartOptions) (orca.Receipt, error)
	TaskList(context.Context, string) ([]orca.Task, error)
	Workers(context.Context, orca.ListOptions) ([]orca.Worker, error)
	WorkerStop(context.Context, string, string) (orca.Receipt, error)
	TerminalList(context.Context) ([]orca.Terminal, error)
	TerminalSend(context.Context, string, string) (orca.Receipt, error)
	WorkerRelease(context.Context, string, string) (orca.Receipt, error)
	WorktreeSet(context.Context, string, string, string, string) (orca.Receipt, error)
}
type Linear interface {
	ListComments(context.Context, string) ([]linear.Comment, error)
	CreateComment(context.Context, string, string) (linear.Comment, error)
	UpdateIssue(context.Context, string, string, []string, []string) (linear.Issue, error)
}
type GitHub interface {
	FindOpenPR(context.Context, string, string, string) (*github.PullRequest, error)
	CreatePR(context.Context, string, string, github.CreatePRRequest) (github.PullRequest, error)
}

// Tools isolates git, gate, and reviewer execution from the transition loop.
type Tools interface {
	Baseline(context.Context, string) (string, error)
	Head(context.Context, string, string) (string, error)
	ChangedFiles(context.Context, string, string, string) ([]string, error)
	MergeBase(context.Context, string, string) (string, error)
	Gate(context.Context, string, string, string) (string, error)
	Review(context.Context, string, string, string, *float64) (string, string, error)
}
type Ticket struct {
	Estimate                                             *float64
	ID, Identifier, Title, Todo, Check, Scope, TestFiles string
	Lane                                                 int
	NewTest                                              bool
}
type Config struct {
	Lane3Globs                                                          []string
	Machine                                                             string
	TierSettings                                                        map[string]string
	Escalate                                                            string
	Repo, Owner, Repository, Terminal, Unit, Root, Agent, Model, Effort string
	PollInterval                                                        time.Duration
	WorkerIdleTimeout                                                   time.Duration
	Tier                                                                string
}
type Executor struct {
	Followups     FollowupLinear
	FollowupTools FollowupTools
	Orca          Orca
	Linear        Linear
	GitHub        GitHub
	Tools         Tools
	Config        Config
	Now           func() time.Time
	Recovery      Recovery
	Sleep         func(context.Context, time.Duration) error
	// Stderr receives best-effort warnings; nil means os.Stderr.
	Stderr io.Writer
}
type Result struct {
	State             machine.State
	RunID, DispatchID string
	PR                github.PullRequest
}

// boardStatus maps a phase to an Orca board column so the desktop shows executor progress.
func boardStatus(p machine.Phase) string {
	switch p {
	case machine.PhaseWorker, machine.PhaseCorrection:
		return "in-progress"
	case machine.PhaseMerged:
		return "completed"
	}
	return "in-review"
}

func (e *Executor) warn(format string, args ...any) {
	w := e.Stderr
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "warning: "+format+"\n", args...)
}

// Execute stops at merge ownership; it never merges a PR. Errors are deliberately
// generic because external error messages and receipts may contain credentials.
func (e *Executor) Execute(ctx context.Context, t Ticket) (Result, error) {
	return e.execute(ctx, t, nil)
}

func (e *Executor) execute(ctx context.Context, t Ticket, restored *checkpoint) (result Result, err error) {
	result.State = machine.Initial()
	defer func() {
		if errors.Is(err, errBudgetExhausted) {
			err = nil
		}
	}()
	c := e.Config
	if c.Unit == "" {
		c.Unit = "executor"
	}
	if c.Agent == "" {
		c.Agent = "codex"
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.WorkerIdleTimeout <= 0 {
		c.WorkerIdleTimeout = 15 * time.Minute
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if !component.MatchString(t.Identifier) || !component.MatchString(c.Unit) || !component.MatchString(c.Repository) || c.Repo == "" || c.Terminal == "" || t.ID == "" || t.Todo == "" || t.Check == "" || t.Scope == "" || strings.ContainsAny(t.Check+t.Scope+t.TestFiles, "\r\n") || (t.NewTest && t.TestFiles == "") || t.Lane < 1 || t.Lane > 2 {
		return result, errors.New("invalid executor configuration or ticket brief")
	}
	if e.Orca == nil || e.Linear == nil || e.GitHub == nil || e.Tools == nil {
		return result, errors.New("missing executor dependency")
	}
	if c.Root == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return result, errors.New("cannot resolve state root")
		}
		c.Root = filepath.Join(home, ".local", "state", "orca-factory")
	}
	if err := e.restoreFailLog(ctx, c, t); err != nil {
		return result, err
	}
	branch := "agent/" + t.Identifier + "/" + c.Unit
	request := func(step string) string { return scopedRequest(t, c, step+"-"+result.DispatchID, 1) }
	releaseAttempted := false
	releaseAllowed := false
	release := func() error {
		if result.DispatchID == "" || releaseAttempted || !releaseAllowed {
			return nil
		}
		releaseAttempted = true
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		// A settled dispatch no longer needs its worker; a failed release only leaks
		// an idle worker and must not fail the ticket.
		if _, releaseErr := e.Orca.WorkerRelease(cleanup, request("release"), result.DispatchID); releaseErr != nil {
			e.warn("%s worker release failed for dispatch %s: %v", t.Identifier, result.DispatchID, releaseErr)
		}
		return nil
	}
	defer func() { err = errors.Join(err, release()) }()
	// diffBase is the head's merge-base with current main: a worker that rebased
	// must not be charged for main's commits since the claim baseline.
	var baseline, diffBase, taskID, spec, brief, head, failureOutput, reviewerTier string
	cp := checkpoint{}
	if restored != nil {
		cp = *restored
	}
	// Transition authorization always precedes the I/O belonging to the next phase.
	transition := func(ev machine.Event, action func([]machine.Effect) error) error {
		previous := result.State
		next, fx, stepErr := machine.Step(previous, ev, machine.DefaultLimits)
		if stepErr != nil {
			return stepErr
		}
		if action != nil {
			if actionErr := action(fx); actionErr != nil {
				return actionErr
			}
		}
		result.State = next
		if next.Phase == machine.PhaseHuman && (next.Infra >= machine.DefaultLimits.Infra || next.Corrections > machine.DefaultLimits.Corrections) {
			evidence := string(previous.Phase)
			if previous.Phase == machine.PhaseReview {
				evidence += "；审查命令档位: " + reviewerTier
			}
			return e.handoff(ctx, t, next, evidence)
		}
		if next.Infra > previous.Infra {
			if err := e.backoff(ctx, next.Infra); err != nil {
				return err
			}
		}
		if previous.Phase == next.Phase && previous.Owner == next.Owner {
			return nil
		}
		body := "阶段: " + string(next.Phase)
		if previous.Phase == machine.PhaseReview {
			body += "\n审查命令档位: " + reviewerTier
		}
		if next.Phase == machine.PhaseHuman {
			body = "转人: " + next.HumanReason + "\n证据: " + string(previous.Phase) + "\n需要你: 检查 worker 分支并重新排队\n" + body
		}
		if ev == machine.EvOpenPR {
			body = "PR 已开: " + result.PR.HTMLURL + "\n" + body
		}
		if previous.Owner != next.Owner {
			body += fmt.Sprintf("\n归属: %s term=%s run=%s attempt=%d since=%s", next.Owner, c.Terminal, result.RunID, next.Attempt, e.Now().UTC().Format(time.RFC3339))
		}
		if next.Phase == machine.PhaseCorrection {
			summary, err := e.failSummary(ctx, c, cmp.Or(diffBase, baseline), branch, head, failureOutput, ev)
			if err != nil {
				return err
			}
			if summary != "" {
				if err := appendFailLog(c, t, summary); err != nil {
					return err
				}
				body += "\n" + summary
			}
		}
		if commentErr := e.comment(ctx, t.ID, body); commentErr != nil {
			return errors.New("Linear transition comment failed")
		}
		if cp.Worktree != "" {
			if _, boardErr := e.Orca.WorktreeSet(ctx, cp.Worktree, t.Identifier, boardStatus(next.Phase), "driver: "+string(next.Phase)); boardErr != nil {
				e.warn("%s Orca board status update failed: %v", t.Identifier, boardErr)
			}
		}
		return nil
	}

	err = transition(machine.EvClaim, func(fx []machine.Effect) error {
		if !hasEffect(fx, "worker-start-with-request-id") {
			return errors.New("claim lacks worker effect")
		}
		var receipt orca.Receipt
		var callErr error
		if cp.Run != "" {
			// A new executor terminal must take over the checkpointed Run, not create one.
			if _, callErr = e.Orca.RunUse(ctx, runRequest(t, c, "run-use"), cp.Run); callErr != nil {
				return fmt.Errorf("Orca run takeover failed; reconcile request before retry: %w", callErr)
			}
			result.RunID = cp.Run
		} else {
			receipt, callErr = e.Orca.RunCreate(ctx, runRequest(t, c, "run"), t.Identifier+" "+t.Title)
			if callErr != nil {
				return fmt.Errorf("Orca run creation failed; reconcile request before retry: %w", callErr)
			}
			result.RunID = receiptString(receipt, "runId")
			if result.RunID == "" {
				result.RunID = nestedString(receipt, "run", "id")
			}
			if result.RunID == "" {
				return errors.New("Orca run receipt missing ID")
			}
		}
		baseline, callErr = e.Tools.Baseline(ctx, c.Repo)
		if callErr != nil || baseline == "" {
			return errors.New("baseline lookup failed")
		}
		issue, callErr := e.Linear.UpdateIssue(ctx, t.ID, "In Progress", []string{"auto-impl"}, nil)
		if callErr != nil {
			return errors.New("Linear claim failed")
		}
		claimed := false
		for _, label := range issue.Labels.Nodes {
			if label.Name == "auto-impl" {
				claimed = true
			}
		}
		if !claimed {
			return errors.New("Linear claim was not confirmed")
		}
		rendered, gate, writeErr := writeArtifacts(c, t, branch)
		if writeErr != nil {
			return writeErr
		}
		spec = gate
		brief = rendered
		if cp.Baseline != "" {
			baseline = cp.Baseline
		}
		cp.Run = result.RunID
		cp.Baseline = baseline
		if restored != nil {
			taskID = cp.Task
			result.DispatchID = cp.Dispatch
			return nil
		}
		cp.Pending = true
		if err := saveCheckpoint(c, t, cp); err != nil {
			return err
		}
		receipt, callErr = e.Orca.WorkerStart(ctx, scopedRequest(t, c, "worker", 1), orca.StartOptions{Run: result.RunID, Spec: brief, Worktree: "new-top-level", Repo: "path:" + c.Repo, BaseBranch: baseline, Name: t.Identifier + "-" + c.Unit, Agent: c.Agent, Model: c.Model, Effort: c.Effort})
		result.DispatchID = receiptString(receipt, "dispatchId")
		taskID = receiptString(receipt, "taskId")
		if callErr != nil {
			return fmt.Errorf("worker start failed; reconcile dispatch before retry: %w", callErr)
		}
		if result.DispatchID == "" || taskID == "" {
			return errors.New("worker receipt missing task or dispatch")
		}
		cp.Task = taskID
		cp.Dispatch = result.DispatchID
		cp.Worktree = worktreeReceipt(receipt)
		cp.Pending = false
		return saveCheckpoint(c, t, cp)
	})
	if err != nil {
		return result, err
	}
	result.State.Corrections = cp.Corrections
	for {
		head, failureOutput = "", ""
		report := &orca.WorkerReport{Outcome: "succeeded"}
		var waitErr error
		var idle workerIdle
		watch := workerWatch{stopRequest: scopedRequest(t, c, "stop", result.State.Corrections+1), observe: func(w orca.Worker) error {
			if cp.Worktree != "" || w.WorktreePath() == "" {
				return nil
			}
			next := cp
			next.Worktree = w.WorktreePath()
			if err := saveCheckpoint(c, t, next); err != nil {
				return err
			}
			cp = next
			return nil
		}}
		observeIdle := func() error {
			if !idle.observe(ctx, e, cp.Worktree, c.WorkerIdleTimeout, result.State) {
				return nil
			}
			if err := transition(machine.EvWorkerStall, nil); err != nil {
				return err
			}
			if result.State.Owner == machine.OwnerHuman {
				return errBudgetExhausted
			}
			return nil
		}
		if !cp.Completed {
			err = e.retryRead(ctx, t, &result, "task polling", func() error {
				report, waitErr = e.wait(ctx, result.RunID, taskID, result.DispatchID, c.PollInterval, &watch, observeIdle)
				// The stall transition already accounts for exhaustion and handoff.
				if result.State.Owner == machine.OwnerHuman {
					return nil
				}
				return waitErr
			})
			if err != nil {
				return result, err
			}
			if result.State.Owner == machine.OwnerHuman {
				return result, waitErr
			}
			releaseAllowed = report.Provenance != "executor_liveness"
		}
		if cp.Completed {
			releaseAllowed = cp.Settled
		}
		cp.Completed = false
		if waitErr != nil {
			return result, waitErr
		}
		failureCause := "worker failed"
		if report.Provenance == "executor_liveness" {
			failureCause = report.Body
			failureOutput = report.Body
		}
		event := machine.EvWorkerOK
		if report.Outcome != "succeeded" {
			event = machine.EvWorkerFail
		} else {
			e.processFollowups(ctx, t, report.Body)
		}
		if err = transition(event, nil); err != nil {
			return result, err
		}
		// A settled report authorizes cleanup, including before a correction handoff.
		if err = release(); err != nil {
			return result, err
		}
		if result.State.Phase == machine.PhaseCorrection {
			again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, failureCause, transition)
			if correctionErr != nil {
				return result, correctionErr
			}
			if !again {
				return result, nil
			}
			taskID = cp.Task
			releaseAttempted = false
			releaseAllowed = false
			continue
		}
		if result.State.Owner == machine.OwnerHuman {
			return result, nil
		}
		err = e.retryRead(ctx, t, &result, "branch lookup", func() error { var readErr error; head, readErr = e.Tools.Head(ctx, c.Repo, branch); return readErr })
		if err != nil {
			return result, err
		}
		event = machine.EvBranchOK
		if head == "" || head == baseline {
			event = machine.EvBranchNo
		} else {
			err = e.retryRead(ctx, t, &result, "merge-base lookup", func() error {
				var readErr error
				diffBase, readErr = e.Tools.MergeBase(ctx, c.Repo, head)
				return readErr
			})
			if err != nil {
				return result, err
			}
		}
		if err = transition(event, nil); err != nil || result.State.Owner == machine.OwnerHuman {
			return result, err
		}
		var output, verdict string
		var gateErr, parseErr error
		err = e.retryRead(ctx, t, &result, "gate output", func() error {
			output, gateErr = e.Tools.Gate(ctx, c.Repo, head, spec)
			if stop := dependencyStop(ctx, gateErr); stop != nil {
				if gateErr != nil {
					return gateErr
				}
				return stop
			}
			verdict, parseErr = gateVerdict(output)
			if parseErr != nil {
				if gateErr != nil {
					return gateErr
				}
				return parseErr
			}
			return nil
		})
		if err != nil {
			return result, err
		}
		failureOutput = output
		event = machine.EvGateRed
		if gateErr == nil && verdict == "green" {
			event = machine.EvGateGreen
		}
		if err = transition(event, nil); err != nil {
			return result, err
		}
		if result.State.Phase == machine.PhaseCorrection {
			again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, "gate red", transition)
			if correctionErr != nil {
				return result, correctionErr
			}
			if !again {
				return result, nil
			}
			taskID = cp.Task
			releaseAttempted = false
			releaseAllowed = false
			continue
		}
		if result.State.Owner == machine.OwnerHuman {
			return result, nil
		}
		for result.State.Phase == machine.PhaseReview {
			var reviewErr error
			output, reviewerTier, reviewErr = e.Tools.Review(ctx, c.Repo, diffBase, head, t.Estimate)
			if stop := dependencyStop(ctx, reviewErr); stop != nil {
				return result, stop
			}
			verdict, parseErr = reviewVerdict(output)
			event = machine.EvReviewEmpty
			if reviewErr == nil && parseErr == nil {
				if verdict == "green" {
					event = machine.EvReviewGreen
				} else {
					event = machine.EvReviewRed
				}
			}
			failureOutput = output
			if err = transition(event, nil); err != nil {
				return result, err
			}
		}
		if result.State.Owner == machine.OwnerHuman {
			return result, nil
		}
		if result.State.Phase == machine.PhaseCorrection {
			again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, "reviewer red", transition)
			if correctionErr != nil {
				return result, correctionErr
			}
			if !again {
				return result, nil
			}
			taskID = cp.Task
			releaseAttempted = false
			releaseAllowed = false
			continue
		}
		break
	}
	err = transition(machine.EvOpenPR, func(fx []machine.Effect) error {
		if !hasEffect(fx, "open-pr-rest-idempotent-by-branch") {
			return errors.New("missing PR effect")
		}
		pr, callErr := e.GitHub.FindOpenPR(ctx, c.Owner, c.Repository, branch)
		if callErr != nil {
			return errors.New("PR lookup failed")
		}
		if pr != nil {
			result.PR = *pr
		} else {
			result.PR, callErr = e.GitHub.CreatePR(ctx, c.Owner, c.Repository, github.CreatePRRequest{Head: branch, Base: "main", Title: t.Identifier + " " + t.Title, Body: "Fixes " + t.Identifier + "\n\n" + t.Todo})
			if callErr != nil {
				return errors.New("PR creation failed; reconcile branch before retry")
			}
		}
		if result.PR.Number <= 0 || result.PR.HTMLURL == "" {
			return errors.New("invalid PR receipt")
		}
		return nil
	})
	return result, err
}

var component = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func hasEffect(fx []machine.Effect, want machine.Effect) bool {
	for _, f := range fx {
		if f == want {
			return true
		}
	}
	return false
}
func receiptString(r orca.Receipt, key string) string {
	var s string
	_ = json.Unmarshal(r[key], &s)
	return s
}
func nestedString(r orca.Receipt, key, field string) string {
	var inner orca.Receipt
	_ = json.Unmarshal(r[key], &inner)
	return receiptString(inner, field)
}

const workerTurnStartTimeout = 5 * time.Minute

// One watch per dispatch, retained across read retries; restart begins a fresh
// observation period. An uncertain stop is never issued a second time.
type workerWatch struct {
	unobservedSince time.Time
	stopAttempted   bool
	stopErr         error
	stopRequest     string
	observe         func(orca.Worker) error
}

func (e *Executor) wait(ctx context.Context, run, task, dispatch string, interval time.Duration, watch *workerWatch, observeIdle func() error) (*orca.WorkerReport, error) {
	for {
		tasks, err := e.Orca.TaskList(ctx, run)
		if err != nil {
			return nil, errors.New("task polling failed")
		}
		completed := false
		var report *orca.WorkerReport
		for _, t := range tasks {
			// task-list rows carry no dispatch_id; match it only when supplied.
			if t.ID != task || (t.DispatchID != "" && t.DispatchID != dispatch) || (t.RunID != "" && t.RunID != run) {
				continue
			}
			completed = completed || t.Status == "completed"
			report, err = t.WorkerReport()
			if err != nil {
				return nil, errors.New("invalid worker report")
			}
			if report != nil {
				if report.Outcome != "succeeded" && report.Outcome != "failed" {
					return nil, errors.New("invalid worker outcome")
				}
				break
			}
		}
		if report != nil {
			return report, nil
		}
		workers, err := e.Orca.Workers(ctx, orca.ListOptions{Run: run, IncludeRemote: true})
		if err != nil {
			return nil, err
		}
		var worker *orca.Worker
		for i := range workers {
			if workers[i].DispatchID == dispatch {
				worker = &workers[i]
				break
			}
		}
		if worker != nil && watch.observe != nil {
			if err := watch.observe(*worker); err != nil {
				return nil, err
			}
		}
		unverifiable := worker != nil && worker.Projection.Liveness.Verdict == "unverifiable"
		if worker != nil && worker.Projection.Liveness.Verdict == "exited" {
			return livenessFailure("worker exited without worker_done"), nil
		}
		if worker != nil && !unverifiable && worker.Projection.Stage.Detail == "turn_start_unobserved" {
			now := e.Now()
			if watch.unobservedSince.IsZero() {
				watch.unobservedSince = now
			}
			if now.Sub(watch.unobservedSince) >= workerTurnStartTimeout {
				if !watch.stopAttempted {
					watch.stopAttempted = true
					_, watch.stopErr = e.Orca.WorkerStop(ctx, watch.stopRequest, dispatch)
				}
				if watch.stopErr != nil {
					return nil, watch.stopErr
				}
				return livenessFailure("worker never started its turn"), nil
			}
		} else {
			watch.unobservedSince = time.Time{}
		}
		if !completed {
			if err := observeIdle(); err != nil {
				return nil, err
			}
		}
		sleep := e.Sleep
		if sleep == nil {
			sleep = pause
		}
		if err := sleep(ctx, interval); err != nil {
			return nil, err
		}
	}
}

func livenessFailure(reason string) *orca.WorkerReport {
	return &orca.WorkerReport{Provenance: "executor_liveness", Outcome: "failed", Body: reason}
}

func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// runRequest scopes Run mutations to the executor terminal: Orca rejects a reused
// request ID whose input differs (request_mismatch), and the caller is part of
// that input, so each executor terminal needs its own request while a retry in
// the same terminal stays an idempotent replay.
func runRequest(t Ticket, c Config, step string) string { return scopedRequest(t, c, step, 1) }

// scopedRequest applies the same terminal scope to every Orca mutation.
func scopedRequest(t Ticket, c Config, step string, n int) string {
	return orca.RequestID(t.Identifier, c.Unit, step+"-"+c.Terminal, n)
}
