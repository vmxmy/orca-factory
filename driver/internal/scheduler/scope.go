package scheduler

import (
	"context"
	"slices"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// actualChangeReader distinguishes an empty branch diff from absent branches.
// A failed or unavailable read must retain the ticket's declared scope.
type actualChangeReader interface {
	ActualChanges(context.Context, Ticket) ([]string, bool, error)
}

func (s *Scheduler) scopeConflicts(ctx context.Context, tickets []Ticket, repos map[string]config.Repository) map[string][]string {
	conflicts := map[string][]string{}
	reader, canRead := s.Backend.(actualChangeReader)
	for _, t := range tickets {
		if (!owned(t) && !t.Legacy) || repos[t.Repo].Env["AGENT_SCOPE_AVOID"] == "0" {
			continue
		}
		scope := t.Scope
		if canRead {
			if files, found, err := reader.ActualChanges(ctx, t); err == nil && found {
				scope = files
			}
		}
		conflicts[t.Repo] = append(conflicts[t.Repo], avoidedScope(scope, repos[t.Repo])...)
	}
	return conflicts
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

func scopesOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y || strings.HasPrefix(x, strings.TrimSuffix(y, "/")+"/") || strings.HasPrefix(y, strings.TrimSuffix(x, "/")+"/") || task.ScopeMatch(x, y) || task.ScopeMatch(y, x) {
				return true
			}
		}
	}
	return false
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
