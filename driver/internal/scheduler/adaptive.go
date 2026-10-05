package scheduler

import (
	"context"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/pause"
	"time"
)

type Inspection struct {
	Interval      time.Duration
	FullReconcile bool
}
type Adaptive struct {
	interval    time.Duration
	lastFull    time.Time
	initialized bool
}

// Next returns the delay until the next inspection and a full reconciliation
// flag for the current one. Unknown quota is represented by a fraction of one.
func (a *Adaptive) Next(now time.Time, busy bool, remaining float64) Inspection {
	if busy || a.interval == 0 {
		a.interval = time.Minute
	} else {
		a.interval = min(3*time.Minute, a.interval+time.Minute)
	}
	interval := a.interval
	if remaining < .20 {
		interval = 3 * time.Minute
	}
	full := !a.initialized || now.Sub(a.lastFull) >= 10*time.Minute
	if full {
		a.lastFull = now
		a.initialized = true
	}
	return Inspection{Interval: interval, FullReconcile: full}
}
func (s *Scheduler) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func (s *Scheduler) plan(now time.Time, busy bool, remaining float64) {
	p := s.adaptive.Next(now, busy, remaining)
	s.NextInterval = p.Interval
	s.FullReconcile = p.FullReconcile
}

// LinearLimits is optional so existing backend implementations retain their
// contract. Both request and complexity quotas constrain all repositories.
type quotaSource interface{ LinearLimits() []linear.RateLimit }

func (s *Scheduler) checkPauses(ctx context.Context, now time.Time) (float64, error) {
	if s.Pauses == nil {
		s.Pauses = pause.New(s.clock, nil, nil)
	}
	remaining := 1.0
	var reset time.Time
	source, ok := s.Backend.(quotaSource)
	if ok {
		for _, limit := range source.LinearLimits() {
			if limit.Limit <= 0 || (!limit.Reset.IsZero() && !now.Before(limit.Reset)) {
				continue
			}
			fraction := float64(limit.Remaining) / float64(limit.Limit)
			remaining = min(remaining, fraction)
			if fraction < .05 && limit.Reset.After(reset) {
				reset = limit.Reset
			}
		}
		key := pause.Key{Dependency: pause.Linear, Kind: pause.Quota}
		if remaining < .05 {
			s.Pauses.Pause(key, reset)
		} else {
			s.Pauses.Resume(key)
		}
	}
	return remaining, s.Pauses.Poll(ctx)
}
func (r *Runtime) LinearLimits() []linear.RateLimit {
	var limits []linear.RateLimit
	for _, client := range r.Clients {
		if source, ok := client.(interface{ Usage() linear.Usage }); ok {
			usage := source.Usage()
			limits = append(limits, usage.RequestLimit, usage.ComplexityLimit)
		}
	}
	return limits
}
