package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// The ticket state directory is the same durable location as brief.md/gate.spec.
// Persist association before dispatch: an unknown mutation is never a new worker.
type checkpoint struct {
	MergeQueue, MergeHead, MergeCause, MergeSpec string `json:",omitempty"`
	CatchupRounds, Attempt, Infra                int    `json:",omitempty"`
	Settled                                      bool
	Run, Task, Dispatch, Worktree, Baseline      string
	Corrections                                  int
	Pending                                      bool
	Completed                                    bool
	PendingParent                                string `json:",omitempty"`
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
	if !component.MatchString(t.Identifier) || !component.MatchString(c.Unit) || !component.MatchString(c.Repository) || e.Tools == nil || e.GitHub == nil || e.Orca == nil || e.Linear == nil {
		return result, errors.New("invalid recovery configuration")
	}
	if c.Root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return result, errors.New("cannot resolve state root")
		}
		c.Root = filepath.Join(home, ".local", "state", "orca-factory")
	}
	branch := "agent/" + t.Identifier + "/" + c.Unit
	var head string
	err = e.retryRead(ctx, t, &result, "resume branch lookup", func() error {
		var readErr error
		head, readErr = e.Tools.Head(ctx, c.Repo, branch)
		return readErr
	})
	if err != nil {
		return result, err
	}
	var pr *github.PullRequest
	err = e.retryRead(ctx, t, &result, "resume PR lookup", func() error {
		var readErr error
		pr, readErr = e.GitHub.FindOpenPR(ctx, c.Owner, c.Repository, branch)
		return readErr
	})
	if err != nil {
		return result, err
	}
	cp, err := readCheckpoint(c, t)
	if err != nil {
		return result, err
	}
	if cp != nil {
		result.State.Corrections = cp.Corrections
	}
	mergeRepair, repairErr := e.restoreMergeCorrection(ctx, c, t, &cp, &result)
	if repairErr != nil {
		return result, repairErr
	}
	// The compatibility flag requests recovery, never a new claim. Existing
	// PRs still reconcile tasks before proving a completed frozen handoff.
	if c.Merging && !mergeRepair && pr == nil {
		return result, errors.New("merge correction evidence required")
	}
	// Reconcile checkpointed tasks before ordinary PR handoff.
	if pr != nil && !mergeRepair && cp == nil {
		if pr.Number <= 0 || pr.HTMLURL == "" {
			return result, errors.New("invalid existing PR")
		}
		return e.resumeMergeHandoff(ctx, c, t, *pr, head, nil, result)
	}
	if cp == nil {
		// Stable run-create is a request reconciliation, not a second Run.
		receipt, callErr := e.Orca.RunCreate(ctx, runRequest(t, c, "run"), t.Identifier+" "+t.Title)
		if callErr != nil {
			return result, safeDependencyError(callErr)
		}
		run := receiptID(receipt, "run")
		if run == "" {
			return result, errors.New("resume run receipt missing ID")
		}
		var tasks []orca.Task
		callErr = e.retryRead(ctx, t, &result, "resume old task lookup", func() error {
			var readErr error
			tasks, readErr = e.Orca.TaskList(ctx, run)
			return readErr
		})
		if callErr != nil {
			return result, callErr
		}
		cp, err = recoverDispatch(tasks, &checkpoint{Run: run}, branch)
		if err != nil {
			return result, err
		}
	}
	if mergeRepair && cp.MergeCause != "" && !cp.Pending {
		return e.execute(ctx, t, cp, result.State.Infra)
	}
	if cp != nil && cp.Pending {
		cp.Completed, cp.Settled = false, false
	}
	r := e.recovery()
	if head != "" && r != nil && (cp == nil || (!cp.Pending && cp.Task == "")) {
		var complete bool
		err = e.retryRead(ctx, t, &result, "resume commit lookup", func() error {
			var readErr error
			complete, readErr = r.Complete(ctx, c.Repo, head)
			return readErr
		})
		if err != nil {
			return result, err
		}
		if complete {
			if cp == nil {
				cp = &checkpoint{}
			}
			cp.Completed = true
		}
	}
	if cp != nil {
		if cp.Run != "" && (cp.Task != "" || cp.Pending) {
			var tasks []orca.Task
			err = e.retryRead(ctx, t, &result, "resume task lookup", func() error {
				var readErr error
				tasks, readErr = e.Orca.TaskList(ctx, cp.Run)
				return readErr
			})
			if err != nil {
				return result, err
			}
			cp, err = recoverDispatch(tasks, cp, branch)
			if err != nil {
				return result, err
			}
		}
		if cp.Pending {
			return result, errors.New("unresolved worker start; reconcile request before retry")
		}
		if !cp.Completed && cp.Task == "" {
			return result, errors.New("incomplete worker recovery state")
		}
		if pr != nil && !mergeRepair && cp.Completed && cp.Settled {
			// Preserve a validated frozen handoff only after current task
			// reconciliation. A crash before its first write requires fresh
			// gate/reviewer evidence through execute, not an endless wait.
			resumed, resumeErr := e.resumeMergeHandoff(ctx, c, t, *pr, head, cp, result)
			if !errors.Is(resumeErr, errMissingMergeHandoff) {
				return resumed, resumeErr
			}
		}
		if c.Merging && !mergeRepair {
			return result, errors.New("merge correction evidence required")
		}
		if err := saveCheckpoint(c, t, *cp); err != nil {
			return result, err
		}
		return e.execute(ctx, t, cp, result.State.Infra)
	}
	return e.execute(ctx, t, nil, result.State.Infra)
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
