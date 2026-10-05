package executor

import (
	"context"
	"encoding/json"
	"errors"
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
func (e *Executor) Start(ctx context.Context, t Ticket) (Result, error) {
	c := e.Config
	if c.Unit == "" {
		c.Unit = "executor"
	}
	if !component.MatchString(t.Identifier) || !component.MatchString(c.Unit) || !component.MatchString(c.Repository) || e.Tools == nil || e.GitHub == nil || e.Orca == nil || e.Linear == nil {
		return Result{}, errors.New("invalid recovery configuration")
	}
	if c.Root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Result{}, errors.New("cannot resolve state root")
		}
		c.Root = filepath.Join(home, ".local", "state", "orca-factory")
	}
	branch := "agent/" + t.Identifier + "/" + c.Unit
	head, err := e.Tools.Head(ctx, c.Repo, branch)
	if err != nil {
		return Result{}, errors.New("resume branch lookup failed")
	}
	pr, err := e.GitHub.FindOpenPR(ctx, c.Owner, c.Repository, branch)
	if err != nil {
		return Result{}, errors.New("resume PR lookup failed")
	}
	if pr != nil {
		if pr.Number <= 0 || pr.HTMLURL == "" {
			return Result{}, errors.New("invalid existing PR")
		}
		state := machine.Initial()
		for _, ev := range []machine.Event{machine.EvClaim, machine.EvWorkerOK, machine.EvBranchOK, machine.EvGateGreen, machine.EvReviewGreen, machine.EvOpenPR} {
			state, _, err = machine.Step(state, ev, machine.DefaultLimits)
			if err != nil {
				return Result{}, err
			}
		}
		result := Result{State: state, PR: *pr}
		if err = e.comment(ctx, t.ID, "PR 已开: "+pr.HTMLURL+"\n阶段: 排队合并\n归属: 合并通道"); err != nil {
			return result, errors.New("resume handoff comment failed")
		}
		return result, nil
	}
	cp, err := readCheckpoint(c, t)
	if err != nil {
		return Result{}, err
	}
	if cp == nil {
		// Stable run-create is a request reconciliation, not a second Run.
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
		tasks, callErr := e.Orca.TaskList(ctx, run)
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
		}
	}
	r := e.recovery()
	if head != "" && r != nil {
		complete, err := r.Complete(ctx, c.Repo, head)
		if err != nil {
			return Result{}, errors.New("resume commit lookup failed")
		}
		if complete {
			if cp == nil {
				cp = &checkpoint{}
			}
			cp.Completed = true
		}
	}
	if cp != nil && cp.Run != "" && cp.Task != "" {
		tasks, err := e.Orca.TaskList(ctx, cp.Run)
		if err != nil {
			return Result{}, errors.New("resume task lookup failed")
		}
		for _, task := range tasks {
			if task.ID != cp.Task || (task.RunID != "" && task.RunID != cp.Run) || (cp.Dispatch != "" && task.DispatchID != "" && task.DispatchID != cp.Dispatch) {
				continue
			}
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
				}
			}
		}
	}
	if cp != nil {
		if cp.Pending && !cp.Completed {
			return Result{}, errors.New("unresolved worker start; reconcile request before retry")
		}
		if !cp.Completed && (cp.Task == "" || cp.Dispatch == "") {
			return Result{}, errors.New("incomplete worker recovery state")
		}
		return e.execute(ctx, t, cp)
	}
	return e.Execute(ctx, t)
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
	path := ""
	for _, field := range strings.Split(string(b), "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			path = strings.TrimPrefix(field, "worktree ")
		}
		if field == "branch refs/heads/"+branch && filepath.IsAbs(path) {
			return path, nil
		}
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
