package linear

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

const validationMaxAge = 5 * time.Minute

// SingleTicketValidation is the credential-free evidence needed to inspect one
// ticket. Callers must supply a remote or complete projection observation.
type SingleTicketValidation struct {
	Now      time.Time
	Issue    Issue
	Comments []Comment
	Webhook  WebhookMode
}

// ValidationReport contains only stable stage names and safe aggregate status.
type ValidationReport struct {
	Valid   bool
	Stale   bool
	Webhook WebhookMode
	Stages  map[string]bool
	Reason  string
}

func (r ValidationReport) String() string {
	return fmt.Sprintf("valid=%t stale=%t webhook=%s stages=%d reason=%s", r.Valid, r.Stale, r.Webhook, len(r.Stages), r.Reason)
}

// ValidateSingleTicket deterministically checks claim, worker, reviewer, CI,
// merge, and Done evidence. Missing, conflicting, or stale evidence fails shut.
func ValidateSingleTicket(input SingleTicketValidation) ValidationReport {
	r := ValidationReport{Webhook: input.Webhook, Stages: map[string]bool{}}
	if input.Now.IsZero() {
		r.Reason = "observation time unknown"
		return r
	}
	if input.Issue.ID == "" || input.Issue.Identifier == "" || input.Issue.UpdatedAt.IsZero() {
		r.Reason = "ticket evidence unknown"
		return r
	}
	age := input.Now.Sub(input.Issue.UpdatedAt)
	if age < 0 || age > validationMaxAge {
		r.Stale = true
		r.Reason = "ticket evidence stale"
		return r
	}
	if input.Webhook != WebhookReady {
		r.Reason = "webhook mode degraded"
		return r
	}
	var handoff merge.Handoff
	var progress merge.Progress
	for _, comment := range input.Comments {
		body := strings.TrimSpace(comment.Body)
		switch {
		case strings.HasPrefix(body, "阶段: executor") && strings.Contains(body, "归属: executor") && strings.Contains(body, "term=") && strings.Contains(body, "run="):
			r.Stages["claim"] = true
		case strings.HasPrefix(body, merge.HandoffPrefix):
			var candidate merge.Handoff
			if json.Unmarshal([]byte(strings.TrimPrefix(body, merge.HandoffPrefix)), &candidate) == nil {
				if handoff.Version != 0 {
					r.Reason = "conflicting handoff evidence"
					return r
				}
				handoff = candidate
			}
		case strings.HasPrefix(body, merge.ProgressPrefix):
			var candidate merge.Progress
			if json.Unmarshal([]byte(strings.TrimPrefix(body, merge.ProgressPrefix)), &candidate) == nil && candidate.Phase == "merged" {
				if progress.Phase != "" {
					r.Reason = "conflicting merge evidence"
					return r
				}
				progress = candidate
			}
		}
	}
	if !r.Stages["claim"] {
		r.Reason = "claim evidence unknown"
		return r
	}
	if handoff.Version == 0 || handoff.TicketID != input.Issue.ID || handoff.Identifier != input.Issue.Identifier {
		r.Reason = "worker evidence unknown"
		return r
	}
	if err := handoff.Validated(); err != nil {
		r.Reason = "worker or reviewer evidence invalid"
		return r
	}
	r.Stages["worker"], r.Stages["review"], r.Stages["ci"] = true, true, true
	if progress.Phase != "merged" || progress.TicketID != handoff.TicketID || progress.QueueID != handoff.QueueID || progress.MergeSHA == "" || progress.Head != handoff.Head {
		r.Reason = "merge evidence unknown"
		return r
	}
	r.Stages["merge"] = true
	if input.Issue.State.Name != "Done" {
		r.Reason = "Done state unconfirmed"
		return r
	}
	r.Stages["done"] = true
	r.Valid = true
	return r
}
