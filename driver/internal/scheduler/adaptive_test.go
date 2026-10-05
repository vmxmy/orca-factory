package scheduler

import (
	"context"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/pause"
	"testing"
	"time"
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

type quotaBackend struct {
	*fakeBackend
	limits []linear.RateLimit
}

func (b *quotaBackend) LinearLimits() []linear.RateLimit { return b.limits }
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
	now = now.Add(8 * time.Hour)
	_ = s.Tick(context.Background())
	if len(f.actions) != 1 || f.actions[0].Kind != Human {
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
func TestPauseExpiredQuotaRequiresProbe(t *testing.T) {
	f := &fakeBackend{}
	s := setup(f)
	now := s.Now()
	s.Now = func() time.Time { return now }
	b := &quotaBackend{fakeBackend: f, limits: []linear.RateLimit{{Limit: 100, Remaining: 4, Reset: now.Add(time.Minute)}}}
	s.Backend = b
	_ = s.Tick(context.Background())
	now = now.Add(time.Minute)
	_ = s.Tick(context.Background())
	if !s.Pauses.Paused("a") || f.snapshots != 0 {
		t.Fatal("expired quota resumed without a successful probe or fresh budget")
	}
	b.limits[0].Remaining = 50
	_ = s.Tick(context.Background())
	if s.Pauses.Paused("a") || f.snapshots != 1 {
		t.Fatal("fresh quota did not recover")
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
