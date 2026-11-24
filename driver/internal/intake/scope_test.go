package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestAutoScopeApprovalLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history []task.Comment
		refusal string
	}{
		{name: "fourth approval across stages", history: []task.Comment{{Body: "扩SCOPE：auto old"}, {Body: "扩SCOPE：top-resolver old"}, {Body: "扩SCOPE：worker-auto old"}}},
		{name: "four approvals exhausted", history: []task.Comment{{Body: "扩SCOPE：auto old"}, {Body: "扩SCOPE：top-resolver old"}, {Body: "扩SCOPE：worker-auto old"}, {Body: "扩SCOPE：auto newer"}}, refusal: "每张票自动批准已达 4 次"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := scopedTicket(t, `{"action":"escalate","class":"scope-extension","reason":"scope","body":"proof","scope":["new.go"]}`)
			ledger := &humanLedger{fakeLedger: h.ledger, comments: tc.history}
			before := h.ledger.tasks[0].Description
			refusal, err := AutoApproveScope(context.Background(), ledger, "OF-12", 7, "", h.g.now())
			if err != nil || refusal != tc.refusal {
				t.Fatalf("refusal=%q want=%q err=%v", refusal, tc.refusal, err)
			}
			events := h.ledger.events["canon-of12"]
			if tc.refusal != "" {
				if h.ledger.tasks[0].Description != before || len(h.ledger.edits) != 0 || len(events) != 1 {
					t.Fatalf("refusal changed ticket: edits=%q events=%q", h.ledger.edits, events)
				}
				return
			}
			if len(h.ledger.edits) != 1 || !strings.Contains(h.ledger.tasks[0].Description, "a.go b.go new.go") || len(events) != 2 || !strings.Contains(events[1], "本票第 4 次自动批准，不超过 4 次") {
				t.Fatalf("fourth approval missing: edits=%q events=%q", h.ledger.edits, events)
			}
		})
	}
}

func TestAutoScopePersistsLimitAndRetries(t *testing.T) {
	h := scopedTicket(t, `{"action":"escalate","class":"scope-extension","reason":"scope","body":"proof","scope":["new.go"]}`)
	ledger := &humanLedger{fakeLedger: h.ledger, commentErr: errors.New("comment unavailable")}
	ctx := context.Background()
	if _, err := AutoApproveScope(ctx, ledger, "OF-12", 7, "", h.g.now()); err == nil {
		t.Fatal("lost comment failure")
	}
	if len(h.ledger.edits) != 1 || len(h.ledger.events["canon-of12"]) != 1 {
		t.Fatalf("edits=%v events=%v", h.ledger.edits, h.ledger.events)
	}
	ledger.commentErr = nil
	for range 2 {
		refusal, err := AutoApproveScope(ctx, ledger, "OF-12", 7, "", h.g.now())
		if err != nil || refusal != "" {
			t.Fatalf("refusal=%q err=%v", refusal, err)
		}
	}
	if len(h.ledger.edits) != 1 || len(h.ledger.events["canon-of12"]) != 2 {
		t.Fatalf("retry duplicated writes: %+v", h.ledger)
	}
	for attempt := 8; attempt <= 11; attempt++ {
		result := fmt.Sprintf(`{"action":"escalate","class":"scope-extension","reason":"scope","body":"proof","scope":["new%d.go"]}`, attempt)
		raw, err := json.Marshal(resolver.Record{Version: 1, Attempt: attempt, Phase: "human", Result: result})
		if err != nil {
			t.Fatal(err)
		}
		h.ledger.events["canon-of12"] = append(h.ledger.events["canon-of12"], resolver.Prefix+string(raw))
		// Each call rereads the persisted comments, as a restarted scheduler does.
		refusal, err := AutoApproveScope(ctx, ledger, "OF-12", attempt, "", h.g.now())
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 11 && refusal != "" {
			t.Fatal(refusal)
		}
		if attempt == 11 && !strings.Contains(refusal, "4 次") {
			t.Fatalf("fifth request: %q", refusal)
		}
	}
	if len(h.ledger.edits) != 4 {
		t.Fatalf("edits=%v", h.ledger.edits)
	}
}

func TestAutoScopePreservesHumanGuards(t *testing.T) {
	for _, tc := range []struct{ name, face, comment, state, refusal string }{
		{name: "lane 3 ticket", face: "## LANE 预判\n3\n## SCOPE\na.go", refusal: "lane 3"},
		{name: "missing lane", face: "## SCOPE\na.go", refusal: "LANE"},
		{name: "missing scope", face: "## LANE 预判\n2", refusal: "SCOPE"},
		{name: "canceled", state: "Canceled", refusal: "取消"},
		{name: "lane 3 handoff", comment: `merge-handoff-v1: {"version":1,"lane":3}`, refusal: "lane 3"},
		{name: "deployment paused", comment: `merge-progress-v1: {"version":1,"deployment":{"state":"paused"}}`, refusal: "paused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := scopedTicket(t, `{"action":"escalate","class":"scope-extension","reason":"scope","body":"proof","scope":["new.go"]}`)
			if tc.face != "" {
				h.ledger.tasks[0].Description = tc.face
			}
			if tc.state != "" {
				h.ledger.tasks[0].Metadata.State = tc.state
			}
			ledger := &humanLedger{fakeLedger: h.ledger, comments: []task.Comment{{Body: tc.comment}}}
			refusal, err := AutoApproveScope(context.Background(), ledger, "OF-12", 7, "", h.g.now())
			if err != nil || !strings.Contains(refusal, tc.refusal) || len(h.ledger.edits) != 0 || len(h.ledger.events["canon-of12"]) != 1 {
				t.Fatalf("refusal=%q err=%v ledger=%+v", refusal, err, h.ledger)
			}
		})
	}
}

