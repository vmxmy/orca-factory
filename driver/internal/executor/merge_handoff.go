package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

// freezeMergeHandoff adds no new local recovery store. The caller writes this
// line atomically with merge ownership through the existing Linear comment path.
func (e *Executor) freezeMergeHandoff(ctx context.Context, c Config, t Ticket, result Result, cp checkpoint, head, base, spec, gate, review string) (string, error) {
	raw, err := os.ReadFile(spec)
	if err != nil {
		return "", errors.New("merge gate spec unavailable")
	}
	lane := 0
	for _, line := range strings.Split(gate, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "LANE" {
			lane, _ = strconv.Atoi(fields[1])
		}
	}
	h := merge.Handoff{Version: 1, TicketID: t.ID, Identifier: t.Identifier, Repository: c.Owner + "/" + c.Repository, Unit: c.Unit, PR: result.PR.Number, Head: head, Baseline: base, GateSpec: string(raw), GateOutput: gate, ReviewOutput: review, Lane: lane, Run: result.RunID, Task: cp.Task, Dispatch: result.DispatchID, Worktree: cp.Worktree, Attempt: result.State.Attempt, Corrections: result.State.Corrections, Infra: result.State.Infra, CatchupRounds: cp.CatchupRounds, EnqueuedAt: e.Now().UTC()}
	e.freezeLedger(t, result, cp.Worktree)
	key := sha256.Sum256([]byte(strings.Join([]string{h.Repository, h.TicketID, h.Unit, h.Head, h.Run, h.Task, h.Dispatch}, "\x00")))
	h.QueueID = hex.EncodeToString(key[:])
	rows, err := e.Linear.ListComments(ctx, t.ID)
	if err != nil {
		return "", safeDependencyError(err)
	}
	for _, row := range rows {
		for _, line := range strings.Split(row.Body, "\n") {
			if !strings.HasPrefix(line, merge.HandoffPrefix) {
				continue
			}
			var old merge.Handoff
			if json.Unmarshal([]byte(strings.TrimPrefix(line, merge.HandoffPrefix)), &old) != nil {
				return "", errors.New("invalid existing merge handoff")
			}
			if old.QueueID == h.QueueID {
				h.EnqueuedAt = old.EnqueuedAt
				a, _ := json.Marshal(h)
				b, _ := json.Marshal(old)
				if string(a) != string(b) {
					return "", errors.New("conflicting frozen merge handoff")
				}
				return merge.HandoffPrefix + string(b), nil
			}
		}
	}
	b, err := json.Marshal(h)
	if err != nil {
		return "", errors.New("cannot encode merge handoff")
	}
	return merge.HandoffPrefix + string(b), nil
}

var errMissingMergeHandoff = errors.New("existing PR has no frozen handoff")

// resumeMergeHandoff preserves the original FIFO identity and budgets. An open
// PR alone cannot prove gate/reviewer success, or authorize a fresh handoff.
func (e *Executor) resumeMergeHandoff(ctx context.Context, c Config, t Ticket, pr github.PullRequest, head string, current *checkpoint, prior Result) (Result, error) {
	result := prior
	rows, err := e.Linear.ListComments(ctx, t.ID)
	if err != nil {
		return result, safeDependencyError(err)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	var h *merge.Handoff
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
			if !strings.HasPrefix(line, merge.HandoffPrefix) {
				continue
			}
			raw := strings.TrimPrefix(line, merge.HandoffPrefix)
			var value merge.Handoff
			if json.Unmarshal([]byte(raw), &value) != nil {
				return result, errors.New("invalid existing PR handoff")
			}
			if old, ok := seen[value.QueueID]; ok && old != raw {
				return result, errors.New("conflicting existing PR handoff")
			}
			seen[value.QueueID] = raw
			h = &value
		}
	}
	if h == nil {
		return result, errMissingMergeHandoff
	}
	if current != nil && (h.Run != current.Run || h.Task != current.Task || h.Dispatch != current.Dispatch || h.Corrections != current.Corrections) {
		return result, errors.New("frozen handoff does not match current dispatch")
	}
	if owner != string(machine.OwnerMerge) || h.Version != 1 || h.TicketID != t.ID || h.Identifier != t.Identifier || h.Repository != c.Owner+"/"+c.Repository || h.Unit != c.Unit || h.PR != pr.Number || h.Head != head || pr.Head.SHA != head || head == "" || h.Baseline == "" || h.Run == "" || h.Task == "" || h.Worktree == "" || h.QueueID == "" || h.EnqueuedAt.IsZero() || h.Attempt < 1 || h.Corrections < 0 || h.Infra < 0 {
		return result, errors.New("existing PR lacks matching frozen validation; retain ownership for reconciliation")
	}
	if err := h.Validated(); err != nil {
		return result, err
	}
	state := result.State
	state.Owner = machine.OwnerMerge
	state.Phase = machine.PhaseMergeQueue
	state.Attempt = max(state.Attempt, h.Attempt)
	state.Corrections = max(state.Corrections, h.Corrections)
	// Start already restored current.Infra; both receipts describe overlapping
	// history. Add only the missing historical portion, preserving this Start's
	// read failures already included in state.Infra.
	restoredInfra := 0
	if current != nil {
		restoredInfra = current.Infra
	}
	state.Infra += max(0, h.Infra-restoredInfra)
	return Result{State: state, PR: pr, RunID: h.Run, DispatchID: h.Dispatch}, nil
}
