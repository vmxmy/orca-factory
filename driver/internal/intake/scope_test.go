package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

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
	for attempt := 8; attempt <= 9; attempt++ {
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
		if attempt == 8 && refusal != "" {
			t.Fatal(refusal)
		}
		if attempt == 9 && !strings.Contains(refusal, "2 次") {
			t.Fatalf("third request: %q", refusal)
		}
	}
	if len(h.ledger.edits) != 2 {
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
