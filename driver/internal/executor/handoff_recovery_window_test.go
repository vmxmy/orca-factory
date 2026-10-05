package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

type createdPRLostResponse struct{ *fakeGitHub }

func (g *createdPRLostResponse) CreatePR(ctx context.Context, owner, repo string, req github.CreatePRRequest) (github.PullRequest, error) {
	pr, err := g.fakeGitHub.CreatePR(ctx, owner, repo, req)
	if err != nil {
		return pr, err
	}
	pr.Head.SHA = strings.Repeat("b", 40)
	g.existing = &pr
	return pr, context.Canceled
}

func TestStartRecoversPRCreatedBeforeFrozenHandoff(t *testing.T) {
	e, o, l, g, tools, ticket := fixture(t, strings.Repeat("b", 40))
	e.Orca = o
	e.GitHub = &createdPRLostResponse{g}
	if _, err := e.Execute(context.Background(), ticket); err == nil {
		t.Fatal("expected interruption after remote PR creation")
	}
	for _, body := range l.comments {
		if strings.Contains(body, merge.HandoffPrefix) {
			t.Fatal("handoff unexpectedly persisted before interruption")
		}
	}
	e.Config.Terminal = "replacement_terminal"
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || len(g.created) != 1 || len(o.workerRequests) != 1 || tools.gateCalls != 2 || tools.reviewCalls != 2 {
		t.Fatalf("restart must revalidate existing PR without another worker or PR: result=%+v err=%v creates=%d workers=%d gates=%d reviews=%d", result, err, len(g.created), len(o.workerRequests), tools.gateCalls, tools.reviewCalls)
	}
	var h *merge.Handoff
	for _, body := range l.comments {
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, merge.HandoffPrefix) {
				continue
			}
			var value merge.Handoff
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, merge.HandoffPrefix)), &value); err != nil {
				t.Fatal(err)
			}
			h = &value
			if !strings.Contains(body, "归属: 合并通道") {
				t.Fatal("handoff must be atomic with merge ownership")
			}
		}
	}
	if h == nil {
		t.Fatal("restart did not freeze handoff")
	}
	if err := h.Validated(); err != nil {
		t.Fatal(err)
	}
	if h.PR != 42 || h.Run != "run_one" || h.Task != "task_one" || h.Dispatch != "ctx_one" {
		t.Fatalf("handoff lost original dispatch: %+v", h)
	}
}
