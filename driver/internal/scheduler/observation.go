package scheduler

import (
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

// ObservationRow is the credential-free status view of one ticket. It explains
// why work cannot advance but never authorizes an action.
func ObservationRow(t Ticket, repos []config.Repository) status.Ticket {
	row := status.Ticket{Repo: t.Repo, Identifier: t.Identifier, Owner: string(t.Owner), Phase: string(t.Phase), URL: t.URL, Held: t.Held}
	for _, repo := range repos {
		if repo.Path == t.Repo {
			row.Backend, _ = issues.Backend(repo.Env)
		}
	}
	switch {
	case t.Held:
		row.BlockedReason = "import held"
	case t.MergeProblem != "":
		row.BlockedReason = t.MergeProblem
	case !t.Blocked:
	case t.BlockersIncomplete:
		row.BlockedReason = "dependency evidence incomplete"
	case len(t.Blockers) > 0:
		row.BlockedReason = "blocked by " + strings.Join(t.Blockers, ", ")
	}
	return row
}