// The recorded ledger has no automatic receipt yet: the feature is unreleased.
// Comments[2] of ledger-comments-human-release.json is a real driver receipt
// read back from the ledger with the same shape applyScopeReview writes. The
// derived receipts keep its ID, time, line breaks and resolver-state line and
// replace only the leading "放行：<approver>" with "扩SCOPE：auto".
func TestRecordedAutoScopeApprovalCount(t *testing.T) {
	recorded, err := os.ReadFile("../../testdata/recorded/ledger-comments-human-release.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded, &recording); err != nil {
		t.Fatal(err)
	}
	receipt := recording.Comments[2]
	const approver = "放行：ou_00000000000000000000000000000000"
	if !strings.HasPrefix(receipt.Body, approver+" ") {
		t.Fatalf("recorded receipt changed: %q", receipt.Body)
	}
	auto := receipt
	auto.Body = "扩SCOPE：auto" + strings.TrimPrefix(receipt.Body, approver)
	for _, tc := range []struct {
		name     string
		history  []task.Comment
		approved string
	}{
		{name: "recorded human receipts", history: []task.Comment{receipt, receipt}, approved: "第 1 次"},
		{name: "one auto receipt", history: []task.Comment{receipt, auto}, approved: "第 2 次"},
		{name: "four auto receipts", history: []task.Comment{auto, auto, auto, auto}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := scopedTicket(t, `{"action":"escalate","class":"scope-extension","reason":"scope","body":"proof","scope":["new.go"]}`)
			// Evidence is ordered by CreatedAt, so the current request follows the receipts.
			request := task.Comment{Body: h.ledger.events["canon-of12"][0], CreatedAt: receipt.CreatedAt.Add(time.Hour)}
			h.ledger.events["canon-of12"] = nil
			ledger := &humanLedger{fakeLedger: h.ledger, comments: append(tc.history, request)}
			refusal, err := AutoApproveScope(context.Background(), ledger, "OF-12", 7, "", h.g.now())
			if err != nil {
				t.Fatal(err)
			}
			events := h.ledger.events["canon-of12"]
			if tc.approved == "" {
				if refusal != "每张票自动批准已达 4 次" || len(h.ledger.edits) != 0 || len(events) != 0 {
					t.Fatalf("refusal=%q edits=%q events=%q", refusal, h.ledger.edits, events)
				}
				return
			}
			wantFace := "## 要做\nx\n\n## LANE 预判\n2\n\n## SCOPE\na.go b.go new.go\n\n## 依赖\nDEPENDENCIES: {}"
			want := "扩SCOPE：auto 2026-11-01T12:00:00Z\n条件：AGENT_AUTO_REVIEW=scope；字面路径校验通过；1 个路径不超过 10；不命中 lane 3 或控制文件；本票" + tc.approved + "自动批准，不超过 4 次\n路径：new.go\n" + waitingReceipt
			if refusal != "" || len(h.ledger.edits) != 1 || h.ledger.edits[0] != wantFace || len(events) != 1 || events[0] != want {
				t.Fatalf("refusal=%q edits=%q events=%q", refusal, h.ledger.edits, events)
			}
		})
	}
}

func TestWorkerAutoScopeAuditPreservesEvidenceAndCountsAcrossStages(t *testing.T) {
	h := scopedTicket(t, `{"action":"escalate","class":"scope-extension","reason":"scope","body":"proof","scope":["resolver.go"]}`)
	ctx := context.Background()
	before, err := h.ledger.ListComments(ctx, "canon-of12")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		approved, err := AutoApproveWorkerScope(ctx, h.ledger, "OF-12", []string{"docs/system-manual.md"}, "", h.g.now())
		if err != nil || !approved {
			t.Fatalf("approved=%v err=%v", approved, err)
		}
	}
	after, err := h.ledger.ListComments(ctx, "canon-of12")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(latestHumanEvidence(before), latestHumanEvidence(after)) || len(after) != len(before)+1 {
		t.Fatalf("audit changed protocol or duplicated approval: before=%+v after=%+v", before, after)
	}
	refusal, err := AutoApproveScope(ctx, h.ledger, "OF-12", 7, "", h.g.now())
	if err != nil || refusal != "" {
		t.Fatalf("second approval: refusal=%q err=%v", refusal, err)
	}
	events := h.ledger.events["canon-of12"]
	if !strings.Contains(events[len(events)-1], "第 2 次自动批准") {
		t.Fatalf("worker approval not counted: %q", events)
	}
	approved, err := AutoApproveWorkerScope(ctx, h.ledger, "OF-12", []string{"docs/third.md"}, "", h.g.now())
	if err != nil || !approved {
		t.Fatalf("third approval: approved=%v err=%v", approved, err)
	}
	approved, err = AutoApproveWorkerScope(ctx, h.ledger, "OF-12", []string{"docs/fourth.md"}, "", h.g.now())
	if err != nil || !approved {
		t.Fatalf("fourth approval: approved=%v err=%v", approved, err)
	}
	events = h.ledger.events["canon-of12"]
	if !strings.Contains(events[len(events)-1], "第 4 次自动批准") {
		t.Fatalf("fourth approval missing from audit: %q", events)
	}
	approved, err = AutoApproveWorkerScope(ctx, h.ledger, "OF-12", []string{"docs/fifth.md"}, "", h.g.now())
	if err != nil || approved {
		t.Fatalf("fifth approval: approved=%v err=%v", approved, err)
	}
}
