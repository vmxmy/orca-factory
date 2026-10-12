package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

// Merge-lane decisions and deployment pauses move the ticket's frozen worktree
// card; every card keeps exactly one issue line so pruning can map it back.
func TestMergeLaneMirrorsDecisionsOnOrcaBoard(t *testing.T) {
	ctx := context.Background()
	f := newMergeFixture(t)
	tickets, err := f.runtime.Snapshot(ctx, f.config.Repositories)
	if err != nil || len(tickets) != 1 || tickets[0].Handoff == nil {
		t.Fatalf("snapshot=%+v err=%v", tickets, err)
	}
	ticket := tickets[0]
	if err := f.runtime.mergeDecision(ctx, ticket, f.main, merge.Result{Status: merge.Waiting, Reason: "main advanced"}); err == nil {
		t.Fatal("waiting decision must report its reason")
	}
	progress := &merge.Progress{Version: 1, QueueID: ticket.Handoff.QueueID, Phase: "merged", Deployment: &merge.DeploymentProgress{State: "pending"}}
	if err := f.runtime.pauseDeployment(ctx, ticket, progress, "smoke failed"); err == nil {
		t.Fatal("deployment pause must report its reason")
	}
	cards := f.runtime.Orca.(*fakeOrca).cards
	if len(cards) != 2 || cards[0].Status != "agent-merge" || cards[1].Status != "agent-resolve" || cards[0].Unread || !cards[1].Unread {
		t.Fatalf("cards=%+v", cards)
	}
	for _, c := range cards {
		if c.Path != ticket.Handoff.Worktree || c.PR != 1 || strings.Count(c.Comment, "\nissue: ") != 1 || !strings.HasSuffix(c.Comment, "\nissue: ENG-1") {
			t.Fatalf("card=%+v", c)
		}
	}
}
