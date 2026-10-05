package main

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type releaseSource struct{ last time.Time }

func (s releaseSource) Plugins(context.Context) ([]string, error)         { return []string{"demo"}, nil }
func (s releaseSource) Env(context.Context, string) (string, error)       { return "30", nil }
func (s releaseSource) LatestTag(context.Context, string) (string, error) { return "demo-v1.0.0", nil }
func (s releaseSource) CommitsSince(context.Context, string, string) ([]release.Commit, error) {
	return []release.Commit{{Subject: "fix: corrected"}}, nil
}
func (s releaseSource) LastMerge(context.Context, string) (time.Time, error) { return s.last, nil }
func (s releaseSource) ReadFile(_ context.Context, path string) ([]byte, error) {
	if filepath.Base(path) == "plugin.json" {
		return []byte(`{"version":"1.0.0"}`), nil
	}
	return nil, fs.ErrNotExist
}

type releaseWriter struct{ plans []release.Plan }

func (w *releaseWriter) CreateReleasePR(_ context.Context, _ merge.Request, p release.Plan) (int, error) {
	w.plans = append(w.plans, p)
	return 42, nil
}

type releaseQueue struct{ requests []merge.Request }

func (q *releaseQueue) Enqueue(r merge.Request) error { q.requests = append(q.requests, r); return nil }
func TestServeWiresRelease(t *testing.T) {
	for _, elapsed := range []time.Duration{31 * time.Minute, 30 * time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
			now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
			backend := &statusFakeBackend{}
			s := &scheduler.Scheduler{Backend: backend, Now: func() time.Time { return now }, Config: config.Config{MaxInFlight: 4, TokenBucket: config.TokenBucket{Capacity: 4, RefillPerSecond: 1}}}
			w, q := &releaseWriter{}, &releaseQueue{}
			hook := &serveReleases{Scan: func(ctx context.Context) error {
				plans, err := release.Scan(ctx, releaseSource{now.Add(-elapsed)}, s.Now)
				if err != nil {
					return err
				}
				for _, p := range plans {
					if _, err := release.Submit(ctx, w, q, merge.Request{Repository: "owner/repo", Path: dir}, p); err != nil {
						return err
					}
				}
				return nil
			}}
			s.Merger = hook
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := serveWithStatus(ctx, filepath.Join(dir, "lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
				return s, &scheduler.Runtime{}, nil
			}, func(context.Context) error { cancel(); return nil }, func(err error) { t.Error(err) })
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if elapsed > 30*time.Minute {
				want = 1
			}
			if backend.snapshots != 1 || len(w.plans) != want || len(q.requests) != want {
				t.Fatalf("snapshots=%d plans=%v queue=%v", backend.snapshots, w.plans, q.requests)
			}
			if want == 1 && (w.plans[0].Version != "1.0.1" || q.requests[0].Number != 42) {
				t.Fatal(w.plans, q.requests)
			}
		})
	}
}
