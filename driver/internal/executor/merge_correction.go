package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// Linear owns the correction request; executor.json only reconciles dispatches
// using the existing checkpoint contract. A stale open PR cannot consume it.
func (e *Executor) restoreMergeCorrection(ctx context.Context, c Config, t Ticket, cp **checkpoint, result *Result) (bool, error) {
	rows, err := linear.CriticalComments(ctx, e.Linear, t.ID)
	if err != nil {
		return false, safeDependencyError(err)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	var h *merge.Handoff
	var p *merge.Progress
	owner := ""
	seen := map[string]string{}
	for _, row := range rows {
		for _, line := range strings.Split(row.Body, "\n") {
			if strings.HasPrefix(line, "归属: ") {
				fields := strings.Fields(strings.TrimPrefix(line, "归属: "))
				if len(fields) > 0 {
					owner = fields[0]
				}
			}
			if strings.HasPrefix(line, merge.HandoffPrefix) {
				raw := strings.TrimPrefix(line, merge.HandoffPrefix)
				var v merge.Handoff
				if json.Unmarshal([]byte(raw), &v) != nil {
					return false, errors.New("invalid merge handoff")
				}
				if old, ok := seen[v.QueueID]; ok && old != raw {
					return false, errors.New("conflicting frozen merge handoff")
				}
				seen[v.QueueID] = raw
				h = &v
			}
			if strings.HasPrefix(line, merge.ProgressPrefix) {
				var v merge.Progress
				if json.Unmarshal([]byte(strings.TrimPrefix(line, merge.ProgressPrefix)), &v) != nil {
					return false, errors.New("invalid merge progress")
				}
				p = &v
			}
		}
	}
	if p == nil || p.Phase != "correction" || h == nil || p.QueueID != h.QueueID {
		return false, nil
	}
	if owner != string(machine.OwnerExecutor) || p.Version != 1 || h.Version != 1 || p.TicketID != t.ID || h.TicketID != t.ID || h.Identifier != t.Identifier || h.Repository != c.Owner+"/"+c.Repository || p.Repository != h.Repository || h.Unit != c.Unit || p.PR != h.PR || p.Head != h.Head || p.Main == "" || p.Reason == "" || p.Corrections <= h.Corrections || p.Corrections > machine.DefaultLimits.Corrections || p.CatchupRounds < h.CatchupRounds || p.CatchupRounds >= 2 || p.Infra < h.Infra || p.Attempt < h.Attempt || h.Run == "" || h.Task == "" || !filepath.IsAbs(h.Worktree) {
		return false, errors.New("invalid merge correction identity or budget")
	}
	if err := h.Validated(); err != nil {
		return false, err
	}
	reconstruct := *cp == nil || (*cp).MergeQueue != h.QueueID
	if reconstruct {
		*cp = &checkpoint{Run: h.Run, Task: h.Task, Dispatch: h.Dispatch, Worktree: h.Worktree, Baseline: h.Baseline, Corrections: p.Corrections, Infra: p.Infra, Attempt: p.Attempt, CatchupRounds: p.CatchupRounds, MergeQueue: h.QueueID, MergeHead: h.Head, MergeSpec: h.GateSpec, MergeCause: p.Reason + "; fixed head=" + p.Head + " main=" + p.Main}
	}
	result.State.Infra += max(p.Infra, (*cp).Infra)
	result.State.Corrections = (*cp).Corrections
	if result.State.Infra >= machine.DefaultLimits.Infra {
		result.State.Owner, result.State.Phase = machine.OwnerResolver, machine.PhaseResolver
		result.State.HumanReason = "infra budget exhausted: merge correction recovery"
		if err := e.handoff(ctx, t, result.State, "merge correction recovery", (*cp).Run, *cp); err != nil {
			return false, err
		}
		return false, errBudgetExhausted
	}
	if reconstruct {
		if err := e.recoverMergeChildren(ctx, t, *cp, result); err != nil {
			return false, err
		}
		result.State.Corrections = (*cp).Corrections
		if err := saveCheckpoint(c, t, **cp); err != nil {
			return false, err
		}
	}
	return true, nil
}

// A lost checkpoint is not proof that a new terminal may create another child.
// Follow external parent links to the current correction; missing or ambiguous
// evidence never authorizes replaying a mutation with a new request UUID.
func (e *Executor) recoverMergeChildren(ctx context.Context, t Ticket, cp *checkpoint, result *Result) error {
	var tasks []orca.Task
	if err := e.retryRead(ctx, t, result, "merge correction child lookup", func() error { var err error; tasks, err = e.taskList(ctx, cp.Run); return err }); err != nil {
		return err
	}
	seen := map[string]bool{}
	depth := 0
	for {
		if seen[cp.Task] {
			return errors.New("cyclic merge correction tasks")
		}
		seen[cp.Task] = true
		var parent, child *orca.Task
		for i := range tasks {
			task := &tasks[i]
			if task.RunID != "" && task.RunID != cp.Run {
				continue
			}
			if task.ID == cp.Task {
				if parent != nil {
					return errors.New("ambiguous merge correction parent")
				}
				parent = task
			}
			if task.ParentID == cp.Task {
				if child != nil {
					return errors.New("ambiguous merge correction children")
				}
				child = task
			}
		}
		if child == nil {
			if depth > 0 {
				return nil
			}
			if parent == nil {
				return errors.New("merge correction task evidence unavailable")
			}
			report, err := parent.WorkerReport()
			if err != nil || report == nil || report.Outcome != "succeeded" {
				return errors.New("original merge task is not confirmed complete")
			}
			return nil
		}
		number := cp.Corrections
		if depth > 0 {
			number++
		}
		if child.ID == "" || number > machine.DefaultLimits.Corrections || !strings.HasPrefix(child.Spec, fmt.Sprintf("correction %d/3\n", number)) {
			return errors.New("unconfirmed merge correction child identity")
		}
		cp.Task, cp.Dispatch, cp.Corrections = child.ID, child.DispatchID, number
		cp.MergeCause = ""
		cp.Completed, cp.Settled, cp.Pending = false, false, true
		depth++
	}
}
