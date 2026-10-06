package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type releaseServeSource struct {
	now, last time.Time
	opened    int
}

func (b *releaseServeSource) Plugins(context.Context) ([]string, error) {
	return []string{"demo"}, nil
}
func (b *releaseServeSource) Env(context.Context, string) (string, error) { return "30", nil }
func (b *releaseServeSource) LatestTag(context.Context, string) (string, error) {
	return "demo-v1.0.0", nil
}
func (b *releaseServeSource) CommitsSince(context.Context, string, string) ([]release.Commit, error) {
	return []release.Commit{{Subject: "fix: bug"}}, nil
}
func (b *releaseServeSource) LastMerge(context.Context, string) (time.Time, error) {
	return b.last, nil
}
func (b *releaseServeSource) ReadFile(_ context.Context, p string) ([]byte, error) {
	if filepath.Ext(p) == ".json" {
		return []byte(`{"version":"1.0.0"}`), nil
	}
	return nil, nil
}
func (b *releaseServeSource) CreateReleasePR(context.Context, merge.Request, release.Plan) (int, error) {
	b.opened++
	return 7, nil
}

type releaseServeLinear struct{ serveMergeLinear }

func (l *releaseServeLinear) Snapshot(context.Context, []string, time.Time) ([]linear.Issue, error) {
	return nil, nil
}
func TestServeWiresRelease(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(map[bool]string{true: "quiet_elapsed", false: "still_quiet"}[quiet], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Now()
			b := &releaseServeSource{now: now, last: now.Add(-time.Minute)}
			if quiet {
				b.last = now.Add(-31 * time.Minute)
			}
			dir := t.TempDir()
			t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
			repo := config.Repository{Path: dir, MaxInFlight: 1, StaleHours: 8, Env: map[string]string{"AGENT_GH_REPO": "o/r", "AGENT_LINEAR_PROJECT": "p"}}
			cfg := config.Config{MaxInFlight: 1, Repositories: []config.Repository{repo}, TokenBucket: config.TokenBucket{Capacity: 1, RefillPerSecond: 1}}
			client := &releaseServeLinear{}
			manager := &release.Manager{File: filepath.Join(dir, "releases.json"), Request: merge.Request{Repository: "o/r", Path: dir}, Source: func(context.Context) (release.Source, error) { return b, nil }, Writer: b, Now: func() time.Time { return now }}
			runtime := &scheduler.Runtime{Reader: client, Clients: map[string]scheduler.LinearClient{dir: client}, Repositories: map[string]config.Repository{dir: repo}, Orca: &serveMergeOrca{}, Releases: map[string]*release.Manager{dir: manager}}
			err := serveWithStatus(ctx, filepath.Join(dir, "lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
				return &scheduler.Scheduler{Backend: runtime, Config: cfg}, runtime, nil
			}, func(context.Context) error { cancel(); return ctx.Err() }, func(e error) { t.Error(e) })
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if quiet {
				want = 1
			}
			head, err := manager.Head()
			if err != nil {
				t.Fatal(err)
			}
			if b.opened != want || head.IsZero() == quiet {
				t.Fatalf("release PRs=%d queued at=%v want=%d", b.opened, head, want)
			}
		})
	}
}
