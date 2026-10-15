package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

// This backend represents the external reads and writes of serve, without
// contacting Linear or starting an executor.
type serveQuotaBackend struct {
	scheduler.Backend
	limit               linear.RateLimit
	snapshots, launches int
	actions             []scheduler.Action
}

func (b *serveQuotaBackend) LinearLimits() []linear.RateLimit {
	return []linear.RateLimit{b.limit}
}

func (b *serveQuotaBackend) Snapshot(context.Context, []config.Repository) ([]scheduler.Ticket, error) {
	b.snapshots++
	return []scheduler.Ticket{{ID: "quota-candidate", Identifier: "QUOTA-1", Repo: "/quota-repo", Candidate: true}}, nil
}

func (b *serveQuotaBackend) AutomationDisabled(context.Context, config.Repository) (bool, error) {
	return true, nil
}

func (b *serveQuotaBackend) Apply(_ context.Context, _ scheduler.Ticket, action scheduler.Action) error {
	b.actions = append(b.actions, action)
	return nil
}

func (b *serveQuotaBackend) Launch(context.Context, scheduler.Ticket) error {
	b.launches++
	return nil
}

type serveQuotaProber struct {
	*serveQuotaBackend
	probes int
}

func (b *serveQuotaProber) ProbeLinearQuota(context.Context) error {
	b.probes++
	switch b.probes {
	case 1:
		return errors.New("quota probe unavailable")
	case 2:
		b.limit.Remaining = 8 // Above pause threshold, below resume threshold.
	default:
		b.limit.Remaining = 20
	}
	return nil
}

func TestServeQuotaProbeBackoffAndRecovery(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	now := start
	backend := &serveQuotaProber{serveQuotaBackend: &serveQuotaBackend{
		limit: linear.RateLimit{Limit: 100, Remaining: 1, Reset: start.Add(time.Hour)},
	}}
	s := &scheduler.Scheduler{
		Backend: backend, Now: func() time.Time { return now },
		Config: config.Config{
			MaxInFlight: 1, TokenBucket: config.TokenBucket{Capacity: 1, RefillPerSecond: 1},
			Tuning: config.Tuning{Scheduler: config.DefaultScheduler()},
		},
		Repositories: []config.Repository{{Path: "/quota-repo", MaxInFlight: 1}},
	}
	steps := []struct {
		minute int
		probes int
		paused bool
	}{
		{0, 0, true},
		{3, 0, true},
		{5, 1, true},  // Failure must not trigger a full Snapshot or dispatch.
		{8, 1, true},  // The failed probe must respect the retry backoff.
		{10, 2, true}, // Successful probe below resume threshold stays paused.
		{13, 2, true},
		{15, 3, false}, // Healthy quota permits a normal snapshot and claim.
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	step := 0
	err := serveWithStatus(ctx, filepath.Join(dir, "serve.lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
		return s, nil, nil
	}, func(context.Context) error {
		want := steps[step]
		if backend.probes != want.probes || s.Pauses.Paused("") != want.paused {
			t.Fatalf("minute %d: probes=%d paused=%v; want probes=%d paused=%v (snapshots=%d)", want.minute, backend.probes, s.Pauses.Paused(""), want.probes, want.paused, backend.snapshots)
		}
		if want.paused {
			if backend.snapshots != 0 || len(backend.actions) != 0 || backend.launches != 0 {
				t.Fatalf("minute %d: paused serve spent a full snapshot or dispatched: snapshots=%d actions=%v launches=%d", want.minute, backend.snapshots, backend.actions, backend.launches)
			}
		} else if backend.snapshots != 1 || len(backend.actions) != 1 || backend.actions[0].Kind != scheduler.Claim || backend.launches != 1 {
			t.Fatalf("recovered serve did not schedule normally: snapshots=%d actions=%v launches=%d", backend.snapshots, backend.actions, backend.launches)
		}
		step++
		if step == len(steps) {
			cancel()
		} else {
			now = start.Add(time.Duration(steps[step].minute) * time.Minute)
		}
		return nil
	}, func(err error) { t.Errorf("serve inspection: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	if step != len(steps) {
		t.Fatalf("serve stopped after %d of %d steps", step, len(steps))
	}
}

// This backend deliberately has no ProbeLinearQuota method: its shipped
// contract refreshes quota as part of a full snapshot.
type serveLegacyQuotaBackend struct{ *serveQuotaBackend }

func (b *serveLegacyQuotaBackend) Snapshot(ctx context.Context, repos []config.Repository) ([]scheduler.Ticket, error) {
	b.limit.Remaining = 20
	return b.serveQuotaBackend.Snapshot(ctx, repos)
}

func TestServeQuotaLegacySnapshotRecovery(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	backend := &serveLegacyQuotaBackend{&serveQuotaBackend{
		limit: linear.RateLimit{Limit: 100, Remaining: 1, Reset: now.Add(time.Hour)},
	}}
	s := &scheduler.Scheduler{
		Backend: backend, Now: func() time.Time { return now },
		Config: config.Config{
			MaxInFlight: 1, TokenBucket: config.TokenBucket{Capacity: 1, RefillPerSecond: 1},
			Tuning: config.Tuning{Scheduler: config.DefaultScheduler()},
		},
		Repositories: []config.Repository{{Path: "/quota-repo", MaxInFlight: 1}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	err := serveWithStatus(ctx, filepath.Join(dir, "serve.lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
		return s, nil, nil
	}, func(context.Context) error {
		waits++
		if waits == 1 {
			if !s.Pauses.Paused("") || backend.snapshots != 0 || backend.launches != 0 {
				t.Fatal("legacy backend must pause before spending its recovery snapshot")
			}
			now = now.Add(5 * time.Minute)
			return nil
		}
		if s.Pauses.Paused("") || backend.snapshots != 2 || len(backend.actions) != 1 || backend.actions[0].Kind != scheduler.Claim || backend.launches != 1 {
			t.Fatalf("legacy recovery must probe by snapshot then schedule: paused=%v snapshots=%d actions=%v launches=%d", s.Pauses.Paused(""), backend.snapshots, backend.actions, backend.launches)
		}
		cancel()
		return nil
	}, func(err error) { t.Errorf("serve inspection: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	if waits != 2 {
		t.Fatalf("serve stopped after %d waits", waits)
	}
}
