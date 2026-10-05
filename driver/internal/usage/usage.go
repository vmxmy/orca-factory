// Package usage defines the shared per-ticket API usage contract.
package usage

import (
	"context"
	"math"
)

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

// Collect reads the transcripts of the given worktrees (all treated as unit worktrees
// of the ticket) and fills the shared contract. Reviewer and coordinator attribution
// need the richer CollectItems options and are not available through this signature.
// A ticket without worktrees is an error: nothing can be attributed to it.
func Collect(ctx context.Context, ticket string, worktrees []string) (TicketUsage, error) {
	if err := ctx.Err(); err != nil {
		return TicketUsage{}, err
	}
	options := Options{Ticket: ticket}
	for _, path := range worktrees {
		options.Worktrees = append(options.Worktrees, Worktree{Path: path})
	}
	result, err := CollectItems(options)
	if err != nil {
		return TicketUsage{}, err
	}
	out := TicketUsage{Ticket: result.Ticket, PriceVersion: result.PriceVersion, ByModel: map[string]Totals{}}
	for _, item := range result.Items {
		part := Totals{
			Input:      int64(math.Round(item.InputTokens)),
			Output:     int64(math.Round(item.OutputTokens)),
			CacheRead:  int64(math.Round(item.CacheReadTokens)),
			CacheWrite: int64(math.Round(item.CacheWriteTokens + item.CacheWriteHourTokens)),
			CostUSD:    item.CostUSD,
		}
		out.Totals = out.Totals.add(part)
		out.ByModel[item.Model] = out.ByModel[item.Model].add(part)
		switch item.Role {
		case "worker":
			out.ByRole.Worker = out.ByRole.Worker.add(part)
		case "reviewer":
			out.ByRole.Reviewer = out.ByRole.Reviewer.add(part)
		case "coordinator":
			out.ByRole.Coordinator = out.ByRole.Coordinator.add(part)
		default:
			out.ByRole.Session = out.ByRole.Session.add(part)
		}
		out.Apportioned = out.Apportioned || item.Apportioned
	}
	return out, nil
}

func (t Totals) add(o Totals) Totals {
	return Totals{t.Input + o.Input, t.Output + o.Output, t.CacheRead + o.CacheRead, t.CacheWrite + o.CacheWrite, t.Reasoning + o.Reasoning, t.CostUSD + o.CostUSD}
}
