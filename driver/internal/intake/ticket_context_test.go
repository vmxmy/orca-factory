package intake

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestIntakeTicketContext covers OF-33: before every Agent turn, the project
// checkout's .intake/tickets.md lists the project's open ledger tickets
// (closed Done/Canceled/Duplicate tickets never show) with their blockers; a
// ledger read failure keeps the previous file with a staleness note instead
// of blocking the turn; and a rejected rolling summary backs Turns off to
// half the cap instead of requesting a new summary every round until Turns
// reaches the cap again.
func TestIntakeTicketContext(t *testing.T) {
	t.Setenv("AGENT_INTAKE_MAX_TURNS", "4")
	recordedNormal, err := filepath.Abs("../../testdata/recorded/claude-print-json-result.json")
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	fail := false
	tickets := func(context.Context) ([]TicketContext, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, errors.New("ledger unavailable")
		}
		return []TicketContext{
			{Identifier: "OF-1", State: "Todo", Title: "示例在途票", Blockers: []string{"OF-9"}},
			{Identifier: "OF-2", State: "Done", Title: "已完成的票", Blockers: nil},
		}, nil
	}

	// branchingScript's summary fixture is also recordedNormal: every
	// summary request this test triggers is rejected, which is what
	// exercises the Turns backoff.
	g, logs, _, checkout, taken := rollingSetup(t, branchingScript(recordedNormal, recordedNormal), tickets)
	ctx := context.Background()
	ticketsPath := filepath.Join(checkout, ".intake", "tickets.md")

	turn := func(id string) {
		t.Helper()
		event := recordedEvent(t, "bot-event-thread-at-reply")
		event.Event.Message.MessageId = ptr(id)
		if err := g.Handle(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	first := recordedEvent(t, "bot-event-message-at")
	if err := g.Handle(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, logs); len(got) != 1 {
		t.Fatalf("turn 1 calls %#v", got)
	}
	taken()

	content, err := os.ReadFile(ticketsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "OF-1") || !strings.Contains(string(content), "OF-9") {
		t.Fatalf("open ticket or its blocker missing: %q", content)
	}
	if strings.Contains(string(content), "OF-2") {
		t.Fatalf("closed ticket listed: %q", content)
	}

	mu.Lock()
	fail = true
	mu.Unlock()
	turn("om_ticketctx_turn2")
	if got := calls(t, logs); len(got) != 1 {
		t.Fatalf("ledger failure blocked the turn: %#v", got)
	}
	taken()

	stale, err := os.ReadFile(ticketsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(stale), "账本读取失败，内容可能过期（") {
		t.Fatalf("missing staleness note: %q", stale)
	}
	if !strings.Contains(string(stale), "OF-1") {
		t.Fatalf("stale file lost the previous ticket list: %q", stale)
	}

	mu.Lock()
	fail = false
	mu.Unlock()
	turn("om_ticketctx_turn3")
	if got := calls(t, logs); len(got) != 1 {
		t.Fatalf("turn 3 calls %#v", got)
	}
	taken()

	// Turns now reaches AGENT_INTAKE_MAX_TURNS (4): the gateway requests a
	// summary on top of the reply, and the stand-in rejects it.
	turn("om_ticketctx_turn4")
	if got := calls(t, logs); len(got) != 2 {
		t.Fatalf("turn 4 (reaches the cap) calls %#v", got)
	}
	taken()

	// The fix: a rejected summary backs Turns off to half the cap, so the
	// very next round does not request another summary.
	turn("om_ticketctx_turn5")
	if got := calls(t, logs); len(got) != 1 {
		t.Fatalf("turn 5 requested another summary before the cap: %#v", got)
	}
	taken()

	// Once Turns climbs back to the cap, the gateway requests a summary again.
	turn("om_ticketctx_turn6")
	if got := calls(t, logs); len(got) != 2 {
		t.Fatalf("turn 6 (reaches the cap again) calls %#v", got)
	}
	taken()
}
