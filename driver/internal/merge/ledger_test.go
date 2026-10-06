package merge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

func TestLedgerOnFinish(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 6, 1, 2, 3, 1, time.UTC)
	record := LedgerRecord{Repository: "owner/repo", Team: "team", Project: "project", Stage: "merge", StageAt: start,
		Row: LedgerRow{Ticket: "ZII-896", StartedAt: start, StageSecs: map[string]float64{"worker": 2}}}
	t.Setenv("AGENT_LINEAR_TEAM", "wrong-team")
	t.Setenv("AGENT_LINEAR_PROJECT", "wrong-project")
	var rows []LedgerRow
	fail := true
	writer := &LedgerWriter{Now: func() time.Time { return start.Add(time.Minute) }, Collect: func(context.Context, string, []string) (usage.TicketUsage, error) { return usage.TicketUsage{}, nil }, Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
		if spec.Path == "" || len(spec.Args) != 3 || spec.Args[0] != "ledger" || spec.Args[1] != "append" || spec.Timeout <= 0 {
			t.Fatalf("command: %+v", spec)
		}
		if !slices.Contains(spec.Env, "AGENT_LINEAR_TEAM=team") || !slices.Contains(spec.Env, "AGENT_LINEAR_PROJECT=project") || slices.Contains(spec.Env, "AGENT_LINEAR_TEAM=wrong-team") || slices.Contains(spec.Env, "AGENT_LINEAR_PROJECT=wrong-project") {
			t.Fatal("incorrect ledger destination")
		}
		if fail {
			return nil, errors.New("append unavailable")
		}
		var row LedgerRow
		if err := json.Unmarshal([]byte(spec.Args[2]), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
		return nil, nil
	}}
	path := filepath.Join(t.TempDir(), "claim.json")
	if err := SaveLedgerRecord(path, record); err != nil {
		t.Fatal(err)
	}
	if err := FinishLedger(ctx, path, writer, "merged"); err == nil {
		t.Fatal("append failure swallowed")
	}
	stored, err := ReadLedgerRecord(path)
	if err != nil || stored.Written {
		t.Fatalf("failed append marked complete: %+v %v", stored, err)
	}
	fail = false
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := FinishLedger(ctx, path, writer, "merged"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(rows) != 1 || rows[0].StageSecs["merge"] != 60 || rows[0].StageSecs["worker"] != 2 || rows[0].Usage == nil {
		t.Fatalf("rows: %+v", rows)
	}
	// A separate claim in the same second must not collide with the first.
	record.Row.StartedAt = start.Add(time.Nanosecond)
	if err := SaveLedgerRecord(path, record); err != nil {
		t.Fatal(err)
	}
	if err := FinishLedger(ctx, path, writer, "escalated"); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].Outcome != "escalated" {
		t.Fatalf("claims: %+v", rows)
	}
}
