package intake

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

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
	for _, reason := range []string{"需要授权", "凭证不足", "生产 DDL", "不可逆", "environment unavailable", "证据冲突"} {
		t.Run(reason, func(t *testing.T) {
			_, ledger := humanTicket(t)
			record, _ := json.Marshal(map[string]any{"version": 1, "attempt": 7, "phase": "result", "top": true, "top_reason": reason, "outcome": "succeeded", "result": `{"action":"ruling","reason":"可以继续"}`})
			ledger.events["canon-of12"] = []string{resolver.Prefix + string(record)}
			refusal, err := ApplyTopReview(context.Background(), ledger, "canon-of12", 7, "", time.Now())
			if err != nil || refusal == "" || len(ledger.events["canon-of12"]) != 1 {
				t.Fatalf("original block overwritten: %q %v", refusal, err)
			}
		})
	}
}

func TestTopRulingChecksTicketBoundary(t *testing.T) {
	for _, face := range []string{"## LANE 预判\n3", "## LANE 预判\n2\n## Change\n执行生产 DDL"} {
		t.Run(face, func(t *testing.T) {
			h, ledger := humanTicket(t)
			h.ledger.tasks[0].Description = face
			ledger.events["canon-of12"] = []string{`resolver-state {"version":1,"attempt":7,"phase":"result","top":true,"outcome":"succeeded","result":"{\"action\":\"ruling\",\"reason\":\"继续实施\"}"}`}
			refusal, err := ApplyTopReview(context.Background(), ledger, "canon-of12", 7, "", time.Now())
			if err != nil || refusal == "" || len(ledger.events["canon-of12"]) != 1 {
				t.Fatalf("ticket boundary bypassed: %q %v", refusal, err)
			}
		})
	}
}
