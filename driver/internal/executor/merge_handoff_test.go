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

func TestExecuteFreezesMergeHandoff(t *testing.T) {
	e, _, l, _, _, ticket := fixture(t, strings.Repeat("b", 40))
	result, err := e.Execute(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	var h *merge.Handoff
	for _, body := range l.comments {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "merge-handoff-v1: ") {
				var value merge.Handoff
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "merge-handoff-v1: ")), &value); err != nil {
					t.Fatal(err)
				}
				h = &value
				if !strings.Contains(body, "归属: 合并通道") {
					t.Fatal("evidence not atomic with ownership")
				}
			}
		}
	}
	if h == nil {
		t.Fatal("executor transferred ownership without durable frozen validation")
	}
	if h.Head != strings.Repeat("b", 40) || h.Baseline != "merge-base-sha" || h.PR != 42 || h.Repository != "acme/demo" || h.TicketID != "issue_one" || h.Identifier != "ZII-861" || h.Unit != "executor" || h.Run != "run_one" || h.Task != "task_one" || h.Dispatch != result.DispatchID || h.Worktree != "/worktrees/ZII-861-executor" || h.Attempt != 1 || h.Lane != 2 || h.QueueID == "" || h.EnqueuedAt != e.Now() {
		t.Fatalf("incorrect frozen handoff: %+v", h)
	}
	if !strings.Contains(h.GateSpec, "CHECK: go test ./unit") || !strings.Contains(h.GateOutput, "VERDICT   green") || !strings.Contains(h.ReviewOutput, `"verdict":"green"`) {
		t.Fatalf("lost original validation: %+v", h)
	}
}

func TestStartExistingPRPreservesFrozenHandoff(t *testing.T) {
	e, _, l, g, _, ticket := fixture(t, strings.Repeat("b", 40))
	ticket.NewTest, ticket.TestFiles, ticket.Assertion = true, "test/probe.sh test/assertion.json", "test/assertion.json"
	result, err := e.Execute(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	g.existing = &result.PR
	g.existing.Head.SHA = strings.Repeat("b", 40)
	before := append([]string(nil), l.comments...)
	ticket.Assertion = "test/changed.json"
	e.Config.Merging = true
	resumed, err := e.Start(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.comments) != len(before) || resumed.RunID != "run_one" || resumed.DispatchID != "ctx_one" {
		t.Fatalf("Start replaced evidence/association: comments=%v result=%+v", l.comments, resumed)
	}
}
func TestStartExistingPRWithoutHandoffHandsToResolver(t *testing.T) {
	head := strings.Repeat("b", 40)
	e, o, l, g, tools, ticket := fixture(t, head)
	g.existing = &github.PullRequest{Number: 42, HTMLURL: "https://example.test/pr/42"}
	g.existing.Head.SHA = head
	result, err := e.Start(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Owner != machine.OwnerResolver || result.State.Phase != machine.PhaseResolver || result.State.HumanReason != "existing PR has no frozen handoff" || result.State.Attempt != 2 {
		t.Fatalf("unvalidated PR was not handed to resolver: %+v", result.State)
	}
	if len(l.comments) != 1 || !strings.HasPrefix(l.comments[0], "Agent处置: existing PR has no frozen handoff\n") {
		t.Fatalf("expected one resolver event: %v", l.comments)
	}
	for _, want := range []string{"PR #42", head, "阶段: Agent处置", "归属: 处置Agent term=term_exec run=- attempt=2"} {
		if !strings.Contains(l.comments[0], want) {
			t.Errorf("resolver event missing %q: %s", want, l.comments[0])
		}
	}
	if len(o.workerRequests) != 0 || tools.gateCalls != 0 || tools.reviewCalls != 0 {
		t.Fatalf("starts=%d gate=%d review=%d", len(o.workerRequests), tools.gateCalls, tools.reviewCalls)
	}
}
