package scheduler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// actualChangeReader distinguishes an empty branch diff from absent branches.
// A failed or unavailable read must retain the ticket's declared scope.
type actualChangeReader interface {
	ActualChanges(context.Context, Ticket) ([]string, bool, error)
}

type scopeReservation struct {
	identifier string
	scope      []string
	source     string
}

func (s *Scheduler) scopeConflicts(ctx context.Context, tickets []Ticket, repos map[string]config.Repository) map[string][]scopeReservation {
	reader, _ := s.Backend.(actualChangeReader)
	return scopeReservations(ctx, tickets, repos, reader)
}

func scopeReservations(ctx context.Context, tickets []Ticket, repos map[string]config.Repository, reader actualChangeReader) map[string][]scopeReservation {
	reservations := map[string][]scopeReservation{}
	for _, ticket := range tickets {
		if (!owned(ticket) && !ticket.Legacy) || repos[ticket.Repo].Env["AGENT_SCOPE_AVOID"] == "0" {
			continue
		}
		scope, source := ticket.Scope, "declared"
		if reader != nil {
			if files, found, err := reader.ActualChanges(ctx, ticket); err == nil && found {
				scope, source = files, "actual"
			}
		}
		reservations[ticket.Repo] = append(reservations[ticket.Repo], scopeReservation{identifier: ticket.Identifier, scope: avoidedScope(scope, repos[ticket.Repo]), source: source})
	}
	return reservations
}

// ObserveScopeWaits reads the same owned-ticket reservations as Tick. It does
// not predict reservations made by candidates claimed later in that tick.
func ObserveScopeWaits(ctx context.Context, tickets []Ticket, repos []config.Repository, reader actualChangeReader) {
	byPath := make(map[string]config.Repository, len(repos))
	for _, repo := range repos {
		byPath[repo.Path] = repo
	}
	observeScopeWaits(tickets, byPath, scopeReservations(ctx, tickets, byPath, reader))
}

func observeScopeWaits(tickets []Ticket, repos map[string]config.Repository, reservations map[string][]scopeReservation) {
	for index := range tickets {
		ticket := &tickets[index]
		ticket.ScopeWaits = nil
		if !ticket.Candidate || owned(*ticket) || repos[ticket.Repo].Env["AGENT_SCOPE_AVOID"] == "0" {
			continue
		}
		ticket.ScopeWaits = scopeMatches(avoidedScope(ticket.Scope, repos[ticket.Repo]), reservations[ticket.Repo])
	}
}

func scopeMatches(candidate []string, reservations []scopeReservation) []status.ScopeWait {
	var matches []status.ScopeWait
	for _, reservation := range reservations {
		for _, path := range candidate {
			for _, occupied := range reservation.scope {
				if rule := scopeOverlapRule(path, occupied); rule != "" {
					match := status.ScopeWait{Identifier: reservation.identifier, CandidatePath: path, OccupiedPath: occupied, Rule: rule, Source: reservation.source}
					if !slices.Contains(matches, match) {
						matches = append(matches, match)
					}
				}
			}
		}
	}
	return matches
}

func scopeWaitReason(matches []status.ScopeWait) string {
	reasons := make([]string, 0, len(matches))
	for _, match := range matches {
		reasons = append(reasons, fmt.Sprintf("%s candidate=%s occupied=%s rule=%s source=%s", match.Identifier, match.CandidatePath, match.OccupiedPath, match.Rule, match.Source))
	}
	return "scope wait: " + strings.Join(reasons, "; ")
}

// avoidedScope drops entries wholly covered by AGENT_SCOPE_SHARED. Concurrent
// edits to those files reach the merge lane, which turns a text conflict into
// a correction. An entry only partly covered, such as a parent directory, stays.
func avoidedScope(scope []string, repo config.Repository) []string {
	shared := strings.Fields(repo.Env["AGENT_SCOPE_SHARED"])
	kept := []string{}
	for _, entry := range scope {
		if !slices.ContainsFunc(shared, func(pattern string) bool {
			return entry == pattern || strings.HasPrefix(entry, strings.TrimSuffix(pattern, "/")+"/") || task.ScopeMatch(pattern, entry)
		}) {
			kept = append(kept, entry)
		}
	}
	return kept
}

func scopeOverlapRule(candidate, occupied string) string {
	switch {
	case candidate == occupied:
		return "equal"
	case strings.HasPrefix(candidate, strings.TrimSuffix(occupied, "/")+"/"):
		return "occupied-directory"
	case strings.HasPrefix(occupied, strings.TrimSuffix(candidate, "/")+"/"):
		return "candidate-directory"
	case task.ScopeMatch(candidate, occupied):
		return "candidate-pattern"
	case task.ScopeMatch(occupied, candidate):
		return "occupied-pattern"
	default:
		return ""
	}
}

func declaredScope(body string) []string {
	found := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if found && line != "" {
			if strings.HasPrefix(line, "#") {
				return nil
			}
			return strings.Fields(line)
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "##" && fields[1] == "SCOPE" {
			found = true
		}
	}
	return nil
}
