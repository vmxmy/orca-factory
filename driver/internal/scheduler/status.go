package scheduler

import (
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func (s *Scheduler) pushStatus() {
	now := s.clock()
	status.Push(now, func(view *status.View) {
		view.GlobalPause = status.GlobalPause{}
		if s.Pauses != nil {
			for _, p := range s.Pauses.Active() {
				if p.Key.Repo == "" {
					view.GlobalPause.Active = true
					view.GlobalPause.DurationSeconds = max(view.GlobalPause.DurationSeconds, int64(now.Sub(p.Start).Seconds()))
				}
			}
		}
	})
}

func (s *Scheduler) pushTicket(t Ticket, a Action) {
	owner := machine.OwnerExecutor
	switch a.Kind {
	case Merge:
		owner = machine.OwnerMerge
	case Human:
		owner = machine.OwnerHuman
	case Release:
		owner = machine.OwnerNone
	}
	status.Push(s.clock(), func(view *status.View) {
		status.SetTicket(view, status.Ticket{Repo: t.Repo, Identifier: t.Identifier, Owner: string(owner), Phase: string(a.Phase)}, owner == machine.OwnerExecutor || owner == machine.OwnerMerge)
	})
}
