package scheduler

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/logx"
)

func (s *Scheduler) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return logx.New("scheduler", os.Stderr)
}

func (s *Scheduler) decision(t Ticket, event, reason string, attrs ...any) {
	fields := []any{"event", event, "ticket", t.Identifier, "ticket_id", t.ID, "repo", t.Repo}
	if reason != "" {
		fields = append(fields, "reason", reason)
	}
	fields = append(fields, attrs...)
	s.logger().Info("scheduler decision", fields...)
}

// Counts describe the input snapshot, not tentative ownership writes. A missing
// snapshot is explicit because a global pause must not trigger dependency reads.
func (s *Scheduler) summary(start time.Time, tickets []Ticket, snapshot bool, err error) {
	total, candidates := 0, 0
	for _, t := range tickets {
		if owned(t) {
			total++
		}
		if t.Candidate {
			candidates++
		}
	}
	limits := map[string]int{}
	pausedRepos := map[string]bool{}
	for _, r := range s.Config.Repositories {
		limits[r.Path] = r.MaxInFlight
		pausedRepos[r.Path] = s.Pauses != nil && s.Pauses.Paused(r.Path)
	}
	s.logger().Info("scheduler inspection", "event", "summary", "in_flight", total,
		"candidates", candidates, "snapshot_available", snapshot, "max_in_flight", s.Config.MaxInFlight,
		"repository_limits", limits, "paused", s.Pauses != nil && s.Pauses.Paused(""),
		"repository_paused", pausedRepos, "duration_ms", time.Since(start).Milliseconds(), "err", err)
}

func evidence(negative bool, err error) any {
	if err != nil {
		return nil
	}
	return !negative
}

func (s *Scheduler) ownership(t Ticket, absent, inactive bool, terminalErr, dispatchErr error) {
	result := "uncertain"
	if terminalErr == nil && !absent || dispatchErr == nil && !inactive {
		result = "alive"
	} else if terminalErr == nil && dispatchErr == nil {
		result = "dead"
	}
	s.decision(t, "ownership", "", "terminal", t.Terminal, "run", t.Run,
		"terminal_exists", evidence(absent, terminalErr), "active_dispatch", evidence(inactive, dispatchErr),
		"result", result, "terminal_error", terminalErr, "dispatch_error", dispatchErr)
}

func (s *Scheduler) launch(ctx context.Context, t Ticket) error {
	err := s.Backend.Launch(ctx, t)
	result := "success"
	if err != nil {
		result = "uncertain"
	}
	s.decision(t, "launch", "", "attempt", t.Attempt, "result", result, "err", err)
	return err
}
