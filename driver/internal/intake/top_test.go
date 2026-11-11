package intake

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

func TestTopRulingIgnoresBoundaryWords(t *testing.T) {
	h, ledger := humanTicket(t)
	h.ledger.tasks[0].Description = "## LANE 预判\n2\n\n## Change\n无需授权，在测试环境调整数据结构，不得操作生产数据。"
	decision, err := os.ReadFile("../../testdata/recorded/resolver-top-ruling.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decision), "无需授权") {
		t.Fatal("recording no longer covers harmless boundary words")
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "attempt": 7, "phase": "result", "top": true, "top_class": "brief-interpretation", "top_reason": "票面口径二选一", "outcome": "succeeded", "result": string(decision)})
	if err != nil {
		t.Fatal(err)
	}
	ledger.events["canon-of12"] = []string{resolver.Prefix + string(raw)}
	refusal, err := ApplyTopReview(context.Background(), ledger, "canon-of12", 7, "", time.Now())
	events := ledger.events["canon-of12"]
	if err != nil || refusal != "" || len(events) != 2 || !strings.HasPrefix(events[1], "裁定：top-resolver ") {
		t.Fatalf("boundary words blocked ruling: refusal=%q err=%v events=%q", refusal, err, events)
	}
}

func TestTopScopeUsesAutomaticLimits(t *testing.T) {
	for _, path := range []string{"src/new.go", "../secret", ".github/workflows/go.yml", "core/gate.sh", "billing/key", "a b c d e f g h i j k"} {
		t.Run(path, func(t *testing.T) {
			h, ledger := humanTicket(t)
			h.ledger.tasks[0].Description = "## LANE 预判\n2\n\n## SCOPE\nsrc/old.go\n"
			proposal, _ := json.Marshal(resolver.Resolution{Action: "escalate", Reason: "需要扩 SCOPE", Body: "已核对", Class: "scope-extension", Scope: strings.Fields(path)})
			record, _ := json.Marshal(map[string]any{"version": 1, "attempt": 7, "phase": "result", "top": true, "outcome": "failed", "result": string(proposal)})
			ledger.events["canon-of12"] = []string{resolver.Prefix + string(record)}
			refusal, err := ApplyTopReview(context.Background(), ledger, "canon-of12", 7, "billing/*", time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			events := ledger.events["canon-of12"]
			if path == "src/new.go" {
				if refusal != "" || len(events) != 2 || !strings.HasPrefix(events[1], "扩SCOPE：top-resolver ") || !strings.Contains(h.ledger.tasks[0].Description, "src/new.go") {
					t.Fatalf("scope not approved: %q %q", refusal, events)
				}
			} else if refusal == "" || len(events) != 1 {
				t.Fatalf("unsafe scope: %q %q", refusal, events)
			}
		})
	}
}

func TestTopRulingCannotReplaceOriginalBlock(t *testing.T) {
	for _, class := range []string{"credentials", "environment", "authorization", "irreversible", "production-ddl", "production-data", "证据冲突"} {
		t.Run(class, func(t *testing.T) {
			h, ledger := humanTicket(t)
			h.ledger.tasks[0].Description = "## LANE 预判\n2"
			record, _ := json.Marshal(map[string]any{"version": 1, "attempt": 7, "phase": "result", "top": true, "top_class": class, "outcome": "succeeded", "result": `{"action":"ruling","reason":"可以继续"}`})
			ledger.events["canon-of12"] = []string{resolver.Prefix + string(record)}
			want := "必须转人：" + class
			if class == "证据冲突" {
				ledger.events["canon-of12"] = append(ledger.events["canon-of12"], merge.HandoffPrefix+"invalid")
				want = "canon-of12 证据冲突：merge-handoff-v1 解析失败"
			}
			before := len(ledger.events["canon-of12"])
			refusal, err := ApplyTopReview(context.Background(), ledger, "canon-of12", 7, "", time.Now())
			if err != nil || refusal != want || len(ledger.events["canon-of12"]) != before {
				t.Fatalf("original block overwritten: %q %v", refusal, err)
			}
		})
	}
}

func TestTopRulingRejectsUnclassifiedBlock(t *testing.T) {
	for _, class := range []string{"", "unknown"} {
		t.Run(class, func(t *testing.T) {
			h, ledger := humanTicket(t)
			h.ledger.tasks[0].Description = "## LANE 预判\n2"
			raw, err := json.Marshal(resolver.Record{Version: 1, Attempt: 7, Phase: "result", Top: true, TopClass: class, Outcome: "succeeded", Result: `{"action":"ruling","reason":"可以继续"}`})
			if err != nil {
				t.Fatal(err)
			}
			ledger.events["canon-of12"] = []string{resolver.Prefix + string(raw)}
			refusal, err := ApplyTopReview(context.Background(), ledger, "canon-of12", 7, "", time.Now())
			if err != nil || refusal != "必须转人：原转人阻塞未分类" || len(ledger.events["canon-of12"]) != 1 {
				t.Fatalf("unclassified block released: refusal=%q err=%v events=%q", refusal, err, ledger.events["canon-of12"])
			}
		})
	}
}

func TestTopRulingChecksTicketBoundary(t *testing.T) {
	for _, face := range []string{"## LANE 预判\n3", "## Change\n调整实现"} {
		t.Run(face, func(t *testing.T) {
			h, ledger := humanTicket(t)
			h.ledger.tasks[0].Description = face
			ledger.events["canon-of12"] = []string{`resolver-state {"version":1,"attempt":7,"phase":"result","top":true,"top_class":"brief-interpretation","outcome":"succeeded","result":"{\"action\":\"ruling\",\"reason\":\"继续实施\"}"}`}
			refusal, err := ApplyTopReview(context.Background(), ledger, "canon-of12", 7, "", time.Now())
			if err != nil || refusal == "" || len(ledger.events["canon-of12"]) != 1 {
				t.Fatalf("ticket boundary bypassed: %q %v", refusal, err)
			}
		})
	}
}
