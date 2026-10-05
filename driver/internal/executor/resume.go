package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// The ticket state directory is the same durable location as brief.md/gate.spec.
// Persist association before dispatch: an unknown mutation is never a new worker.
type checkpoint struct {
	Settled                                 bool
	Run, Task, Dispatch, Worktree, Baseline string
	Corrections                             int
	Pending                                 bool
	Completed                               bool
}

func checkpointPath(c Config, t Ticket) string {
	return filepath.Join(c.Root, c.Repository, t.Identifier, "executor.json")
}
func saveCheckpoint(c Config, t Ticket, cp checkpoint) error {
	path := checkpointPath(c, t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("cannot create recovery directory")
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return errors.New("cannot encode recovery state")
	}
	if err = os.WriteFile(path+".new", b, 0600); err != nil {
		return errors.New("cannot write recovery state")
	}
	if err = os.Rename(path+".new", path); err != nil {
		return errors.New("cannot commit recovery state")
	}
	return nil
}
func readCheckpoint(c Config, t Ticket) (*checkpoint, error) {
	b, err := os.ReadFile(checkpointPath(c, t))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot read recovery state")
	}
	var cp checkpoint
	if json.Unmarshal(b, &cp) != nil {
		return nil, errors.New("invalid recovery state")
	}
	return &cp, nil
}

