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

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
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
	WorkerStop(context.Context, string, string) (orca.Receipt, error)
	WorktreeSet(context.Context, orca.BoardCard) (orca.Receipt, error)
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
	Estimate                                                        *float64
	ID, Identifier, Title, Todo, Check, Scope, TestFiles, Assertion string
	Lane                                                            int
	// Number is the GitHub issue number; 0 for other backends.
	Number  int
	NewTest bool
}
type Config struct {
	// Merging requires durable correction evidence; it never authorizes a fresh run.
	Merging                                                             bool
	Lane3Globs                                                          []string
	Machine, Mode                                                       string
	TierSettings                                                        map[string]string
	WorkerSkills                                                        []string
	TierChain                                                           []string
	Escalate                                                            string
	Repo, Owner, Repository, Terminal, Unit, Root, Agent, Model, Effort string
	PollInterval                                                        time.Duration
	WorkerIdleTimeout                                                   time.Duration
	WorkerMaxDuration                                                   time.Duration
	Tier                                                                string
}
type Executor struct {
	Orca     Orca
	Issues   issues.Store
	GitHub   GitHub
	Tools    Tools
	Config   Config
	Now      func() time.Time
	Recovery Recovery
	Sleep    func(context.Context, time.Duration) error
	// Heartbeat is the process-level liveness evidence the scheduler reads; nil writes none.
	Heartbeat *orca.Heartbeat
	// HeartbeatPath, when set, starts Heartbeat once Start accepts the reservation.
	HeartbeatPath string
	// ProbeModel confirms a model error read from a worker's screen with one
	// request; nil means modelhealth.Probe in the repository checkout.
	ProbeModel func(context.Context, modelhealth.Ref) modelhealth.Status
	// Stderr receives best-effort structured logs; nil means os.Stderr.
	Stderr     io.Writer
	logContext *slog.Logger
}
type Result struct {
	State             machine.State
	RunID, DispatchID string
	PR                github.PullRequest
	// StallReason is the coordinator's latest STALLED reason, cleaned to one line.
	StallReason string
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
	defer func() {
		if errors.Is(err, maintenance.ErrRefused) {
			err = errors.Join(err, e.comment(ctx, t.ID, maintenance.RefusedLine+"派工未发往 Orca，维护恢复后重放 term="+e.Config.Terminal))
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
	if c.WorkerMaxDuration <= 0 {
		c.WorkerMaxDuration = 120 * time.Minute
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if t.NewTest && strings.TrimSpace(t.Assertion) == "" {
		return result, errors.New("new-test evidence requires CHECK_ASSERTION in CHECK_TEST_FILES; add a version 1 JSON assertion manifest and request gate-spec-refresh before handoff; frozen work requires a fresh reviewed handoff")
	}
	if !component.MatchString(t.Identifier) || !component.MatchString(c.Unit) || !component.MatchString(c.Repository) || c.Repo == "" || c.Terminal == "" || t.ID == "" || t.Todo == "" || t.Check == "" || t.Scope == "" || strings.ContainsAny(t.Check+t.Scope+t.TestFiles+t.Assertion, "\r\n") || (t.NewTest && t.TestFiles == "") || t.Lane < 1 || t.Lane > 2 {
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
	var mergeGateOutput, mergeReviewOutput, overrunEvidence string
	var scopeBlocked []string
	var failedSubject string
	var reviewFail reviewFailure
	cp := checkpoint{}
	if restored != nil {
		cp = *restored
	}
	if restored == nil {
		fallback, fallbackErr := config.ParseFallback(c.TierSettings[config.FallbackKey("AGENT_TIER_"+strings.ToUpper(cmp.Or(c.Tier, "default")))])
		registry, registryErr := modelhealth.Open(c.Root)
		if fallbackErr == nil && registryErr == nil {
			candidates := append([]modelhealth.Ref{{Agent: c.Agent, Model: c.Model, Effort: c.Effort}}, fallback...)
			if reason := modelParkReason(registry, candidates, e.Now()); reason != "" {
				return result, e.parkModel(ctx, t, &result, cp, reason)
			}
		} else {
			e.logger().Warn("first dispatch model check unavailable", "error", logErrorSummary(errors.Join(fallbackErr, registryErr)))
		}
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
		if next.Phase == machine.PhaseResolver && (next.Infra >= machine.DefaultLimits.Infra || next.Corrections > machine.DefaultLimits.Corrections || ev == machine.EvWorkerScopeBlocked || ev == machine.EvCorrectionStalled || ev == machine.EvWorkerOverrun) {
			evidence := string(previous.Phase)
			if previous.Phase == machine.PhaseReview {
				evidence += "；审查命令档位: " + reviewerTier
				if reviewFail.kind != "" {
					evidence += "；reviewer 失败类别: " + reviewFail.kind
				}
			}
			if ev == machine.EvWorkerScopeBlocked {
				evidence += "；SCOPE 外文件: " + strings.Join(scopeBlocked, " ")
			}
			if ev == machine.EvCorrectionStalled {
				evidence += "；停滞判断: " + result.StallReason
			}
			if ev == machine.EvWorkerOverrun {
				evidence += "；" + overrunEvidence
			}
			e.logger().Info("resolver handoff", withWorkerSubject(failedSubject, "reason", next.HumanReason, "evidence", evidence)...)
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
			e.Heartbeat.SetRun(result.RunID)
		} else {
			receipt, callErr = e.Orca.RunCreate(ctx, runRequest(t, c, "run"), t.Identifier+" "+t.Title)
			if callErr != nil {
				return fmt.Errorf("Orca run creation failed; reconcile request before retry: %w", safeDependencyError(callErr))
			}
			result.RunID = receiptID(receipt, "run")
			if result.RunID == "" {
				return errors.New("Orca run receipt missing ID")
			}
			e.Heartbeat.SetRun(result.RunID)
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
				tier, agent, model, effort, err := correctionTier(c, cp.Corrections)
				if err != nil {
					return err
				}
				selected := e.tierModel(tier, modelhealth.Ref{Agent: agent, Model: model, Effort: effort})
				if err := e.startDispatch(ctx, c, t, &cp, orca.StartOptions{Run: result.RunID, Task: cp.Task, Worktree: "path:" + cp.Worktree, Agent: selected.Agent, Model: selected.Model, Effort: selected.Effort}); err != nil {
					return err
				}
			}
			taskID = cp.Task
			result.DispatchID = cp.Dispatch
			return nil
		}
		workerSpec, err := workerTaskSpec(c.Repo, brief, c.WorkerSkills)
		if err != nil {
			return err
		}
		e.logger().Info("worker skills", "worker_skills", strings.Join(c.WorkerSkills, ","), "correction", cp.Corrections)
		first := e.tierModel(cmp.Or(c.Tier, "default"), modelhealth.Ref{Agent: c.Agent, Model: c.Model, Effort: c.Effort})
		if err := e.startDispatch(ctx, c, t, &cp, orca.StartOptions{Run: result.RunID, Spec: workerSpec, Worktree: "new-top-level", Repo: "path:" + c.Repo, BaseBranch: baseline, Name: t.Identifier + "-" + c.Unit, Agent: first.Agent, Model: first.Model, Effort: first.Effort}); err != nil {
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
		again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, cp.MergeCause, failureSummary, nil, transition)
		if correctionErr != nil || !again {
			return result, correctionErr
		}
		taskID = cp.Task
	}
	for {
		head, failureOutput, failedSubject = "", "", ""
		report := &orca.WorkerReport{Outcome: "succeeded"}
		var waitErr error
		var idle workerIdle
		watch := workerWatch{unobservedSince: cp.UnobservedSince, stop: func(dispatch string) error {
			_, err := e.Orca.WorkerStop(ctx, scopedRequest(t, c, "stop-"+dispatch, 1), dispatch)
			return err
		}, persist: func(since time.Time) error {
			next := cp
			next.UnobservedSince = since
			if err := saveCheckpoint(c, t, next); err != nil {
				return err
			}
			cp = next
			return nil
		}, observe: func(w orca.Worker) error {
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
		watch.ended = func() *orca.WorkerReport { return e.modelReport(ctx, c, cp, result.DispatchID) }
		var modelChecked int64
		var modelStopped *orca.WorkerReport
		observeIdle := func() error {
			if idle.observed && idle.lastOutput != modelChecked && e.Now().Sub(time.UnixMilli(idle.lastOutput)) >= resolver.ModelCheckDelay {
				modelChecked = idle.lastOutput
				if modelStopped = e.stopModelUnavailable(ctx, c, cp, result.RunID, result.DispatchID, &watch); modelStopped != nil {
					return errModelStopped
				}
			}
			ev := machine.EvWorkerStall
			if ran := e.Now().Sub(cp.DispatchStartedAt); ran >= c.WorkerMaxDuration {
				ev = machine.EvWorkerOverrun
				overrunEvidence = fmt.Sprintf("运行超时: 已运行 %d 分钟；终端最后输出 %s", int(ran/time.Minute), idle.lastOutputText())
			} else if !idle.observe(ctx, e, cp.Worktree, c.WorkerIdleTimeout, result.State) {
				return nil
			}
			if err := transition(ev, nil); err != nil {
				return err
			}
			if result.State.Owner == machine.OwnerResolver {
				return errBudgetExhausted
			}
			return nil
		}
		if !cp.Completed && cp.DispatchStartedAt.IsZero() {
			next := cp
			next.DispatchStartedAt = e.Now().UTC()
			if err := saveCheckpoint(c, t, next); err != nil {
				return result, err
			}
			cp = next
		}
		if !cp.Completed {
			err = e.retryRead(ctx, t, &result, "task polling", func() error {
				report, waitErr = e.wait(ctx, result.RunID, taskID, result.DispatchID, c.PollInterval, &watch, observeIdle)
				if errors.Is(waitErr, errModelStopped) {
					report, waitErr = modelStopped, nil
				}
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
			if report.Provenance == provenanceExecutorModelParked {
				return result, e.parkModel(ctx, t, &result, cp, report.Body)
			}
			releaseAllowed = report.Provenance != provenanceExecutorLiveness && report.Provenance != provenanceExecutorUnstarted && report.Provenance != provenanceExecutorModel
		}
		if cp.Completed {
			releaseAllowed = cp.Settled
		}
		cp.Completed = false
		if waitErr != nil {
			return result, waitErr
		}
		failureCause := "worker failed"
		if report.Provenance == provenanceExecutorLiveness || report.Provenance == provenanceExecutorUnstarted || report.Provenance == provenanceExecutorModel {
			failureCause = report.Body
			failureOutput = report.Body
		}
		event := machine.EvWorkerOK
		if report.Outcome != "succeeded" {
			event = machine.EvWorkerFail
			failedSubject = workerSubject(report)
			if scopeBlocked = scopeBlockedFiles(report.Body, spec); len(scopeBlocked) > 0 {
				event = machine.EvWorkerScopeBlocked
				if cp.MergeSpec == "" {
					var extended bool
					var extendErr error
					if c.Mode == "auto" {
						extended, extendErr = intake.AutoApproveWorkerScope(ctx, e.Issues, t.ID, scopeBlocked, strings.Join(c.Lane3Globs, " "), e.Now())
					} else if os.Getenv("AGENT_WORKER_AUTO_SCOPE") == "1" {
						extended, extendErr = intake.ExtendWorkerScope(ctx, e.Issues, t.ID, t.Scope, scopeBlocked, strings.Join(c.Lane3Globs, " "))
					}
					if extendErr != nil {
						return result, extendErr
					}
					if extended {
						t.Scope += " " + strings.Join(scopeBlocked, " ")
						brief, spec, err = writeArtifacts(c, t, branch)
						if err != nil {
							return result, err
						}
						event = machine.EvWorkerScopeExtended
					}
				}
			}
		}
		if report.Provenance == provenanceExecutorUnstarted || report.Provenance == provenanceExecutorModel {
			event = machine.EvWorkerUnstarted
			if report.Provenance == provenanceExecutorModel {
				event = machine.EvWorkerModelUnavailable
			}
			next := cp
			next.Restarts++
			if err = saveCheckpoint(c, t, next); err != nil {
				return result, err
			}
			cp = next
		}
		if err = transition(event, nil); err != nil {
			return result, err
		}
		// A settled report authorizes cleanup, including before a correction handoff.
		if err = release(); err != nil {
			return result, err
		}
		if result.State.Phase == machine.PhaseCorrection {
			corrector := e
			if report.Provenance == provenanceExecutorModel {
				corrector = e.modelRedo(report.Body)
			}
			again, correctionErr := corrector.correct(ctx, c, t, &result, &cp, brief, failureCause, failureSummary, report, transition)
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
		if cp.MergeSpec == "" {
			if err = e.honorGateSpecRefresh(ctx, t, &result); err != nil {
				return result, err
			}
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
			again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, "gate red", failureSummary, nil, transition)
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
			output, reviewerTier, reviewErr = e.Tools.Review(WithReviewBrief(ctx, ReviewBrief{Text: brief, Lane3Globs: strings.Join(c.Lane3Globs, " "), Prior: priorReview(cp, result.State.Attempt)}), c.Repo, diffBase, head, t.Estimate)
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
			if event == machine.EvReviewEmpty {
				reviewFail = classifyReviewFailure(reviewErr, output)
				if err := e.comment(ctx, t.ID, "reviewer 失败类别: "+reviewFail.kind+"\n"+reviewFail.detail); err != nil {
					return result, errors.New("review failure evidence comment failed")
				}
			}
			if event == machine.EvReviewRed {
				next := cp
				next.PriorReview, next.PriorReviewAttempt = reviewFindings(output), result.State.Attempt
				if err = saveCheckpoint(c, t, next); err != nil {
					return result, err
				}
				cp = next
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
			again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, "reviewer red", failureSummary, nil, transition)
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
	if err = e.reportFrozenGateSpecRefresh(ctx, t, &result); err != nil {
		return result, err
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
			body, err := e.pullRequestBody(ctx, t, reviewFollowUps(mergeReviewOutput))
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

const (
	provenanceExecutorLiveness  = "executor_liveness"
	provenanceExecutorUnstarted = "executor_unstarted"
	provenanceExecutorModel     = "executor_model_unavailable"
	// provenanceExecutorModelParked stops a worker whose tier has no available
	// candidate left; the ticket is parked for the model.
	provenanceExecutorModelParked = "executor_model_parked"
)

type workerWatch struct {
	observe func(orca.Worker) error
	stop    func(dispatch string) error
	// ended reports a worker that exited or lost its dispatch without
	// worker_done after a model error on its screen; nil leaves it a liveness
	// failure.
	ended           func() *orca.WorkerReport
	persist         func(unobservedSince time.Time) error
	unobservedSince time.Time
}

func (w *workerWatch) markUnobserved(since time.Time) error {
	if w.persist != nil {
		if err := w.persist(since); err != nil {
			return err
		}
	}
	w.unobservedSince = since
	return nil
}

func (e *Executor) wait(ctx context.Context, run, task, dispatch string, interval time.Duration, watch *workerWatch, observeIdle func() error) (*orca.WorkerReport, error) {
	// readReport also returns the task when it awaits a dispatch, a candidate
	// for loss that only the worker read can confirm.
	readReport := func() (*orca.WorkerReport, bool, *orca.Task, error) {
		tasks, err := e.taskList(ctx, run)
		if err != nil {
			return nil, false, nil, safeDependencyError(err)
		}
		completed := false
		var awaiting *orca.Task
		var report *orca.WorkerReport
		for _, t := range tasks {
			// task-list rows carry no dispatch_id; match it only when supplied.
			if t.ID != task || (t.DispatchID != "" && t.DispatchID != dispatch) || (t.RunID != "" && t.RunID != run) {
				continue
			}
			completed = completed || t.Status == "completed"
			report, err = t.WorkerReport()
			if err != nil {
				return nil, false, nil, errors.New("invalid worker report")
			}
			if report != nil {
				if report.Outcome != "succeeded" && report.Outcome != "failed" {
					return nil, false, nil, errors.New("invalid worker outcome")
				}
				break
			}
			if resolver.AwaitsDispatch(t) {
				awaiting = &t
			}
		}
		return report, completed, awaiting, nil
	}
	for {
		report, completed, awaiting, err := readReport()
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
		if awaiting != nil {
			// The dispatch may report between the two reads; only a task read
			// after the worker read can confirm loss.
			if report, completed, awaiting, err = readReport(); err != nil {
				return nil, err
			}
			if report != nil {
				return report, nil
			}
		}
		if worker != nil && watch.observe != nil {
			if err := watch.observe(*worker); err != nil {
				return nil, err
			}
		}
		// An abandoned worker (orca-worker-list-abandoned.json) is fenced and
		// reports no liveness verdict; like an exited one it never reports.
		if worker != nil && (worker.Projection.Liveness.Verdict == "exited" || worker.Projection.Stage.Worker == "abandoned") {
			report, _, _, err := readReport()
			if err != nil {
				return nil, err
			}
			if report != nil {
				return report, nil
			}
			if report, err := endedReport(watch, readReport); err != nil || report != nil {
				return report, err
			}
			if worker.Projection.Stage.Worker == "abandoned" {
				return livenessFailure("worker abandoned without worker_done"), nil
			}
			return livenessFailure("worker exited without worker_done"), nil
		}
		if worker != nil && resolver.StartUnobserved(worker.DispatchStatus, worker.Projection.Stage.Detail) {
			if stopped, err := e.stopUnstarted(ctx, run, dispatch, watch); err != nil || stopped != nil {
				return stopped, err
			}
		} else if !watch.unobservedSince.IsZero() {
			if err := watch.markUnobserved(time.Time{}); err != nil {
				return nil, err
			}
		}
		if awaiting != nil {
			listed, status := worker != nil, ""
			if listed {
				status = worker.DispatchStatus
			}
			if reason, lost := resolver.LostDispatch(*awaiting, dispatch, listed, status); lost {
				if report, err := endedReport(watch, readReport); err != nil || report != nil {
					return report, err
				}
				return livenessFailure(reason), nil
			}
		}
		if !completed {
			if err := observeIdle(); err != nil {
				if !errors.Is(err, errModelStopped) {
					return nil, err
				}
				// worker_done may land between the last task read and the stop.
				if report, _, _, readErr := readReport(); readErr != nil || report != nil {
					return report, readErr
				}
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

// endedReport is the model report for a worker that ended without
// worker_done, unless worker_done arrived while its model was probed.
func endedReport(watch *workerWatch, readReport func() (*orca.WorkerReport, bool, *orca.Task, error)) (*orca.WorkerReport, error) {
	if watch.ended == nil {
		return nil, nil
	}
	model := watch.ended()
	// worker_done may land while the screen is read and the model probed.
	report, _, _, err := readReport()
	if err != nil || report != nil {
		return report, err
	}
	return model, nil
}

// stopUnstarted applies the resolver's window to a dispatch whose turn never
// started. An unconfirmed stop keeps waiting; the next poll replays it.
func (e *Executor) stopUnstarted(ctx context.Context, run, dispatch string, watch *workerWatch) (*orca.WorkerReport, error) {
	now := e.Now()
	if watch.unobservedSince.IsZero() {
		if err := watch.markUnobserved(now); err != nil {
			return nil, err
		}
	}
	if now.Sub(watch.unobservedSince) < resolver.StartUnobservedWindow {
		return nil, nil
	}
	if watch.stop(dispatch) != nil {
		e.logger().Warn("unstarted worker stop not acknowledged", "dispatch", dispatch)
	}
	workers, err := e.Orca.Workers(ctx, orca.ListOptions{Run: run, IncludeRemote: true})
	if err != nil {
		return nil, safeDependencyError(err)
	}
	for _, w := range workers {
		if w.DispatchID == dispatch && resolver.DispatchEnded(w.DispatchStatus) {
			return &orca.WorkerReport{Provenance: provenanceExecutorUnstarted, Outcome: "failed", Body: fmt.Sprintf("worker turn did not start within %s; dispatch %s stopped", resolver.StartUnobservedWindow, dispatch)}, nil
		}
	}
	return nil, nil
}

// stopModelUnavailable stops a quiet worker whose screen shows a model error
// and returns modelReport's report once the stop is confirmed; otherwise nil,
// and the stall handling goes on.
func (e *Executor) stopModelUnavailable(ctx context.Context, c Config, cp checkpoint, run, dispatch string, watch *workerWatch) *orca.WorkerReport {
	report := e.modelReport(ctx, c, cp, dispatch)
	if report == nil {
		return nil
	}
	if watch.stop(dispatch) != nil {
		e.logger().Warn("model-unavailable worker stop not acknowledged", "dispatch", dispatch)
	}
	workers, err := e.Orca.Workers(ctx, orca.ListOptions{Run: run, IncludeRemote: true})
	if err != nil {
		return nil
	}
	for _, w := range workers {
		if w.DispatchID == dispatch && resolver.DispatchEnded(w.DispatchStatus) {
			return report
		}
	}
	return nil
}

// modelReport reads the worker's screen. When it shows the launched model's
// limit or an unsupported model, it records the model and returns the report
// that redoes the brief on the tier's next available candidate, or parks the
// ticket when none remains; otherwise nil.
func (e *Executor) modelReport(ctx context.Context, c Config, cp checkpoint, dispatch string) *orca.WorkerReport {
	used, err := modelhealth.Parse(cp.Launch)
	if err != nil {
		return nil
	}
	reader, ok := e.Orca.(resolver.ScreenReader)
	if !ok {
		return nil
	}
	now := e.Now()
	status, err := resolver.ScreenModel(ctx, reader, dispatch, used, now)
	if err != nil {
		e.logger().Warn("worker screen unreadable for model check", "dispatch", dispatch, "error", logErrorSummary(err))
		return nil
	}
	if status.State != modelhealth.Unavailable {
		return nil
	}
	// The worker may only quote such an error; a probe of the model decides.
	probe := e.ProbeModel
	if probe == nil {
		probe = func(ctx context.Context, ref modelhealth.Ref) modelhealth.Status {
			return modelhealth.Probe(ctx, ref, c.Repo, modelhealth.Endpoint{})
		}
	}
	confirmed := probe(ctx, used)
	if confirmed.State != modelhealth.Unavailable {
		e.logger().Info("worker screen model error not confirmed by probe", "dispatch", dispatch, "reason", confirmed.Reason)
		return nil
	}
	status = resolver.ConfirmedStatus(status, confirmed)
	tier, agent, model, effort, err := correctionTier(c, cp.Corrections)
	if err != nil {
		return nil
	}
	fallback, err := config.ParseFallback(e.Config.TierSettings[config.FallbackKey("AGENT_TIER_"+strings.ToUpper(tier))])
	if err != nil {
		return nil
	}
	registry, err := modelhealth.Open(e.Config.Root)
	if err == nil {
		err = registry.Record(status)
	}
	if err != nil {
		e.logger().Warn("worker model error not recorded", "dispatch", dispatch, "error", logErrorSummary(err))
		return nil
	}
	candidates := append([]modelhealth.Ref{{Agent: agent, Model: model, Effort: effort}}, fallback...)
	if next, ok := registry.Select(candidates, now); ok {
		return &orca.WorkerReport{Provenance: provenanceExecutorModel, Outcome: "failed", Body: fmt.Sprintf("model unavailable: %s; dispatch %s ended; next candidate %s", used, dispatch, next)}
	}
	return &orca.WorkerReport{Provenance: provenanceExecutorModelParked, Outcome: "failed", Body: modelParkReason(registry, candidates, now)}
}

func modelParkReason(registry *modelhealth.Registry, candidates []modelhealth.Ref, now time.Time) string {
	statuses := make([]modelhealth.Status, 0, len(candidates))
	for _, ref := range candidates {
		status := registry.Status(ref, now)
		if status.State != modelhealth.Unavailable {
			return ""
		}
		statuses = append(statuses, status)
	}
	return config.ModelParkReason(statuses)
}

// modelRedo is e whose correction step writes a fixed redo note instead of
// running a correction session: the model, not the work, failed, and the
// session's own model may share the exhausted account.
func (e *Executor) modelRedo(reason string) *Executor {
	inner := e.recovery()
	if inner == nil {
		return e
	}
	redo := *e
	redo.Recovery = modelRedoRecovery{Recovery: inner, note: "上一派工因模型不可用被停止，不是实现失败：" + reason + "\n在当前分支已有工作的基础上继续完成原简报。\n"}
	return &redo
}

type modelRedoRecovery struct {
	Recovery
	note string
}

func (r modelRedoRecovery) Correction(_ context.Context, req CorrectionRequest) error {
	return os.WriteFile(req.Path, []byte(r.note), 0o600)
}

// parkModel records the model reason before releasing the ticket's ownership,
// so ResumeModelParks can read the candidates once one recovers.
func (e *Executor) parkModel(ctx context.Context, t Ticket, result *Result, cp checkpoint, reason string) error {
	status.Push(e.Now(), func(view *status.View) {
		status.SetTicket(view, status.Ticket{Repo: e.Config.Repo, Identifier: t.Identifier}, false)
	})
	// The reason lands before the labels and keeps the owner, so a stop between
	// the writes leaves either an unparked ticket still owned by the executor or
	// a parked one whose model reason ResumeModelParks can read; ownership is
	// released only after the labels, as the scheduler parks.
	if err := e.comment(ctx, t.ID, "搁置："+reason); err != nil {
		return errors.New("model park reason failed")
	}
	if _, err := e.Issues.UpdateIssue(ctx, t.ID, "Todo", []string{"parked"}, []string{"ready-for-agent", "auto-impl", "auto-failed"}); err != nil {
		return errors.New("model park labels failed")
	}
	result.State.Phase, result.State.Owner, result.State.HumanReason, result.State.ExecAlive = machine.PhaseUnclaimed, machine.OwnerNone, reason, false
	completed := fmt.Sprintf("worker 派工 %s 已停止，分支保留", result.DispatchID)
	if result.DispatchID == "" {
		completed = "首轮派工前当前档候选均在冷却，未派工"
	}
	body := fmt.Sprintf("搁置：%s\n已完成：%s\n阶段: %s\n归属: %s", reason, completed, machine.PhaseUnclaimed, machine.OwnerNone)
	if err := e.comment(ctx, t.ID, body); err != nil {
		return errors.New("model park comment failed")
	}
	e.board(ctx, t, cp.Worktree, machine.PhaseUnclaimed, "搁置："+reason, 0)
	return nil
}

// errModelStopped ends a wait whose worker stopModelUnavailable stopped.
var errModelStopped = errors.New("worker stopped for an unavailable model")

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
