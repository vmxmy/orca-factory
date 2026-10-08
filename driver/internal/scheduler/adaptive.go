package scheduler

import (
	"context"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
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
	return min(s.checkLinearQuota(now), s.checkGitHubQuota(now)), s.Pauses.Poll(ctx)
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
func (r *Runtime) GitHubLimits() []github.Usage {
	var limits []github.Usage
	for _, client := range r.Clients {
		if source, ok := client.(interface{ Usage() github.Usage }); ok {
			limits = append(limits, source.Usage())
		}
	}
	return limits
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
		} else if !s.discoveryQuotaPaused && (!s.quotaPaused || remaining >= s.resumeFraction()) {
			s.Pauses.Resume(key)
			s.quotaPaused = false
		}
	}
	return remaining
}
func (r *Runtime) LinearLimits() []linear.RateLimit {
	var limits []linear.RateLimit
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	usageView := status.Usage{Hour: now.UTC().Truncate(time.Hour), Limits: []status.ClientLimits{}}
	githubView := status.Usage{Hour: now.UTC().Truncate(time.Hour), Limits: []status.ClientLimits{}}
	seen := map[LinearClient]bool{}
	for repo, client := range r.Clients {
		if source, ok := client.(interface{ Usage() github.Usage }); ok {
			usage := source.Usage()
			if !seen[client] {
				githubView.Requests += usage.Requests
				seen[client] = true
			}
			githubView.Limits = append(githubView.Limits, status.ClientLimits{Repo: repo, Requests: status.RateLimit{Limit: usage.Limit, Remaining: usage.Remaining, Reset: usage.Reset}})
		}
		if source, ok := client.(interface{ Usage() linear.Usage }); ok {
			usage := source.Usage()
			if !seen[client] {
				usageView.Requests += usage.Requests
				usageView.Complexity += usage.Complexity
				seen[client] = true
			}
			convert := func(l linear.RateLimit) status.RateLimit {
				return status.RateLimit{Limit: l.Limit, Remaining: l.Remaining, Reset: l.Reset}
			}
			usageView.Limits = append(usageView.Limits, status.ClientLimits{Repo: repo, Requests: convert(usage.RequestLimit), Complexity: convert(usage.ComplexityLimit)})
			limits = append(limits, usage.RequestLimit, usage.ComplexityLimit)
		}
	}
	status.Push(now, func(view *status.View) { view.Linear = usageView; view.GitHub = githubView })
	return limits
}
