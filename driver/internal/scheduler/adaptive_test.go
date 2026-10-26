package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

func TestAdaptiveIntervalPolicy(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var a Adaptive
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute, 3 * time.Minute} {
		p := a.Next(now, false)
		if p.Interval != want {
			t.Fatalf("idle %d=%v", i, p.Interval)
		}
	}
	if p := a.Next(now, true); p.Interval != time.Minute {
		t.Fatal(p)
	}
	if p := a.Next(now.Add(10*time.Minute-time.Nanosecond), true); p.FullReconcile {
		t.Fatal("early reconciliation")
	}
	if p := a.Next(now.Add(10*time.Minute), true); !p.FullReconcile {
		t.Fatal("missing reconciliation")
	}
	if p := a.Next(now.Add(10*time.Minute), true); p.FullReconcile {
		t.Fatal("duplicate reconciliation")
	}
}

func TestSchedulerStaleInFlightResolves(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{{ID: "a", Repo: "a", Owner: machine.OwnerExecutor, UpdatedAt: time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)}}, absent: true, inactive: true}
	s := setup(f)
	now := s.Now().Add(7*time.Hour - time.Minute)
	s.Now = func() time.Time { return now }
	_ = s.Tick(context.Background())
	for _, a := range f.actions {
		if a.Kind == Resolve {
			t.Fatal("resolved before the stale limit", f.actions)
		}
	}
	f.actions = nil
	now = now.Add(2 * time.Minute)
	_ = s.Tick(context.Background())
	if len(f.actions) != 1 || f.actions[0].Kind != Resolve {
		t.Fatal("stale limit not enforced", f.actions)
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
func TestAdaptiveIntervalReconcileDeadline(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var a Adaptive
	for _, minute := range []int{0, 1, 3, 6} {
		a.Next(now.Add(time.Duration(minute)*time.Minute), false)
	}
	p := a.Next(now.Add(9*time.Minute), false)
	if p.Interval != time.Minute {
		t.Fatalf("next=%v would overshoot ten-minute reconciliation", p.Interval)
	}
}