// Start reconciles remote evidence before Execute. Execute remains the explicit
// new-claim entry point for callers that already performed reconciliation.
func (e *Executor) Start(ctx context.Context, t Ticket) (result Result, err error) {
	e = e.withLog(t)
	phase := machine.Phase("recovery")
	recoveryStep := "validate-configuration"
	basis := "checkpoint-association"
	delegated := false
	defer func() {
		if !delegated {
			e.logError(phase, err, "recovery_step", recoveryStep)
		}
	}()
	c := e.Config
	if c.Unit == "" {
		c.Unit = "executor"
	}
	if !component.MatchString(t.Identifier) || !component.MatchString(c.Unit) || !component.MatchString(c.Repository) || e.Tools == nil || e.GitHub == nil || e.Orca == nil || e.Linear == nil {
		return Result{}, errors.New("invalid recovery configuration")
	}
	phase = machine.PhaseUnclaimed
	recoveryStep = "state-root"
	if c.Root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Result{}, errors.New("cannot resolve state root")
		}
		c.Root = filepath.Join(home, ".local", "state", "orca-factory")
	}
	recoveryStep = "read-checkpoint"
	cp, checkpointErr := readCheckpoint(c, t)
	if cp != nil {
		if cp.Completed {
			phase = machine.PhaseBranchCheck
		} else if cp.Task != "" || cp.Pending {
			phase = machine.PhaseWorker
		}
	}
	e.recoverySnapshot(ctx, t, cp, checkpointErr)
	branch := "agent/" + t.Identifier + "/" + c.Unit
	recoveryStep = "branch-lookup"
	head, err := e.Tools.Head(ctx, c.Repo, branch)
	if err != nil {
		return Result{}, fmt.Errorf("resume branch lookup failed: %w", err)
	}
	recoveryStep = "pr-lookup"
	pr, err := e.GitHub.FindOpenPR(ctx, c.Owner, c.Repository, branch)
	if err != nil {
		return Result{}, errors.New("resume PR lookup failed")
	}
	if pr != nil {
		e.recoveryPath(cp, "existing-pr", "open-pr-by-branch")
		recoveryStep = "validate-existing-pr"
		if pr.Number <= 0 || pr.HTMLURL == "" {
			return Result{}, errors.New("invalid existing PR")
		}
		recoveryStep = "reconstruct-existing-pr"
		state := machine.Initial()
		for _, ev := range []machine.Event{machine.EvClaim, machine.EvWorkerOK, machine.EvBranchOK, machine.EvGateGreen, machine.EvReviewGreen, machine.EvOpenPR} {
			state, _, err = e.stepDecision(state, ev, "recovery-reconstruction", phase)
			if err != nil {
				return Result{}, err
			}
		}
		result = Result{State: state, PR: *pr}
		phase = state.Phase
		recoveryStep = "existing-pr-handoff"
		if err = e.comment(ctx, t.ID, "PR 已开: "+pr.HTMLURL+"\n阶段: 排队合并\n归属: 合并通道"); err != nil {
			return result, errors.New("resume handoff comment failed")
		}
		return result, nil
	}
	recoveryStep = "validate-checkpoint"
	if checkpointErr != nil {
		return Result{}, checkpointErr
	}
	if cp == nil {
		// Stable run-create is a request reconciliation, not a second Run.
		recoveryStep = "reconcile-run"
		receipt, callErr := e.Orca.RunCreate(ctx, runRequest(t, c, "run"), t.Identifier+" "+t.Title)
		if callErr != nil {
			return Result{}, errors.New("resume run reconciliation failed")
		}
		run := receiptString(receipt, "runId")
		if run == "" {
			run = nestedString(receipt, "run", "id")
		}
		if run == "" {
			return Result{}, errors.New("resume run receipt missing ID")
		}
		e.association(run, "", "")
		recoveryStep = "old-task-lookup"
		tasks, callErr := e.taskList(ctx, run)
		if callErr != nil {
			return Result{}, errors.New("resume old task lookup failed")
		}
		for _, task := range tasks {
			if (task.RunID != "" && task.RunID != run) || !strings.Contains(task.Spec, branch) {
				continue
			}
			if cp != nil {
				return Result{}, errors.New("ambiguous old tasks; reconcile active dispatch")
			}
			cp = &checkpoint{Run: run, Task: task.ID, Dispatch: task.DispatchID}
			e.association(cp.Run, cp.Task, cp.Dispatch)
			phase = machine.PhaseWorker
		}
	}
	r := e.recovery()
	if head != "" && r != nil {
		recoveryStep = "commit-lookup"
		complete, err := r.Complete(ctx, c.Repo, head)
		if err != nil {
			return Result{}, errors.New("resume commit lookup failed")
		}
		if complete {
			if cp == nil {
				cp = &checkpoint{}
			}
			cp.Completed = true
			basis = "complete-commit"
			phase = machine.PhaseBranchCheck
		}
	}
	if cp != nil && cp.Run != "" && cp.Task != "" {
		e.association(cp.Run, cp.Task, cp.Dispatch)
		recoveryStep = "task-lookup"
		tasks, err := e.taskList(ctx, cp.Run)
		if err != nil {
			return Result{}, errors.New("resume task lookup failed")
		}
		for _, task := range tasks {
			if task.ID != cp.Task || (task.RunID != "" && task.RunID != cp.Run) || (cp.Dispatch != "" && task.DispatchID != "" && task.DispatchID != cp.Dispatch) {
				continue
			}
			recoveryStep = "task-result"
			report, err := task.WorkerReport()
			if err != nil {
				return Result{}, errors.New("resume task result invalid")
			}
			if report != nil {
				if task.DispatchID != "" {
					cp.Dispatch = task.DispatchID
				}
				cp.Pending = false
				cp.Settled = true
				if report.Outcome == "succeeded" {
					cp.Completed = true
					basis = "worker-report-succeeded"
					phase = machine.PhaseBranchCheck
				}
			}
		}
	}
	if cp != nil {
		recoveryStep = "reconcile-pending-worker"
		if cp.Pending && !cp.Completed && cp.Task == "" {
			if c.Merging {
				// A merge-conflict resume has no worker to reconcile; start fresh.
				return e.Execute(ctx, t)
			}
			// The previous executor's request ID belongs to its terminal and cannot be
			// replayed here; reconcile by the run's tasks instead.
			task, found, err := e.pendingWorker(ctx, cp.Run, branch)
			if err != nil {
				return Result{}, err
			}
			if !found {
				e.recoveryPath(cp, "new-worker", "no-recoverable-worker")
				delegated = true
				return e.execute(ctx, t, nil)
			}
			cp.Task, cp.Dispatch, cp.Pending = task.ID, task.DispatchID, false
			basis = "run-task-match"
		}
		if cp.Pending && !cp.Completed {
			e.recoveryPath(cp, "unresolved-worker", "pending-start-unconfirmed")
			return Result{}, errors.New("unresolved worker start; reconcile request before retry")
		}
		recoveryStep = "validate-worker-association"
		// task-list carries no dispatch_id, so an adopted task may lack one.
		if !cp.Completed && cp.Task == "" {
			return Result{}, errors.New("incomplete worker recovery state")
		}
		path := "active-worker"
		if cp.Completed {
			path = "completed-worker"
		}
		e.recoveryPath(cp, path, basis)
		delegated = true
		return e.execute(ctx, t, cp)
	}
	e.recoveryPath(cp, "new-worker", "no-recoverable-worker")
	delegated = true
	return e.execute(ctx, t, nil)
}
func (c *Commands) Complete(ctx context.Context, repo, head string) (bool, error) {
	b, err := c.run(ctx, repo, "git", "show", "-s", "--format=%(trailers:key=agent-status,valueonly)", head)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) == "complete", nil
}
func (c *Commands) Worktree(ctx context.Context, repo, branch string) (string, error) {
	b, err := c.run(ctx, repo, "git", "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", err
	}
	// Orca names the worker worktree <ticket>-<unit> and checks out a local branch
	// of its own; agent/<ticket>/<unit> is only the push target, so match either.
	name := strings.ReplaceAll(strings.TrimPrefix(branch, "agent/"), "/", "-")
	path, byName := "", ""
	for _, field := range strings.Split(string(b), "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			path = strings.TrimPrefix(field, "worktree ")
			if filepath.IsAbs(path) && filepath.Base(path) == name {
				byName = path
			}
		}
		if field == "branch refs/heads/"+branch && filepath.IsAbs(path) {
			return path, nil
		}
	}
	if byName != "" {
		return byName, nil
	}
	return "", errors.New("original worktree not found")
}
func worktreeReceipt(r orca.Receipt) string {
	for _, path := range []string{receiptString(r, "worktreePath"), nestedString(r, "worktree", "path"), nestedString(r, "placement", "worktreePath")} {
		if filepath.IsAbs(path) {
			return path
		}
	}
	return ""
}

// pendingWorker finds the task an unconfirmed worker-start may have created.
func (e *Executor) pendingWorker(ctx context.Context, run, branch string) (orca.Task, bool, error) {
	if run == "" {
		return orca.Task{}, false, nil
	}
	tasks, err := e.taskList(ctx, run)
	if err != nil {
		return orca.Task{}, false, fmt.Errorf("pending worker lookup failed: %w", err)
	}
	var found []orca.Task
	for _, task := range tasks {
		if (task.RunID == "" || task.RunID == run) && strings.Contains(task.Spec, branch) {
			found = append(found, task)
		}
	}
	switch len(found) {
	case 0:
		return orca.Task{}, false, nil
	case 1:
		return found[0], true, nil
	}
	return orca.Task{}, false, errors.New("ambiguous pending worker tasks; reconcile active dispatch")
}
