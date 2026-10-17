package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

func startupScheduler(logs *bytes.Buffer, backend scheduler.Backend, discover func(context.Context) (projects.Result, error)) *scheduler.Scheduler {
	now := time.Date(2026, 10, 17, 21, 41, 0, 0, time.UTC)
	return &scheduler.Scheduler{
		Backend:       backend,
		Logger:        slog.New(slog.NewTextHandler(logs, nil)),
		Now:           func() time.Time { return now },
		Discover:      discover,
		DiscoverEvery: projectDiscoveryInterval,
	}
}

func TestServeWaitsForOrcaAtStartup(t *testing.T) {
	orcaDown := &infra.Global{Dep: infra.Orca, Err: errors.New("orca dependency unavailable")}
	calls := 0
	backend := &statusFakeBackend{}
	var logs bytes.Buffer
	s := startupScheduler(&logs, backend, func(context.Context) (projects.Result, error) {
		calls++
		if calls <= 6 {
			return projects.Result{}, orcaDown
		}
		return projects.Result{Eligible: []projects.Project{{Path: "/repo", Repository: config.Repository{Path: "/repo"}}}}, nil
	})
	var slept []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	if err := s.AwaitProjects(context.Background(), sleep); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	if !reflect.DeepEqual(slept, want) {
		t.Fatalf("backoff = %v, want %v", slept, want)
	}
	if len(s.Repositories) != 1 || s.Repositories[0].Path != "/repo" {
		t.Fatalf("repositories after recovery: %+v", s.Repositories)
	}
	if backend.snapshots != 0 {
		t.Fatalf("waiting read the ledger %d times", backend.snapshots)
	}
	if got := strings.Count(logs.String(), "level=WARN"); got != 6 {
		t.Fatalf("want one WARN per retry, got %d:\n%s", got, logs.String())
	}

	configErr := errors.New("projects: feishu backend expected")
	sleeps := 0
	s = startupScheduler(&bytes.Buffer{}, backend, func(context.Context) (projects.Result, error) { return projects.Result{}, configErr })
	err := s.AwaitProjects(context.Background(), func(context.Context, time.Duration) error { sleeps++; return nil })
	if !errors.Is(err, configErr) || sleeps != 0 {
		t.Fatalf("unrecoverable error must not retry: err=%v sleeps=%d", err, sleeps)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s = startupScheduler(&bytes.Buffer{}, backend, func(context.Context) (projects.Result, error) { return projects.Result{}, orcaDown })
	if err := s.AwaitProjects(ctx, sleepContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait returned %v", err)
	}
}
