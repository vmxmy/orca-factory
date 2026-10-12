package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
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
	r.board(ctx, t, machine.PhaseMerged, fmt.Sprintf("PR #%d", h.PR))
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

// board mirrors a merge-lane phase onto the ticket's frozen worktree card. The
// board is an observation, so a failed write only warns.
func (r *Runtime) board(ctx context.Context, t Ticket, phase machine.Phase, detail string) {
	if t.Handoff == nil || t.Handoff.Worktree == "" {
		return
	}
	card := orca.TicketCard(t.Handoff.Worktree, t.Identifier, t.Title, t.Number, phase, detail)
	card.PR = t.Handoff.PR
	r.setCard(ctx, t, card)
}

func (r *Runtime) setCard(ctx context.Context, t Ticket, card orca.BoardCard) {
	if _, err := r.Orca.WorktreeSet(ctx, card); err != nil {
		logx.New("scheduler", os.Stderr).Warn("Orca board update failed", "ticket", t.Identifier, "column", card.Status, "err", err)
	}
}
