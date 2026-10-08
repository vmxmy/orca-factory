package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/pause"
)

func TestAdaptiveIntervalPolicy(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var a Adaptive
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute, 3 * time.Minute} {
		p := a.Next(now, false, 1)
		if p.Interval != want {
			t.Fatalf("idle %d=%v", i, p.Interval)
		}
	}
	if p := a.Next(now, true, 1); p.Interval != time.Minute {
		t.Fatal(p)
	}
	if p := a.Next(now, true, .19); p.Interval <= time.Minute {
		t.Fatal("low quota not throttled")
	}
	if p := a.Next(now, true, .20); p.Interval != time.Minute {
		t.Fatal("20% throttled")
	}
	if p := a.Next(now.Add(10*time.Minute-time.Nanosecond), true, 1); p.FullReconcile {
		t.Fatal("early reconciliation")
	}
	if p := a.Next(now.Add(10*time.Minute), true, 1); !p.FullReconcile {
		t.Fatal("missing reconciliation")
	}
	if p := a.Next(now.Add(10*time.Minute), true, 1); p.FullReconcile {
		t.Fatal("duplicate reconciliation")
	}
}

type githubQuotaBackend struct {
	*fakeBackend
	usage github.Usage
}

func (b *githubQuotaBackend) GitHubLimits() []github.Usage { return []github.Usage{b.usage} }
func TestGitHubQuotaPausesUntilResetWithoutLinear(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{{ID: "a", Repo: "a", Candidate: true}}}
	s := setup(f)
	now := s.Now()
	s.Now = func() time.Time { return now }
	b := &githubQuotaBackend{fakeBackend: f, usage: github.Usage{Limit: 5000, Remaining: 1, Reset: now.Add(time.Hour)}}
	s.Backend = b
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 0 || !s.Pauses.Paused("") {
		t.Fatal("GitHub quota did not block dispatch")
	}
	now = now.Add(time.Hour)
	b.usage = github.Usage{Limit: 5000, Remaining: 4999, Reset: now.Add(time.Hour)}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 1 || s.Pauses.Paused("") {
		t.Fatal("GitHub quota did not recover after reset")
	}
}

type quotaBackend struct {
	*fakeBackend
	limits  []linear.RateLimit
	refresh func()
}

func (b *quotaBackend) LinearLimits() []linear.RateLimit { return b.limits }
func (b *quotaBackend) Snapshot(ctx context.Context, repos []config.Repository) ([]Ticket, error) {
	tickets, err := b.fakeBackend.Snapshot(ctx, repos)
	if b.refresh != nil {
		b.refresh()
	}
	return tickets, err
}
func TestPauseSchedulerScopeAndStale(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{{ID: "a", Repo: "a", Owner: machine.OwnerExecutor, UpdatedAt: time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)}, {ID: "b", Repo: "b", Candidate: true}}}
	s := setup(f)
	now := s.Now()
	s.Now = func() time.Time { return now }
	s.Pauses = pause.New(s.Now, nil, nil)
	key := pause.Key{Dependency: pause.Orca, Repo: "a"}
	s.Pauses.Pause(key, time.Time{})
	now = now.Add(9 * time.Hour)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 1 || f.launched[0].ID != "b" || len(f.actions) != 1 {
		t.Fatalf("paused actions=%v launches=%v", f.actions, f.launched)
	}
	s.Pauses.Resume(key)
	f.actions = nil
	f.tickets = f.tickets[:1]
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 0 {
		t.Fatal("pause time counted as stale", f.actions)
	}
	f.absent, f.inactive = true, true
	now = now.Add(8 * time.Hour)
	_ = s.Tick(context.Background())
	if len(f.actions) != 1 || f.actions[0].Kind != Resolve {
		t.Fatal("effective stale not enforced", f.actions)
	}
}
func TestPauseLinearQuotaThreshold(t *testing.T) {
	for _, remaining := range []int64{4, 5, 19, 20} {
		t.Run(time.Duration(remaining).String(), func(t *testing.T) {
			f := &fakeBackend{tickets: []Ticket{{ID: "a", Repo: "a", Candidate: true}}}
			s := setup(f)
			b := &quotaBackend{fakeBackend: f, limits: []linear.RateLimit{{Limit: 100, Remaining: remaining, Reset: s.Now().Add(time.Hour)}}}
			s.Backend = b
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if remaining < 5 {
				if !s.Pauses.Paused("b") || len(f.launched) != 0 || f.snapshots != 0 {
					t.Fatal("critical quota did not stop all work")
				}
			} else if len(f.launched) != 1 {
				t.Fatal("noncritical quota blocked dispatch")
			}
			if remaining < 20 && s.NextInterval <= time.Minute {
				t.Fatal("quota did not slow polling")
			}
		})
	}
}

