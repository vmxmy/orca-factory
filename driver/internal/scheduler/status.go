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
		status.SetTicket(view, status.Ticket{Repo: t.Repo, Identifier: t.Identifier, Owner: string(owner), Phase: string(a.Phase)}, owner == machine.OwnerExecutor || owner == machine.OwnerMerge || owner == machine.OwnerResolver)
	})
}
