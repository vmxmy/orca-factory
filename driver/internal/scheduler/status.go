package scheduler

import (
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func (s *Scheduler) pushTicket(t Ticket, a Action) {
	owner := machine.OwnerExecutor
	switch a.Kind {
	case Merge:
		owner = machine.OwnerMerge
	case Resolve:
		owner = machine.OwnerResolver
	case Release:
		owner = machine.OwnerNone
	}
	status.Push(s.clock(), func(view *status.View) {
		row := status.Ticket{Repo: t.Repo, Identifier: t.Identifier, Owner: string(owner), Phase: string(a.Phase)}
		if owner == machine.OwnerExecutor {
			row.ExecutionState = "unregistered"
			row.BlockedReason = status.ExecutorStartUnconfirmed
		}
		status.SetTicket(view, row, owner == machine.OwnerExecutor || owner == machine.OwnerMerge || owner == machine.OwnerResolver)
	})
}
