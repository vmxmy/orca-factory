package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

// Persist the decision atomically with ownership before starting a terminal.
// An uncertain launch is recovered by the ordinary scheduler inspection path.
func (r *Runtime) mergeDecision(ctx context.Context, t Ticket, main string, result merge.Result) error {
	h := t.Handoff
	p := merge.Progress{Version: 1, QueueID: h.QueueID, TicketID: t.ID, Repository: h.Repository, PR: h.PR, Head: h.Head, Main: main, Phase: "waiting", Corrections: h.Corrections + result.Corrections, CatchupRounds: result.Rounds, Infra: h.Infra, Attempt: max(1, h.Attempt), Reason: result.Reason}
	if t.Progress != nil && t.Progress.QueueID == h.QueueID {
		retainMergeProgress(&p, t.Progress)
	}
	human := result.Status == merge.Human || p.Corrections > machine.DefaultLimits.Corrections
	// A lane 2 review keeps the waiting phase so the resolver can approve the
	// same frozen handoff instead of revalidating a new queue identity.
	review := result.Status == merge.Review && !human
	if result.Status == merge.Correction {
		p.Phase = "correction"
	}
	if human {
		p.Phase = "human"
	}
	raw, _ := json.Marshal(p)
	body := merge.ProgressPrefix + string(raw)
	if human || review {
		if _, err := r.Clients[t.Repo].UpdateIssue(ctx, t.ID, "In Progress", []string{"auto-impl", "auto-failed"}, []string{"ready-for-agent"}); err != nil {
			return errors.New("merge escalation unconfirmed")
		}
		event, evidence, demand := p.Reason, "merge validation", "检查合并结果并重新排队"
		if review {
			event, evidence, demand = "lane 2 复核", "merge policy", "独立复核 PR 当前 head，通过后提交原 handoff 的 merge 提案"
		}
		body = "Agent处置: " + event + "\n证据: " + evidence + "\n处置要求: " + demand + "\n阶段: " + string(machine.PhaseResolver) + "\n归属: " + string(machine.OwnerResolver) + "\n" + body
	} else if result.Status == merge.Correction {
		body = fmt.Sprintf("阶段: %s\n归属: %s term=- run=%s attempt=%d\n%s", machine.PhaseCorrection, machine.OwnerExecutor, h.Run, p.Attempt, body)
	}
	if err := r.mergeComment(ctx, t, body); err != nil {
		return err
	}
	switch {
	case human || review:
		r.board(ctx, t, machine.PhaseResolver, p.Reason)
	case result.Status == merge.Correction:
		r.board(ctx, t, machine.PhaseCorrection, p.Reason)
	default:
		r.board(ctx, t, machine.PhaseMergeQueue, p.Reason)
	}
	if human || review {
		return nil
	}
	if result.Status == merge.Correction {
		t.Owner = machine.OwnerExecutor
		t.Phase = machine.PhaseCorrection
		t.Run = h.Run
		t.Attempt = p.Attempt
		t.Progress = &p
		return r.Launch(ctx, t)
	}
	return errors.New(result.Reason)
}

func mergeRounds(t Ticket) (int, error) {
	h, p := t.Handoff, t.Progress
	if h.CatchupRounds < 0 || h.Corrections < 0 || h.Infra < 0 {
		return 0, errors.New("invalid merge budget")
	}
	if p == nil {
		return h.CatchupRounds, nil
	}
	switch p.Phase {
	case "waiting", "correction", "human", "merge-intent", "merged":
	default:
		return 0, errors.New("unknown merge progress phase")
	}
	if p.QueueID != h.QueueID {
		return h.CatchupRounds, nil
	}
	if p.Version != 1 || p.TicketID != h.TicketID || p.Repository != h.Repository || p.PR != h.PR || p.Head != h.Head || p.CatchupRounds < 0 {
		return 0, errors.New("conflicting merge decision")
	}
	if p.Phase == "correction" || p.Phase == "human" {
		return 0, errors.New("merge decision already transferred ownership")
	}
	return max(h.CatchupRounds, p.CatchupRounds), nil
}

// Progress extensions share one receipt; carry both budgets and deployment when
// writing another phase of the same queue identity.
func retainMergeProgress(next, prior *merge.Progress) {
	if prior == nil || prior.QueueID != next.QueueID {
		return
	}
	next.Deployment = prior.Deployment
	next.Corrections = max(next.Corrections, prior.Corrections)
	next.CatchupRounds = max(next.CatchupRounds, prior.CatchupRounds)
	next.Infra = max(next.Infra, prior.Infra)
	next.Attempt = max(next.Attempt, prior.Attempt)
	if next.Reason == "" {
		next.Reason = prior.Reason
	}
}
