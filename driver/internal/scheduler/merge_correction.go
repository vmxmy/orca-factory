package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/mergechannel"
)

// Persist the decision and ownership in one comment before labels can block retries.
// An uncertain launch is recovered by the ordinary scheduler inspection path.
func (r *Runtime) mergeDecision(ctx context.Context, t Ticket, main string, result merge.Result) error {
	h := t.Handoff
	p := merge.Progress{Version: 1, QueueID: h.QueueID, TicketID: t.ID, Repository: h.Repository, PR: h.PR, Head: h.Head, Main: main, Phase: "waiting", Corrections: h.Corrections + result.Corrections, CatchupRounds: result.Rounds, Infra: h.Infra, Attempt: max(1, h.Attempt), Reason: result.Reason}
	if t.Progress != nil && t.Progress.QueueID == h.QueueID {
		mergechannel.RetainMergeProgress(&p, t.Progress)
	}
	human := result.Status == merge.Human || p.Corrections > machine.DefaultLimits.Corrections
	review := result.Status == merge.Review && !human
	// Lane 3 never reaches a resolver: automerge authorization excludes it, so
	// the ticket transfers straight to a human instead of Agent处置.
	direct := result.Status == merge.HumanDirect && !human
	// A held correction writes nothing; the merge lane reaches it again.
	if result.Status == merge.Correction && !human && r.memoryHeld {
		r.deferredCorrections = append(r.deferredCorrections, t)
		return nil
	}
	if result.Status == merge.Correction {
		p.Phase = "correction"
		p.Attempt = max(p.Attempt, t.Attempt)
	}
	if human || direct {
		p.Phase = "human"
	}
	raw, _ := json.Marshal(p)
	body := merge.ProgressPrefix + string(raw)
	if direct && autoMode(r.Repositories[t.Repo].Env) {
		return r.parkTicket(ctx, t, p.Reason, "merge policy", body)
	}
	if human || review || direct {
		if direct {
			body = "转人: " + p.Reason + "\n证据: merge policy\n阶段: " + string(machine.PhaseHuman) + "\n归属: " + string(machine.OwnerHuman) + "\n" + body
		} else {
			event, evidence, demand := p.Reason, "merge validation", "检查合并结果并重新排队"
			if review && strings.HasPrefix(p.Reason, errHeadRef.Error()) {
				event, evidence, demand = p.Reason, "PR head ref", "把 head 推到 agent/<票号>/<单元>，open-pr 后提交新的 merge handoff"
			} else if review && p.Reason == errPRIdentity.Error() {
				event, evidence, demand = p.Reason, "PR head/base 与冻结 handoff 不符", "核对 PR 当前 head、base 与仓库，重新验证 gate 和 review 后提交新的 merge handoff 与 queue_id，不得批准旧 handoff"
			} else if review && p.Reason == errPRClosed.Error() {
				event, evidence, demand = p.Reason, "PR state=closed 且 merged=false", "核对 PR 关闭原因并决定后续处理，不得直接批准旧 handoff；如需继续合并，验证有效的 open PR 后提交新的 merge handoff 与 queue_id，不自动重开或重建 PR"
			} else if review {
				event, evidence, demand = "lane 2 复核", "merge policy", "独立复核 PR 当前 head，通过后提交原 handoff 的 merge 提案"
			}
			if result.InfrastructureFailures > 0 {
				evidence = fmt.Sprintf("merge validation\n失败次数: %d\n最后失败步骤: %s", result.InfrastructureFailures, result.InfrastructureStep)
				demand = "修复合并验证基础设施后重新排队"
			}
			body = "Agent处置: " + event + "\n证据: " + evidence + "\n处置要求: " + demand + "\n阶段: " + string(machine.PhaseResolver) + "\n归属: " + string(machine.OwnerResolver) + "\n" + body
		}
	} else if result.Status == merge.Correction {
		body = fmt.Sprintf("阶段: %s\n归属: %s term=- run=%s attempt=%d\n%s", machine.PhaseCorrection, machine.OwnerExecutor, h.Run, p.Attempt, body)
	}
	if err := r.mergeComment(ctx, t, body); err != nil {
		return err
	}
	if human || review || direct {
		if _, err := r.Clients[t.Repo].UpdateIssue(ctx, t.ID, "In Progress", []string{"auto-impl", "auto-failed"}, []string{"ready-for-agent"}); err != nil {
			return errors.New("merge escalation unconfirmed")
		}
	}
	switch {
	case direct:
		r.board(ctx, t, machine.PhaseHuman, p.Reason)
	case human || review:
		r.board(ctx, t, machine.PhaseResolver, p.Reason)
	case result.Status == merge.Correction:
		r.board(ctx, t, machine.PhaseCorrection, p.Reason)
	default:
		r.board(ctx, t, machine.PhaseMergeQueue, p.Reason)
	}
	if human || review || direct {
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
	if !mergechannel.ProgressMatchesHandoff(p, h) {
		return 0, errors.New("conflicting merge decision")
	}
	if p.Phase == "correction" || p.Phase == "human" {
		return 0, errors.New("merge decision already transferred ownership")
	}
	return max(h.CatchupRounds, p.CatchupRounds), nil
}

// Resolver revalidation gives a new queue another attempt while retaining at
// most one lane round. The resolver's own merge of main is not another round.
func laneCatchupRounds(t Ticket) (int, error) {
	if t.Handoff == nil {
		return 0, nil
	}
	if t.Progress == nil || t.Progress.QueueID != t.Handoff.QueueID {
		return min(t.Handoff.CatchupRounds, 1), nil
	}
	if !mergechannel.ProgressMatchesHandoff(t.Progress, t.Handoff) {
		return 0, errors.New("conflicting merge decision")
	}
	return min(max(t.Handoff.CatchupRounds, t.Progress.CatchupRounds), 1), nil
}
