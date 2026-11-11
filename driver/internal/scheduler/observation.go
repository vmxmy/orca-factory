package scheduler

import (
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

// ObservationRow is the credential-free status view of one ticket. It explains
// why work cannot advance but never authorizes an action.
func ObservationRow(t Ticket, repos []config.Repository, now time.Time) status.Ticket {
	row := status.Ticket{Repo: t.Repo, Identifier: t.Identifier, Owner: string(t.Owner), Phase: string(t.Phase), URL: t.URL, Held: t.Held, Title: t.Title}
	switch {
	case owned(t):
	case t.Candidate:
		row.Stage = "candidate"
	case t.Owner == machine.OwnerHuman:
		row.Stage = "human"
		row.HumanKind = "takeover"
		if len(notificationScope(t)) > 0 {
			row.HumanKind = "scope"
		} else if p := t.Progress; p != nil && p.MergeSHA != "" && p.Deployment != nil && p.Deployment.State != "passed" {
			row.HumanKind = "deploy"
		}
		_, reason := feishuNotification(t)
		runes := []rune(strings.Join(strings.Fields(reason), " "))
		row.HumanReason = string(runes[:min(len(runes), 120)])
	}
	row.Stages = stageDurations(t, now)
	if p := t.Progress; p != nil && p.Deployment != nil {
		row.Deployment = &status.Deployment{QueueID: p.QueueID, SHA: p.MergeSHA, Workflow: p.Deployment.Workflow, State: p.Deployment.State, Reason: p.Deployment.Reason}
	}
	if h := t.Handoff; h != nil {
		count := h.Corrections
		if p := t.Progress; p != nil && p.QueueID == h.QueueID {
			count = max(count, p.Corrections)
		}
		// readMergeHandoff does not validate these fields; a malformed value is no evidence.
		if h.Lane >= 1 && h.Lane <= 3 {
			row.Lane = h.Lane
		}
		if count >= 0 {
			row.Corrections = &status.Corrections{Count: count, Limit: machine.DefaultLimits.Corrections}
		}
	}
	for _, repo := range repos {
		if repo.Path == t.Repo {
			row.Backend, _ = issues.Backend(repo.Env)
		}
	}
	switch {
	case t.Held:
		row.BlockedReason = "import held"
	case t.Parked != "":
		row.BlockedReason = "parked: " + t.Parked
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

func ObservationRows(tickets []Ticket, repos []config.Repository, now time.Time) []status.Ticket {
	rows := make([]status.Ticket, len(tickets))
	queues := map[string][]int{}
	for index, ticket := range tickets {
		rows[index] = ObservationRow(ticket, repos, now)
		if ticket.Phase == machine.PhaseMergeQueue && ticket.Handoff != nil && ticket.MergeProblem == "" {
			queues[ticket.Repo] = append(queues[ticket.Repo], index)
		}
	}
	for _, queue := range queues {
		sort.Slice(queue, func(first, second int) bool {
			left, right := tickets[queue[first]].Handoff, tickets[queue[second]].Handoff
			if left.EnqueuedAt.Equal(right.EnqueuedAt) {
				return left.QueueID < right.QueueID
			}
			return left.EnqueuedAt.Before(right.EnqueuedAt)
		})
		for position, index := range queue {
			rows[index].QueuePosition = position + 1
		}
	}
	return rows
}

func SnapshotRows(tickets []Ticket, repos []config.Repository, now time.Time) []status.Ticket {
	snapshot := make([]status.Ticket, 0, len(tickets))
	rows := ObservationRows(tickets, repos, now)
	for index, ticket := range tickets {
		if ticket.Owner == machine.OwnerExecutor || ticket.Owner == machine.OwnerMerge || rows[index].Stage == "human" || ticket.Parked != "" || DeploymentBarrier(ticket) {
			snapshot = append(snapshot, rows[index])
		}
	}
	sort.Slice(snapshot, func(i, j int) bool {
		a, b := snapshot[i], snapshot[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Identifier < b.Identifier
	})
	return snapshot
}
