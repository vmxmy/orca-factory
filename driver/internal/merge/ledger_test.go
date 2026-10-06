package merge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

func parseLedgerRow(t *testing.T, body string) map[string]any {
	t.Helper()
	raw, ok := strings.CutPrefix(body, "ledger ")
	if !ok {
		t.Fatalf("not a ledger line: %q", body)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatalf("invalid ledger json: %v", err)
	}
	return row
}

// The merge lane and the executor handoff each append one ledger row; usage
// collection failure only omits usage; repeats are deduped on (ticket, started_at).
func TestLedgerOnFinish(t *testing.T) {
	started := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	usageOK := func(context.Context, string) (*usage.TicketUsage, error) {
		return &usage.TicketUsage{Ticket: "ZII-896", Totals: usage.Totals{Input: 10, Output: 5, CostUSD: 0.001}}, nil
	}
	usageFail := func(context.Context, string) (*usage.TicketUsage, error) {
		return nil, errors.New("no transcripts")
	}

	// Merge path: one merged row with usage, repeated call is a no-op.
	var mergeComments []string
	mergeList := func(context.Context) ([]string, error) { return mergeComments, nil }
	mergeCreate := func(_ context.Context, body string) error {
		mergeComments = append(mergeComments, body)
		return nil
	}
	row := LedgerRow{Ticket: "ZII-896", Lane: 2, Outcome: "merged", PR: 171,
		Corrections: 2, Minutes: 12, StartedAt: started, FinishedAt: started.Add(12 * time.Minute)}
	if !FinishRunLedger(context.Background(), row, mergeList, mergeCreate, usageOK, nil) {
		t.Fatal("merge ledger row not written")
	}
	if FinishRunLedger(context.Background(), row, mergeList, mergeCreate, usageOK, nil) {
		t.Fatal("duplicate merge ledger row written")
	}
	if len(mergeComments) != 1 {
		t.Fatalf("merge comments=%d", len(mergeComments))
	}
	m := parseLedgerRow(t, mergeComments[0])
	if m["outcome"] != "merged" || m["ticket"] != "ZII-896" || m["usage"] == nil {
		t.Fatalf("merge row: %v", m)
	}

	// Executor handoff path: escalated row; failed usage collection omits usage.
	var execComments []string
	execList := func(context.Context) ([]string, error) { return execComments, nil }
	execCreate := func(_ context.Context, body string) error {
		execComments = append(execComments, body)
		return nil
	}
	execRow := LedgerRow{Ticket: "ZII-896", Lane: 2, Outcome: "escalated",
		Corrections: 3, StartedAt: started, FinishedAt: started.Add(3 * time.Minute)}
	if !FinishRunLedger(context.Background(), execRow, execList, execCreate, usageFail, nil) {
		t.Fatal("executor ledger row not written")
	}
	if len(execComments) != 1 {
		t.Fatalf("exec comments=%d", len(execComments))
	}
	e := parseLedgerRow(t, execComments[0])
	if e["outcome"] != "escalated" || e["usage"] != nil {
		t.Fatalf("exec row should omit usage on collection failure: %v", e)
	}

	// Distinct started_at on the same ticket writes a new row (new claim).
	second := row
	second.StartedAt = started.Add(time.Hour)
	if !FinishRunLedger(context.Background(), second, mergeList, mergeCreate, usageOK, nil) {
		t.Fatal("new claim row not written")
	}
	if len(mergeComments) != 2 {
		t.Fatalf("after new claim merge comments=%d", len(mergeComments))
	}
}
