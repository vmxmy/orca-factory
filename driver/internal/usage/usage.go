// Package usage defines the shared per-ticket API usage contract.
package usage

import "context"

// Totals records token counts and their estimated cost at API prices in USD.
type Totals struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Reasoning  int64   `json:"reasoning"`
	CostUSD    float64 `json:"cost_usd"`
}

// RoleUsage partitions usage by the role that incurred it.
type RoleUsage struct {
	Worker      Totals `json:"worker"`
	Reviewer    Totals `json:"reviewer"`
	Coordinator Totals `json:"coordinator"`
	Session     Totals `json:"session"`
}

// TicketUsage includes totals and two independent breakdowns of those totals.
// PriceVersion identifies the API price table used for every cost in this record.
type TicketUsage struct {
	Totals
	Ticket       string            `json:"ticket"`
	ByRole       RoleUsage         `json:"by_role"`
	ByModel      map[string]Totals `json:"by_model,omitempty"`
	PriceVersion string            `json:"price_version"`
	// Apportioned is true when totals include allocated coordinator usage.
	Apportioned bool `json:"apportioned"`
}

// Collect is an interface stub. Worktrees contains filesystem paths belonging to
// the ticket; collection will be implemented separately. No usage is read yet.
func Collect(ctx context.Context, ticket string, worktrees []string) (TicketUsage, error) {
	return TicketUsage{}, nil
}
