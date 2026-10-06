package merge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

// LedgerRow mirrors the executor run-ledger record so both lanes append rows
// in the same `ledger <json>` comment format consumed by ledger.sh / stats.go.
type LedgerRow struct {
	Ticket      string             `json:"ticket"`
	Lane        int                `json:"lane"`
	Tier        string             `json:"tier,omitempty"`
	Outcome     string             `json:"outcome"`
	FirstGate   string             `json:"first_gate,omitempty"`
	Corrections int                `json:"corrections,omitempty"`
	CIReds      int                `json:"ci_reds,omitempty"`
	PR          int                `json:"pr,omitempty"`
	Minutes     float64            `json:"minutes"`
	StartedAt   time.Time          `json:"started_at"`
	FinishedAt  time.Time          `json:"finished_at"`
	StageSecs   map[string]int     `json:"stage_secs,omitempty"`
	Usage       *usage.TicketUsage `json:"usage,omitempty"`
}

// AppendLine renders the `ledger <json>` comment line.
func (r LedgerRow) AppendLine() string {
	b, _ := json.Marshal(r)
	return "ledger " + string(b)
}

// mergeRowMatches dedupes on (ticket, started_at) against existing ledger lines.
func mergeRowMatches(body, ticket string, started time.Time) bool {
	raw, ok := strings.CutPrefix(body, "ledger ")
	if !ok {
		return false
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		return false
	}
	if row["ticket"] != ticket {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprintf("%v", row["started_at"]))
	return err == nil && at.Equal(started)
}

// listLedgerBodies returns the raw bodies of existing ledger comments.
type ledgerLister func(ctx context.Context) ([]string, error)
type ledgerCreater func(ctx context.Context, body string) error
type usageCollector func(ctx context.Context, ticket string) (*usage.TicketUsage, error)

// finishRunLedger appends one deduped ledger row for a finished merge-lane run.
// Usage-collection and store errors never block the caller; a failed usage
// collection only omits the usage field.
func finishRunLedger(ctx context.Context, row LedgerRow, list ledgerLister, create ledgerCreater, collect usageCollector, warn func(string, error)) bool {
	if create == nil || list == nil {
		return false
	}
	if !row.StartedAt.IsZero() {
		existing, err := list(ctx)
		if err != nil {
			if warn != nil {
				warn("ledger: list existing failed", err)
			}
		} else {
			for _, body := range existing {
				if mergeRowMatches(body, row.Ticket, row.StartedAt) {
					return false
				}
			}
		}
	}
	if collect != nil {
		if u, err := collect(ctx, row.Ticket); err != nil {
			if warn != nil {
				warn("ledger: usage collection failed; omitting usage", err)
			}
		} else {
			row.Usage = u
		}
	}
	if err := create(ctx, row.AppendLine()); err != nil {
		if warn != nil {
			warn("ledger: append failed", err)
		}
		return false
	}
	return true
}

// FinishRunLedger exposes finishRunLedger for the scheduler's merge lane.
func FinishRunLedger(ctx context.Context, row LedgerRow, list func(context.Context) ([]string, error), create func(context.Context, string) error, collect func(context.Context, string) (*usage.TicketUsage, error), warn func(string, error)) bool {
	return finishRunLedger(ctx, row, list, create, collect, warn)
}
