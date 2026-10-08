package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// cleanupMerged is best effort after confirmed Done. Frozen handoff identity
// survives a missing local checkpoint; cleanup never changes ticket ownership.
func (r *Runtime) cleanupMerged(ctx context.Context, t Ticket) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	logger := logx.New("scheduler", os.Stderr)
	warn := func(step string, err error) {
		logger.Warn("postmerge cleanup failed", "ticket", t.Identifier, "step", step, "err", err)
	}
	h := t.Handoff
	receipt, err := r.Orca.WorkerList(ctx, orca.ListOptions{Run: h.Run, Limit: 100, IncludeRemote: true})
	if err != nil {
		warn("worker-list", err)
	} else {
		var workers []struct {
			Dispatch      string `json:"dispatchId"`
			Status        string `json:"dispatchStatus"`
			TerminalState string `json:"terminalState"`
		}
		if err := json.Unmarshal(receipt["workers"], &workers); err != nil {
			warn("worker-list", err)
		} else {
			for _, w := range workers {
				if w.Dispatch == "" || w.TerminalState == "released" || (w.Status != "completed" && w.Status != "failed") {
					continue
				}
				request := orca.RequestID(t.Identifier, "merge-lane", "release-"+w.Dispatch, 1)
				if _, err := r.Orca.WorkerRelease(ctx, request, w.Dispatch); err != nil {
					warn("worker-release", err)
				}
			}
		}
	}
	boardID := t.Identifier
	if _, ok := r.Clients[t.Repo].(interface{ Repository() string }); ok {
		boardID = ""
	}
	if _, err := r.Orca.WorktreeSet(ctx, h.Worktree, boardID, "completed", fmt.Sprintf("driver: 已合并 #%d", h.PR)); err != nil {
		warn("worktree-set", err)
	}
	branch := "refs/heads/agent/" + t.Identifier + "/" + h.Unit
	raw, err := r.command(ctx, t.Repo, "git", "ls-remote", "--heads", "origin", branch)
	if err != nil {
		warn("branch-lookup", err)
		return
	}
	if strings.TrimSpace(string(raw)) == "" {
		return
	}
	if _, err := r.command(ctx, t.Repo, "git", "push", "origin", "--delete", branch); err != nil {
		warn("branch-delete", err)
	}
}
