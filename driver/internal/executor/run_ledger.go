package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

// LedgerRow is one run record appended to the ledger when a ticket finishes.
// Field names match the existing ledger.sh / stats.go format.
type LedgerRow struct {
	Ticket      string             `json:"ticket"`
	Lane        int                `json:"lane"`
	Tier        string             `json:"tier,omitempty"`
	Outcome     string             `json:"outcome"`
	FirstGate   string             `json:"first_gate,omitempty"`
	Corrections int                `json:"corrections,omitempty"`
	CiReds      int                `json:"ci_reds,omitempty"`
	PR          int                `json:"pr,omitempty"`
	Minutes     float64            `json:"minutes"`
	StartedAt   time.Time          `json:"started_at"`
	FinishedAt  time.Time          `json:"finished_at"`
	StageSecs   map[string]int     `json:"stage_secs,omitempty"`
	Usage       *usage.TicketUsage `json:"usage,omitempty"`
}

// AppendLine renders the `ledger <json>` comment line for a finished run.
func (r LedgerRow) AppendLine() string {
	b, _ := json.Marshal(r)
	return "ledger " + string(b)
}

// LedgerWriter creates a durable ledger comment with at-most-once semantics
// per (ticket, started_at). Usage-collection and store errors never block
// the caller; a failed usage collection only omits the usage field.
type LedgerWriter struct {
	Create  func(ctx context.Context, body string) error
	List    func(ctx context.Context) ([]string, error)
	Logger  *slog.Logger
	Collect func(ctx context.Context, ticket string) (*usage.TicketUsage, error)
	Now     func() time.Time
}

func (w *LedgerWriter) logErr(msg string, err error) {
	if w.Logger != nil {
		w.Logger.Warn(msg, "err", err)
	}
}

// Finish records one ledger row and reports whether a new row was written.
func (w *LedgerWriter) Finish(ctx context.Context, row LedgerRow) bool {
	if w == nil || w.Create == nil || w.List == nil {
		return false
	}
	if !row.StartedAt.IsZero() {
		existing, err := w.List(ctx)
		if err != nil {
			w.logErr("ledger: list existing failed", err)
		} else {
			for _, body := range existing {
				if ledgerRowMatches(body, row.Ticket, row.StartedAt) {
					return false
				}
			}
		}
	}
	if w.Collect != nil {
		if u, err := w.Collect(ctx, row.Ticket); err != nil {
			w.logErr("ledger: usage collection failed; omitting usage", err)
		} else {
			row.Usage = u
		}
	}
	if err := w.Create(ctx, row.AppendLine()); err != nil {
		w.logErr("ledger: append failed", err)
		return false
	}
	return true
}

// ledgerRowMatches reports whether a stored ledger comment carries the same
// (ticket, started_at) dedupe key.
func ledgerRowMatches(body, ticket string, started time.Time) bool {
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

// StageSecsFromDurations builds the stage_secs map from per-phase durations.
func StageSecsFromDurations(durations map[string]time.Duration) map[string]int {
	if len(durations) == 0 {
		return nil
	}
	out := map[string]int{}
	keys := make([]string, 0, len(durations))
	for k := range durations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = int(durations[k].Seconds())
	}
	return out
}
