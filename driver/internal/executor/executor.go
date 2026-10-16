// Package executor drives one ticket through the state machine to merge ownership.
package executor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

type Orca interface {
	RunCreate(context.Context, string, string) (orca.Receipt, error)
	RunUse(context.Context, string, string) (orca.Receipt, error)
	WorkerStart(context.Context, string, orca.StartOptions) (orca.Receipt, error)
	TaskList(context.Context, string) ([]orca.Task, error)
	Workers(context.Context, orca.ListOptions) ([]orca.Worker, error)
	TerminalList(context.Context) ([]orca.Terminal, error)
	TerminalSend(context.Context, string, string) (orca.Receipt, error)
	WorkerRelease(context.Context, string, string) (orca.Receipt, error)
	WorktreeSet(context.Context, orca.BoardCard) (orca.Receipt, error)
}
type Issues interface {
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
	// Number is the GitHub issue number; 0 for other backends.
	Number  int
	NewTest bool
}
type Config struct {
	// Merging requires durable correction evidence; it never authorizes a fresh run.
	Merging                                                             bool
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
	Orca     Orca
	Issues   Issues
	GitHub   GitHub
	Tools    Tools
	Config   Config
	Now      func() time.Time
	Recovery Recovery
	Sleep    func(context.Context, time.Duration) error
	// Stderr receives best-effort structured logs; nil means os.Stderr.
	Stderr     io.Writer
	logContext *slog.Logger
}
type Result struct {
	State             machine.State
	RunID, DispatchID string
	PR                github.PullRequest
}

// board mirrors a phase onto the ticket worktree's Orca card. The board is an
// observation, so a failed write only warns.
func (e *Executor) board(ctx context.Context, t Ticket, worktree string, phase machine.Phase, detail string, pr int) {
	if worktree == "" {
		return
	}
	card := orca.TicketCard(worktree, t.Identifier, t.Title, t.Number, phase, detail)
	card.PR = pr
	if _, err := e.Orca.WorktreeSet(ctx, card); err != nil {
		e.warn("%s Orca board status update failed: %v", t.Identifier, err)
	}
}

func (e *Executor) warn(format string, args ...any) {
	e.logger().Warn(fmt.Sprintf(format, args...))
}

// Execute stops at merge ownership; it never merges a PR. Errors are deliberately
// generic because external error messages and receipts may contain credentials.
func (e *Executor) Execute(ctx context.Context, t Ticket) (Result, error) {
	return e.withLog(ctx).execute(ctx, t, nil, 0)
}

