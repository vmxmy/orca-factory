package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

func startupScheduler(logs *bytes.Buffer, backend scheduler.Backend, now *time.Time, discover func(context.Context) (projects.Result, error)) *scheduler.Scheduler {
	return &scheduler.Scheduler{
		Backend:       backend,
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:           func() time.Time { return *now },
		Discover:      discover,
		DiscoverEvery: projectDiscoveryInterval,
	}
}

func fakeOrca(t *testing.T, envelope string) *orca.Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+envelope+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &orca.Client{Path: path}
}

func TestServeWaitsForOrcaAtStartup(t *testing.T) {
	t.Run("orca runtime_unavailable retries with bounded WARN volume", func(t *testing.T) {
		client := fakeOrca(t, `{"ok":false,"error":{"code":"runtime_unavailable"}}`)
		calls := 0
		backend := &statusFakeBackend{}
		var logs bytes.Buffer
		now := time.Date(2026, 10, 17, 21, 41, 0, 0, time.UTC)
		s := startupScheduler(&logs, backend, &now, func(ctx context.Context) (projects.Result, error) {
			calls++
			if calls <= 14 {
				_, err := client.Workers(ctx, orca.ListOptions{})
				return projects.Result{}, err
			}
			return projects.Result{Eligible: []projects.Project{{Path: "/repo", Repository: config.Repository{Path: "/repo"}}}}, nil
		})
		var slept []time.Duration
		sleep := func(_ context.Context, d time.Duration) error { slept = append(slept, d); now = now.Add(d); return nil }
		if err := s.AwaitProjects(context.Background(), sleep); err != nil {
			t.Fatal(err)
		}
		want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
		if len(slept) != 14 || !reflect.DeepEqual(slept[:6], want) || slept[13] != time.Minute {
			t.Fatalf("backoff = %v, want prefix %v capped at 1m", slept, want)
		}
		if len(s.Repositories) != 1 || s.Repositories[0].Path != "/repo" {
			t.Fatalf("repositories after recovery: %+v", s.Repositories)
		}
		if backend.snapshots != 0 {
			t.Fatalf("waiting read the ledger %d times", backend.snapshots)
		}
		var warns []string
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "project discovery waiting") {
				warns = append(warns, line)
			}
		}
		if len(warns) != 2 {
			t.Fatalf("want WARN at first failure and after 10m, got %d:\n%s", len(warns), logs.String())
		}
		if !strings.Contains(warns[0], "dependency=Orca") || !strings.Contains(warns[0], "reason=runtime_unavailable") {
			t.Fatalf("WARN must name dependency and reason: %s", warns[0])
		}
	})

	permanent := map[string]struct {
		discover func(*testing.T) func(context.Context) (projects.Result, error)
		reason   string
	}{
		"feishu authentication failure exits": {
			reason: "Feishu authentication_failed",
			discover: func(t *testing.T) func(context.Context) (projects.Result, error) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(`{"code":10014,"msg":"app secret invalid"}`))
				}))
				t.Cleanup(server.Close)
				client, err := feishu.New(feishu.Options{Endpoint: server.URL, AppID: "TEST-PLACEHOLDER-CRED", AppSecret: "TEST-PLACEHOLDER-CRED"})
				if err != nil {
					t.Fatal(err)
				}
				return func(ctx context.Context) (projects.Result, error) {
					_, err := client.Tables(ctx, "base")
					return projects.Result{}, err
				}
			},
		},
		"missing orca executable exits": {
			reason: "Orca executable_missing",
			discover: func(t *testing.T) func(context.Context) (projects.Result, error) {
				client := &orca.Client{Path: filepath.Join(t.TempDir(), "orca")}
				return func(ctx context.Context) (projects.Result, error) {
					_, err := client.Workers(ctx, orca.ListOptions{})
					return projects.Result{}, err
				}
			},
		},
	}
	for name, tc := range permanent {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 17, 21, 41, 0, 0, time.UTC)
			sleeps := 0
			s := startupScheduler(&bytes.Buffer{}, &statusFakeBackend{}, &now, tc.discover(t))
			err := s.AwaitProjects(context.Background(), func(context.Context, time.Duration) error { sleeps++; return nil })
			if err == nil || sleeps != 0 {
				t.Fatalf("permanent failure must not retry: err=%v sleeps=%d", err, sleeps)
			}
			if !strings.Contains(err.Error(), tc.reason) || strings.Contains(err.Error(), "TEST-PLACEHOLDER-CRED") {
				t.Fatalf("exit reason = %q, want %q without credentials", err, tc.reason)
			}
		})
	}

	t.Run("cancelled wait returns", func(t *testing.T) {
		client := fakeOrca(t, `{"ok":false,"error":{"code":"runtime_unavailable"}}`)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		now := time.Date(2026, 10, 17, 21, 41, 0, 0, time.UTC)
		s := startupScheduler(&bytes.Buffer{}, &statusFakeBackend{}, &now, func(ctx context.Context) (projects.Result, error) {
			_, err := client.Workers(context.Background(), orca.ListOptions{})
			return projects.Result{}, err
		})
		if err := s.AwaitProjects(ctx, sleepContext); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait returned %v", err)
		}
	})
}
