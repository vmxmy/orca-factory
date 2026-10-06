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
	interval = min(interval, a.lastFull.Add(10*time.Minute).Sub(now))
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
	if s.linearQuotaRetry.After(now) {
		s.NextInterval = min(s.NextInterval, s.linearQuotaRetry.Sub(now))
	}
	if s.Pauses != nil {
		if next := s.Pauses.NextProbe(); next.After(now) {
			s.NextInterval = min(s.NextInterval, next.Sub(now))
		}
	}
	s.FullReconcile = p.FullReconcile
}

// LinearLimits is optional so existing backend implementations retain their
// contract. Both request and complexity quotas constrain all repositories.
type quotaSource interface{ LinearLimits() []linear.RateLimit }

// quotaProber is optional: it spends one cheap request to refresh the quota headers.
type quotaProber interface{ ProbeLinearQuota(context.Context) error }

// Quota fractions now come from Config.Tuning.Scheduler (pause_fraction, resume_fraction).
func (s *Scheduler) pauseFraction() float64  { return s.Config.Tuning.Scheduler.PauseFraction }
func (s *Scheduler) resumeFraction() float64 { return s.Config.Tuning.Scheduler.ResumeFraction }
func (s *Scheduler) quotaProbeBackoff() time.Duration {
	return s.Config.Tuning.Scheduler.QuotaProbeBackoff
}

func (s *Scheduler) checkPauses(ctx context.Context, now time.Time) (float64, error) {
	if s.Pauses == nil {
		s.Pauses = pause.New(s.clock, nil, nil)
	}
	return s.checkLinearQuota(now), s.Pauses.Poll(ctx)
}

func (s *Scheduler) checkLinearQuota(now time.Time) float64 {
	remaining := 1.0
	var reset time.Time
	source, ok := s.Backend.(quotaSource)
	if ok {
		for _, limit := range source.LinearLimits() {
			if limit.Limit <= 0 {
				continue
			}
			// Paused ticks cannot refresh headers. Once reset is due, allow a
			// snapshot to fetch the new budget instead of reusing stale usage.
			if !limit.Reset.IsZero() && !now.Before(limit.Reset) {
				continue
			}
			fraction := float64(limit.Remaining) / float64(limit.Limit)
			if fraction < s.pauseFraction() && limit.Reset.IsZero() {
				if s.linearQuotaRetry.IsZero() {
					s.linearQuotaRetry = now.Add(s.quotaProbeBackoff())
				}
				// Keep the expired deadline until Snapshot actually refreshes
				// the headers, including when another dependency blocks Tick.
				if !now.Before(s.linearQuotaRetry) {
					continue
				}
				if s.linearQuotaRetry.After(reset) {
					reset = s.linearQuotaRetry
				}
			}
			remaining = min(remaining, fraction)
			if fraction < s.pauseFraction() && limit.Reset.After(reset) {
				reset = limit.Reset
			}
		}
		key := pause.Key{Dependency: pause.Linear, Kind: pause.Quota}
		if remaining < s.pauseFraction() {
			s.Pauses.Pause(key, reset)
			s.quotaPaused = true
		} else if !s.quotaPaused || remaining >= s.resumeFraction() {
			s.Pauses.Resume(key)
			s.quotaPaused = false
		}
	}
	return remaining
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