func (e *Executor) execute(ctx context.Context, t Ticket, restored *checkpoint, infraUsed int) (result Result, err error) {
	if err := e.freshExecution(ctx, t); err != nil {
		return result, err
	}
	result.State = machine.Initial()
	result.State.Infra = infraUsed
	defer func() {
		if errors.Is(err, errBudgetExhausted) {
			err = nil
		}
		e.logError(result.State.Phase, err)
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
	if e.Orca == nil || e.Issues == nil || e.GitHub == nil || e.Tools == nil {
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
	var baseline, diffBase, taskID, spec, brief, head, failureOutput, failureSummary, reviewerTier string
	var mergeGateOutput, mergeReviewOutput string
	cp := checkpoint{}
	if restored != nil {
		cp = *restored
	}
	// Transition authorization always precedes the I/O belonging to the next phase.
	transition := func(ev machine.Event, action func([]machine.Effect) error) error {
		previous := result.State
		next, fx, stepErr := e.step(previous, ev)
		if stepErr != nil {
			return stepErr
		}
		charged := next.Infra > previous.Infra
		if action != nil {
			if actionErr := action(fx); actionErr != nil {
				return actionErr
			}
		}
		// Reads inside the authorized action may consume this same ticket budget.
		next.Infra = result.State.Infra + (next.Infra - previous.Infra)
		result.State = next
		status.Push(e.Now(), func(view *status.View) {
			status.SetTicket(view, status.Ticket{Repo: c.Repo, Identifier: t.Identifier, Owner: string(next.Owner), Phase: string(next.Phase)}, next.Owner == machine.OwnerExecutor || next.Owner == machine.OwnerMerge)
		})
		if next.Phase == machine.PhaseResolver && (next.Infra >= machine.DefaultLimits.Infra || next.Corrections > machine.DefaultLimits.Corrections) {
			evidence := string(previous.Phase)
			if previous.Phase == machine.PhaseReview {
				evidence += "；审查命令档位: " + reviewerTier
			}
			return e.handoff(ctx, t, next, evidence, result.RunID, &cp)
		}
		if charged {
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
		if next.Phase == machine.PhaseResolver {
			body = "Agent处置: " + next.HumanReason + "\n证据: " + string(previous.Phase) + "\n处置要求: 检查 worker 分支并重新排队\n" + body
		}
		if ev == machine.EvOpenPR {
			body = "PR 已开: " + result.PR.HTMLURL + "\n" + body
		}
		if previous.Owner != next.Owner {
			body += fmt.Sprintf("\n归属: %s term=%s run=%s attempt=%d since=%s", next.Owner, c.Terminal, result.RunID, next.Attempt, e.Now().UTC().Format(time.RFC3339))
		}
		if ev == machine.EvOpenPR {
			evidence, err := e.freezeMergeHandoff(ctx, c, t, result, cp, head, diffBase, spec, mergeGateOutput, mergeReviewOutput)
			if err != nil {
				return err
			}
			body += "\n" + evidence
		}
		if next.Phase == machine.PhaseCorrection {
			summary, err := e.failSummary(ctx, c, cmp.Or(diffBase, baseline), branch, head, failureOutput, ev)
			if err != nil {
				return err
			}
			failureSummary = summary
			if summary != "" {
				if err := appendFailLog(c, t, summary); err != nil {
					return err
				}
				body += "\n" + summary
			}
		}
		if commentErr := e.comment(ctx, t.ID, body); commentErr != nil {
			return errors.New("ticket transition comment failed")
		}
		e.board(ctx, t, cp.Worktree, next.Phase, "", result.PR.Number)
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
				return fmt.Errorf("Orca run takeover failed; reconcile request before retry: %w", safeDependencyError(callErr))
			}
			result.RunID = cp.Run
		} else {
			receipt, callErr = e.Orca.RunCreate(ctx, runRequest(t, c, "run"), t.Identifier+" "+t.Title)
			if callErr != nil {
				return fmt.Errorf("Orca run creation failed; reconcile request before retry: %w", safeDependencyError(callErr))
			}
			result.RunID = receiptID(receipt, "run")
			if result.RunID == "" {
				return errors.New("Orca run receipt missing ID")
			}
		}
		callErr = e.retryRead(ctx, t, &result, "baseline lookup", func() error {
			var readErr error
			baseline, readErr = e.Tools.Baseline(ctx, c.Repo)
			if readErr != nil {
				return readErr
			}
			if baseline == "" {
				return errors.New("baseline lookup failed")
			}
			return nil
		})
		if callErr != nil {
			return callErr
		}
		if restored != nil && cp.Corrections > 0 && baseline != cp.Baseline {
			// A correction may span main advancing: the new worker must branch from
			// current main, or the gate runs against a stale base and fails driver-wired.
			cp.Baseline = baseline
		}
		issue, callErr := e.Issues.UpdateIssue(ctx, t.ID, "In Progress", []string{"auto-impl"}, nil)
		if callErr != nil {
			return errors.New("ticket claim failed")
		}
		claimed := false
		for _, label := range issue.Labels.Nodes {
			if label.Name == "auto-impl" {
				claimed = true
			}
		}
		if !claimed {
			return errors.New("ticket claim was not confirmed")
		}
		rendered, gate, writeErr := writeArtifacts(c, t, branch)
		if writeErr != nil {
			return writeErr
		}
		spec = gate
		if cp.MergeSpec != "" {
			if err := os.WriteFile(spec, []byte(cp.MergeSpec), 0600); err != nil {
				return errors.New("cannot restore frozen gate spec")
			}
		}
		brief = rendered
		if cp.Baseline != "" {
			baseline = cp.Baseline
		}
		cp.Run = result.RunID
		cp.Baseline = baseline
		if restored != nil {
			if cp.PendingParent != "" {
				_, agent, model, effort, err := correctionTier(c, cp.Corrections)
				if err != nil {
					return err
				}
				if err := e.startDispatch(ctx, c, t, &cp, orca.StartOptions{Run: result.RunID, Task: cp.Task, Worktree: "path:" + cp.Worktree, Agent: agent, Model: model, Effort: effort}); err != nil {
					return err
				}
			}
			taskID = cp.Task
			result.DispatchID = cp.Dispatch
			return nil
		}
		workerSpec, err := workerTaskSpec(c.Repo, brief)
		if err != nil {
			return err
		}
		if err := e.startDispatch(ctx, c, t, &cp, orca.StartOptions{Run: result.RunID, Spec: workerSpec, Worktree: "new-top-level", Repo: "path:" + c.Repo, BaseBranch: baseline, Name: t.Identifier + "-" + c.Unit, Agent: c.Agent, Model: c.Model, Effort: c.Effort}); err != nil {
			return err
		}
		taskID, result.DispatchID = cp.Task, cp.Dispatch
		return nil
	})
	if err != nil {
		return result, err
	}
	result.State.Corrections = cp.Corrections
	result.State.Attempt = max(result.State.Attempt, cp.Attempt)
	if cp.MergeCause != "" {
		result.State.Phase = machine.PhaseCorrection
		again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, cp.MergeCause, failureSummary, transition)
		if correctionErr != nil || !again {
			return result, correctionErr
		}
		taskID = cp.Task
	}
	for {
		head, failureOutput = "", ""
		report := &orca.WorkerReport{Outcome: "succeeded"}
		var waitErr error
		var idle workerIdle
		watch := workerWatch{observe: func(w orca.Worker) error {
			if cp.Worktree != "" || w.WorktreePath() == "" {
				return nil
			}
			next := cp
			next.Worktree = w.WorktreePath()
			if err := saveCheckpoint(c, t, next); err != nil {
				return err
			}
			cp = next
			// Real worker-start receipts omit the worktree, so the claim
			// transition could not place the card yet.
			e.board(ctx, t, cp.Worktree, result.State.Phase, "", 0)
			return nil
		}}
		observeIdle := func() error {
			if !idle.observe(ctx, e, cp.Worktree, c.WorkerIdleTimeout, result.State) {
				return nil
			}
			if err := transition(machine.EvWorkerStall, nil); err != nil {
				return err
			}
			if result.State.Owner == machine.OwnerResolver {
				return errBudgetExhausted
			}
			return nil
		}
		if !cp.Completed {
			err = e.retryRead(ctx, t, &result, "task polling", func() error {
				report, waitErr = e.wait(ctx, result.RunID, taskID, result.DispatchID, c.PollInterval, &watch, observeIdle)
				// The stall transition already accounts for exhaustion and handoff.
				if result.State.Owner == machine.OwnerResolver {
					return nil
				}
				return waitErr
			})
			if err != nil {
				return result, err
			}
			if result.State.Owner == machine.OwnerResolver {
				return result, waitErr
			}
			releaseAllowed = report.Provenance != provenanceExecutorLiveness
		}
		if cp.Completed {
			releaseAllowed = cp.Settled
		}
		cp.Completed = false
		if waitErr != nil {
			return result, waitErr
		}
		failureCause := "worker failed"
		if report.Provenance == provenanceExecutorLiveness {
			failureCause = report.Body
			failureOutput = report.Body
		}
		event := machine.EvWorkerOK
		if report.Outcome != "succeeded" {
			event = machine.EvWorkerFail
		}
		if err = transition(event, nil); err != nil {
			return result, err
		}
		// A settled report authorizes cleanup, including before a correction handoff.
		if err = release(); err != nil {
			return result, err
		}
		if result.State.Phase == machine.PhaseCorrection {
			again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, failureCause, failureSummary, transition)
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
		if result.State.Owner == machine.OwnerResolver {
			return result, nil
		}
		err = e.retryRead(ctx, t, &result, "branch lookup", func() error { var readErr error; head, readErr = e.Tools.Head(ctx, c.Repo, branch); return readErr })
		if err != nil {
			return result, err
		}
		event = machine.EvBranchOK
		if head == "" || head == baseline {
			event = machine.EvBranchNo
		} else if cp.MergeHead != "" && head == cp.MergeHead {
			return result, errors.New("merge correction requires a new head")
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
		if err = transition(event, nil); err != nil || result.State.Owner == machine.OwnerResolver {
			return result, err
		}
		var output, verdict string
		var gateErr, parseErr error
		err = e.retryRead(ctx, t, &result, "gate output", func() error {
			done := e.external("gate")
			output, gateErr = e.Tools.Gate(ctx, c.Repo, head, spec)
			verdict, parseErr = gateVerdict(output)
			done(errors.Join(gateErr, parseErr), "verdict", verdict)
			stop := dependencyStop(ctx, gateErr)
			if stop != nil {
				if gateErr != nil {
					return gateErr
				}
				return stop
			}
			var ticketErr *infra.Ticket
			var transientErr *infra.Transient
			if errors.As(gateErr, &ticketErr) || errors.As(gateErr, &transientErr) {
				return gateErr
			}
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
			mergeGateOutput = output
		}
		if err = transition(event, nil); err != nil {
			return result, err
		}
		if result.State.Phase == machine.PhaseCorrection {
			again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, "gate red", failureSummary, transition)
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
		if result.State.Owner == machine.OwnerResolver {
			return result, nil
		}
		for result.State.Phase == machine.PhaseReview {
			var reviewErr error
			done := e.external("reviewer")
			output, reviewerTier, reviewErr = e.Tools.Review(WithReviewBrief(ctx, ReviewBrief{Text: brief, Lane3Globs: strings.Join(c.Lane3Globs, " ")}), c.Repo, diffBase, head, t.Estimate)
			verdict, parseErr = reviewVerdict(output)
			event = machine.EvReviewEmpty
			if reviewErr == nil && parseErr == nil {
				if verdict == "green" {
					event = machine.EvReviewGreen
					mergeReviewOutput = output
				} else {
					event = machine.EvReviewRed
				}
			}
			done(errors.Join(reviewErr, parseErr), "verdict", verdict)
			stop := dependencyStop(ctx, reviewErr)
			if stop != nil {
				return result, stop
			}
			failureOutput = output
			if err = transition(event, nil); err != nil {
				return result, err
			}
		}
		if result.State.Owner == machine.OwnerResolver {
			return result, nil
		}
		if result.State.Phase == machine.PhaseCorrection {
			again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, "reviewer red", failureSummary, transition)
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
	err = transition(machine.EvOpenPR, func(fx []machine.Effect) (err error) {
		done := e.external("open-pr")
		defer func() { done(err) }()
		if !hasEffect(fx, "open-pr-rest-idempotent-by-branch") {
			return errors.New("missing PR effect")
		}
		var pr *github.PullRequest
		callErr := e.retryRead(ctx, t, &result, "PR lookup", func() error {
			var readErr error
			pr, readErr = e.GitHub.FindOpenPR(ctx, c.Owner, c.Repository, branch)
			return readErr
		})
		if callErr != nil {
			return callErr
		}
		if pr != nil {
			result.PR = *pr
		} else {
			body, err := e.pullRequestBody(ctx, t)
			if err != nil {
				return err
			}
			result.PR, callErr = e.GitHub.CreatePR(ctx, c.Owner, c.Repository, github.CreatePRRequest{Head: branch, Base: "main", Title: t.Identifier + " " + t.Title, Body: body})
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

const provenanceExecutorLiveness = "executor_liveness"

type workerWatch struct {
	observe func(orca.Worker) error
}

func (e *Executor) wait(ctx context.Context, run, task, dispatch string, interval time.Duration, watch *workerWatch, observeIdle func() error) (*orca.WorkerReport, error) {
	readReport := func() (*orca.WorkerReport, bool, error) {
		tasks, err := e.taskList(ctx, run)
		if err != nil {
			return nil, false, safeDependencyError(err)
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
				return nil, false, errors.New("invalid worker report")
			}
			if report != nil {
				if report.Outcome != "succeeded" && report.Outcome != "failed" {
					return nil, false, errors.New("invalid worker outcome")
				}
				break
			}
		}
		return report, completed, nil
	}
	for {
		report, completed, err := readReport()
		if err != nil {
			return nil, err
		}
		if report != nil {
			return report, nil
		}
		workers, err := e.Orca.Workers(ctx, orca.ListOptions{Run: run, IncludeRemote: true})
		if err != nil {
			return nil, safeDependencyError(err)
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
		if worker != nil && worker.Projection.Liveness.Verdict == "exited" {
			report, _, err := readReport()
			if err != nil {
				return nil, err
			}
			if report != nil {
				return report, nil
			}
			return livenessFailure("worker exited without worker_done"), nil
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
	return &orca.WorkerReport{Provenance: provenanceExecutorLiveness, Outcome: "failed", Body: reason}
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
