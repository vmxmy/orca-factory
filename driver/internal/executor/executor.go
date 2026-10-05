// Package executor drives one ticket through the state machine to merge ownership.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	WorkerStart(context.Context, string, orca.StartOptions) (orca.Receipt, error)
	TaskList(context.Context, string) ([]orca.Task, error)
	WorkerRelease(context.Context, string, string) (orca.Receipt, error)
}
type Linear interface {
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
	Gate(context.Context, string, string, string) (string, error)
	Review(context.Context, string, string, string) (string, error)
}
type Ticket struct {
	ID, Identifier, Title, Todo, Check, Scope, TestFiles string
	Lane                                                 int
	NewTest                                              bool
}
type Config struct {
	TierSettings                                                        map[string]string
	Escalate                                                            string
	Repo, Owner, Repository, Terminal, Unit, Root, Agent, Model, Effort string
	PollInterval                                                        time.Duration
	Tier                                                                string
}
type Executor struct {
	Orca     Orca
	Linear   Linear
	GitHub   GitHub
	Tools    Tools
	Config   Config
	Now      func() time.Time
	Recovery Recovery
	Sleep    func(context.Context, time.Duration) error
}
type Result struct {
	State             machine.State
	RunID, DispatchID string
	PR                github.PullRequest
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
	branch := "agent/" + t.Identifier + "/" + c.Unit
	request := func(step string) string { return orca.RequestID(t.Identifier, c.Unit, step+"-"+result.DispatchID, 1) }
	releaseAttempted := false
	releaseAllowed := false
	release := func() error {
		if result.DispatchID == "" || releaseAttempted || !releaseAllowed {
			return nil
		}
		releaseAttempted = true
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, releaseErr := e.Orca.WorkerRelease(cleanup, request("release"), result.DispatchID)
		if releaseErr != nil {
			return errors.New("worker release failed; reconcile dispatch before retry")
		}
		return nil
	}
	defer func() { err = errors.Join(err, release()) }()
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
			return e.handoff(ctx, t, next, string(previous.Phase))
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
		if next.Phase == machine.PhaseHuman {
			body = "转人: " + next.HumanReason + "\n证据: " + string(previous.Phase) + "\n需要你: 检查 worker 分支并重新排队\n" + body
		}
		if ev == machine.EvOpenPR {
			body = "PR 已开: " + result.PR.HTMLURL + "\n" + body
		}
		if previous.Owner != next.Owner {
			body += fmt.Sprintf("\n归属: %s term=%s run=%s attempt=%d since=%s", next.Owner, c.Terminal, result.RunID, next.Attempt, e.Now().UTC().Format(time.RFC3339))
		}
		if _, commentErr := e.Linear.CreateComment(ctx, t.ID, body); commentErr != nil {
			return errors.New("Linear transition comment failed")
		}
		return nil
	}
	var baseline, taskID, spec, brief string
	cp := checkpoint{}
	if restored != nil {
		cp = *restored
	}

	err = transition(machine.EvClaim, func(fx []machine.Effect) error {
		if !hasEffect(fx, "worker-start-with-request-id") {
			return errors.New("claim lacks worker effect")
		}
		receipt, callErr := e.Orca.RunCreate(ctx, orca.RequestID(t.Identifier, c.Unit, "run", 1), t.Identifier+" "+t.Title)
		if callErr != nil {
			return errors.New("Orca run creation failed; reconcile request before retry")
		}
		result.RunID = receiptString(receipt, "runId")
		if result.RunID == "" {
			result.RunID = nestedString(receipt, "run", "id")
		}
		if result.RunID == "" {
			return errors.New("Orca run receipt missing ID")
		}
		if cp.Run != "" {
			result.RunID = cp.Run
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
		receipt, callErr = e.Orca.WorkerStart(ctx, orca.RequestID(t.Identifier, c.Unit, "worker", 1), orca.StartOptions{Run: result.RunID, Spec: brief, Worktree: "new-top-level", Repo: "path:" + c.Repo, BaseBranch: baseline, Name: t.Identifier + "-" + c.Unit, Agent: c.Agent, Model: c.Model, Effort: c.Effort})
		result.DispatchID = receiptString(receipt, "dispatchId")
		taskID = receiptString(receipt, "taskId")
		if callErr != nil {
			return errors.New("worker start failed; reconcile dispatch before retry")
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
		report := &orca.WorkerReport{Outcome: "succeeded"}
		var waitErr error
		if !cp.Completed {
			err = e.retryRead(ctx, t, &result, "task polling", func() error {
				report, waitErr = e.wait(ctx, result.RunID, taskID, result.DispatchID, c.PollInterval)
				return waitErr
			})
			if err != nil {
				return result, err
			}
			releaseAllowed = true
		}
		if cp.Completed {
			releaseAllowed = cp.Settled
		}
		cp.Completed = false
		if waitErr != nil {
			return result, waitErr
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
			again, correctionErr := e.correct(ctx, c, t, &result, &cp, brief, "worker failed", transition)
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
		var head string
		err = e.retryRead(ctx, t, &result, "branch lookup", func() error { var readErr error; head, readErr = e.Tools.Head(ctx, c.Repo, branch); return readErr })
		if err != nil {
			return result, err
		}
		event = machine.EvBranchOK
		if head == "" || head == baseline {
			event = machine.EvBranchNo
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
			output, reviewErr := e.Tools.Review(ctx, c.Repo, baseline, head)
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
func (e *Executor) wait(ctx context.Context, run, task, dispatch string, interval time.Duration) (*orca.WorkerReport, error) {
	for {
		tasks, err := e.Orca.TaskList(ctx, run)
		if err != nil {
			return nil, errors.New("task polling failed")
		}
		for _, t := range tasks {
			if t.ID != task || t.DispatchID != dispatch || (t.RunID != "" && t.RunID != run) {
				continue
			}
			report, err := t.WorkerReport()
			if err != nil {
				return nil, errors.New("invalid worker report")
			}
			if report != nil {
				if report.Outcome != "succeeded" && report.Outcome != "failed" {
					return nil, errors.New("invalid worker outcome")
				}
				return report, nil
			}
		}
		if err := pause(ctx, interval); err != nil {
			return nil, err
		}
	}
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
