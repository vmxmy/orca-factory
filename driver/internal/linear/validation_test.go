package linear

import (
	"strings"
	"testing"
	"time"
)

func TestValidateSingleTicketLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	report := ValidateSingleTicket(SingleTicketValidation{
		Now: now, Webhook: WebhookReady,
		Issue: Issue{ID: "issue-1", Identifier: "ZII-913", State: NamedID{Name: "Done"}, UpdatedAt: now.Add(-time.Minute)},
		Comments: []Comment{
			{Body: "阶段: executor\n归属: executor term=term-1 run=run-1"},
			{Body: "merge-handoff-v1: {\"version\":1,\"ticket_id\":\"issue-1\",\"identifier\":\"ZII-913\",\"repository\":\"o/r\",\"unit\":\"unit\",\"pr\":7,\"head\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"baseline\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"gate_spec\":\"UNIT: unit\\nCHECK: make test\\nSCOPE: driver/**\",\"gate_output\":\"UNIT unit\\nVERDICT green\\nSTEP all\\nREASON passed\\nEVIDENCE test\\nSCOPE driver/**\\nLANE 2\",\"review_output\":\"{\\\"verdict\\\":\\\"green\\\",\\\"step\\\":\\\"review\\\"}\",\"lane\":2,\"run\":\"run-1\",\"task\":\"task-1\",\"dispatch\":\"dispatch-1\",\"worktree\":\"/tmp/wt\",\"attempt\":1,\"enqueued_at\":\"2026-10-07T11:00:00Z\",\"queue_id\":\"queue-1\"}"},
			{Body: "merge-progress-v1: {\"version\":1,\"queue_id\":\"queue-1\",\"ticket_id\":\"issue-1\",\"repository\":\"o/r\",\"pr\":7,\"head\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"merge_sha\":\"cccccccccccccccccccccccccccccccccccccccc\",\"phase\":\"merged\"}"},
		},
	})
	if !report.Valid {
		t.Fatalf("report invalid: %+v", report)
	}
	for _, stage := range []string{"claim", "worker", "review", "ci", "merge", "done"} {
		if !report.Stages[stage] {
			t.Errorf("stage %q not observed: %+v", stage, report.Stages)
		}
	}
	if strings.Contains(report.String(), "aaaaaaaa") || strings.Contains(report.String(), "task-1") {
		t.Fatal("validation report leaked durable identifiers")
	}
}

func TestValidateSingleTicketFailsClosedOnStaleOrUnknownEvidence(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	report := ValidateSingleTicket(SingleTicketValidation{
		Now: now, Webhook: WebhookDegraded,
		Issue: Issue{ID: "issue-1", Identifier: "ZII-913", State: NamedID{Name: "Done"}, UpdatedAt: now.Add(-10 * time.Minute)},
	})
	if report.Valid || !report.Stale || report.Webhook != WebhookDegraded || report.Reason == "" {
		t.Fatalf("stale/unknown evidence admitted: %+v", report)
	}
}