func TestAdaptiveIntervalServeHonorsPlan(t *testing.T) {
	f := &fakeBackend{}
	s := setup(f)
	now := s.Now()
	s.Now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	err := Serve(ctx, t.TempDir()+"/serve.lock", func(context.Context) (*Scheduler, error) { return s, nil }, func(context.Context) error {
		waits++
		now = now.Add(30 * time.Second)
		if waits == 6 {
			cancel()
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.snapshots != 2 {
		t.Fatalf("snapshots=%d, want polls at 0 and 60 seconds", f.snapshots)
	}
}
func TestPauseSchedulerCredentialsBeforeDispatch(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{{ID: "a", Repo: "a", Candidate: true}}}
	s := setup(f)
	s.Pauses = pause.New(s.Now, denyCredentials{}, nil)
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("missing credential error")
	}
	if f.snapshots != 0 || len(f.launched) != 0 {
		t.Fatal("dispatch proceeded without credentials")
	}
}

type denyCredentials struct{}

func (denyCredentials) Check(context.Context, pause.Key) error { return context.DeadlineExceeded }

func TestAdaptiveIntervalReconcileDeadline(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var a Adaptive
	for _, minute := range []int{0, 1, 3, 6} {
		a.Next(now.Add(time.Duration(minute)*time.Minute), false, 1)
	}
	p := a.Next(now.Add(9*time.Minute), false, 1)
	if p.Interval != time.Minute {
		t.Fatalf("next=%v would overshoot ten-minute reconciliation", p.Interval)
	}
}
func TestPauseExpiredQuotaResumesSnapshot(t *testing.T) {
	for _, afterReset := range []time.Duration{0, time.Nanosecond} {
		t.Run(afterReset.String(), func(t *testing.T) {
			f := &fakeBackend{}
			s := setup(f)
			now := s.Now()
			s.Now = func() time.Time { return now }
			reset := now.Add(time.Minute)
			b := &quotaBackend{fakeBackend: f, limits: []linear.RateLimit{{Limit: 100, Remaining: 4, Reset: reset}}}
			s.Backend = b
			for _, beforeReset := range []time.Time{now, reset.Add(-time.Nanosecond)} {
				now = beforeReset
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if !s.Pauses.Paused("") || f.snapshots != 0 {
					t.Fatal("critical quota did not pause snapshots before reset")
				}
			}
			now = reset.Add(afterReset)
			// No response arrived during the pause: the low Remaining stays unchanged.
			for wantSnapshots := 1; wantSnapshots <= 2; wantSnapshots++ {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if s.Pauses.Paused("") || f.snapshots != wantSnapshots {
					t.Fatalf("expired quota blocked refresh: paused=%v snapshots=%d, want %d", s.Pauses.Paused(""), f.snapshots, wantSnapshots)
				}
				now = now.Add(time.Minute)
			}
		})
	}
}

func TestPauseQuotaResetRespectsOtherLimits(t *testing.T) {
	for _, knownReset := range []bool{false, true} {
		name := "unknown reset"
		if knownReset {
			name = "later reset"
		}
		t.Run(name, func(t *testing.T) {
			f := &fakeBackend{}
			s := setup(f)
			now := s.Now()
			s.Now = func() time.Time { return now }
			var otherReset time.Time
			if knownReset {
				otherReset = now.Add(time.Hour)
			}
			s.Backend = &quotaBackend{fakeBackend: f, limits: []linear.RateLimit{
				{Limit: 100, Remaining: 4, Reset: now.Add(time.Minute)},
				{Limit: 100, Remaining: 4, Reset: otherReset},
			}}
			start := now
			steps := []struct {
				elapsed   time.Duration
				snapshots int
			}{{0, 0}, {2 * time.Minute, 0}, {5*time.Minute - time.Nanosecond, 0}, {5 * time.Minute, 1}, {5 * time.Minute, 1}, {10*time.Minute - time.Nanosecond, 1}, {10 * time.Minute, 2}}
			if knownReset {
				// Even with a known reset time the pause must still probe after the
				// backoff; a known reset alone never unlatches a paused quota, and the
				// paused branch otherwise never reaches Snapshot to observe recovery.
				steps = []struct {
					elapsed   time.Duration
					snapshots int
				}{{0, 0}, {2 * time.Minute, 0}, {5 * time.Minute, 1}, {10 * time.Minute, 2}}
			}
			for _, step := range steps {
				now = start.Add(step.elapsed)
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if !s.Pauses.Paused("") || f.snapshots != step.snapshots {
					t.Fatalf("elapsed=%v paused=%v snapshots=%d, want paused with %d snapshots", step.elapsed, s.Pauses.Paused(""), f.snapshots, step.snapshots)
				}
			}
		})
	}
}

func TestPauseUnknownQuotaRefreshesHeaders(t *testing.T) {
	for _, healthy := range []bool{false, true} {
		name := "new reset"
		if healthy {
			name = "healthy quota"
		}
		t.Run(name, func(t *testing.T) {
			f := &fakeBackend{tickets: []Ticket{{ID: "a", Repo: "a", Candidate: true}}}
			s := setup(f)
			now := s.Now()
			s.Now = func() time.Time { return now }
			start := now
			b := &quotaBackend{fakeBackend: f, limits: []linear.RateLimit{{Limit: 100, Remaining: 4}}}
			b.refresh = func() {
				if healthy {
					b.limits[0].Remaining = 100
				} else {
					b.limits[0].Reset = start.Add(20 * time.Minute)
				}
			}
			s.Backend = b
			for _, elapsed := range []time.Duration{0, 3 * time.Minute, 5 * time.Minute} {
				now = start.Add(elapsed)
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if elapsed == 3*time.Minute && s.NextInterval != 2*time.Minute {
					t.Fatalf("next=%v misses five-minute quota refresh", s.NextInterval)
				}
			}
			if f.snapshots != 1 || s.Pauses.Paused("") == healthy {
				t.Fatalf("refreshed headers ignored: snapshots=%d paused=%v", f.snapshots, s.Pauses.Paused(""))
			}
			if healthy {
				if len(f.launched) != 1 {
					t.Fatal("healthy response did not restore dispatch")
				}
				return
			}
			if len(f.launched) != 0 {
				t.Fatal("critical response allowed dispatch")
			}
			b.refresh = nil
			now = start.Add(10 * time.Minute)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.snapshots != 1 {
				t.Fatal("new known reset did not defer refresh")
			}
			now = start.Add(20 * time.Minute)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.snapshots != 2 {
				t.Fatal("new known reset did not restore snapshot")
			}
		})
	}
}

func TestPauseProbeDeadlineCapsIdleBackoff(t *testing.T) {
	f := &fakeBackend{}
	s := setup(f)
	now := s.Now()
	s.Now = func() time.Time { return now }
	s.Pauses = pause.New(s.Now, denyCredentials{}, nil)
	_ = s.Tick(context.Background())
	now = now.Add(time.Minute)
	_ = s.Tick(context.Background())
	now = now.Add(2 * time.Minute)
	_ = s.Tick(context.Background())
	if s.NextInterval != 2*time.Minute {
		t.Fatalf("next=%v misses five-minute probe", s.NextInterval)
	}
}
