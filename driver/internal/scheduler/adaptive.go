package scheduler

import (
	"context"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/pause"
	"github.com/vmxmy/orca-factory/driver/internal/status"
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
	if s.quotaRetry.After(now) {
		s.NextInterval = min(s.NextInterval, s.quotaRetry.Sub(now))
	}
	if s.Pauses != nil {
		if next := s.Pauses.NextProbe(); next.After(now) {
			s.NextInterval = min(s.NextInterval, next.Sub(now))
		}
	}
	s.FullReconcile = p.FullReconcile
}

// Quota fractions now come from Config.Tuning.Scheduler (pause_fraction).
func (s *Scheduler) pauseFraction() float64 { return s.Config.Tuning.Scheduler.PauseFraction }
func (s *Scheduler) quotaProbeBackoff() time.Duration {
	return s.Config.Tuning.Scheduler.QuotaProbeBackoff
}

func (s *Scheduler) checkPauses(ctx context.Context, now time.Time) (float64, error) {
	if s.Pauses == nil {
		s.Pauses = pause.New(s.clock, nil, nil)
	}
	return s.checkGitHubQuota(now), s.Pauses.Poll(ctx)
}

type githubQuotaSource interface{ GitHubLimits() []github.Usage }

func (s *Scheduler) checkGitHubQuota(now time.Time) float64 {
	source, ok := s.Backend.(githubQuotaSource)
	if !ok {
		return 1
	}
	remaining := 1.0
	var reset time.Time
	for _, usage := range source.GitHubLimits() {
		if usage.Limit <= 0 || !usage.Reset.IsZero() && !now.Before(usage.Reset) {
			continue
		}
		fraction := float64(usage.Remaining) / float64(usage.Limit)
		if fraction < s.pauseFraction() {
			deadline := usage.Reset
			if deadline.IsZero() {
				if s.githubQuotaRetry.IsZero() {
					s.githubQuotaRetry = now.Add(s.quotaProbeBackoff())
				}
				if !now.Before(s.githubQuotaRetry) {
					continue
				}
				deadline = s.githubQuotaRetry
			}
			if deadline.After(reset) {
				reset = deadline
			}
		}
		remaining = min(remaining, fraction)
	}
	key := pause.Key{Dependency: pause.GitHub, Kind: pause.Quota}
	if remaining < s.pauseFraction() {
		s.Pauses.Pause(key, reset)
	} else {
		s.Pauses.Resume(key)
	}
	return remaining
}

// GitHubLimits also publishes the per-repository GitHub usage observation.
func (r *Runtime) GitHubLimits() []github.Usage {
	var limits []github.Usage
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	view := status.Usage{Hour: now.UTC().Truncate(time.Hour), Limits: []status.ClientLimits{}}
	seen := map[IssueClient]bool{}
	for repo, client := range r.Clients {
		source, ok := client.(interface{ Usage() github.Usage })
		if !ok {
			continue
		}
		usage := source.Usage()
		limits = append(limits, usage)
		if !seen[client] {
			view.Requests += usage.Requests
			seen[client] = true
		}
		view.Limits = append(view.Limits, status.ClientLimits{Repo: repo, Requests: status.RateLimit{Limit: usage.Limit, Remaining: usage.Remaining, Reset: usage.Reset}})
	}
	status.Push(now, func(v *status.View) { v.GitHub = view })
	return limits
}
