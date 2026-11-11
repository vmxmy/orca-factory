package mergechannel

import "github.com/vmxmy/orca-factory/driver/internal/merge"

func ProgressMatchesHandoff(p *merge.Progress, h *merge.Handoff) bool {
	return p.Version == 1 && p.TicketID == h.TicketID && p.Repository == h.Repository && p.PR == h.PR && p.Head == h.Head && p.CatchupRounds >= 0
}

// Progress extensions share one receipt; carry both budgets and deployment when
// writing another phase of the same queue identity.
func RetainMergeProgress(next, prior *merge.Progress) {
	if prior == nil || prior.QueueID != next.QueueID {
		return
	}
	next.Deployment = prior.Deployment
	if next.Main == "" {
		next.Main = prior.Main
	}
	next.Corrections = max(next.Corrections, prior.Corrections)
	next.CatchupRounds = max(next.CatchupRounds, prior.CatchupRounds)
	next.Infra = max(next.Infra, prior.Infra)
	next.Attempt = max(next.Attempt, prior.Attempt)
	if next.Reason == "" {
		next.Reason = prior.Reason
	}
}
