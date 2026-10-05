package main

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/mergelane"
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

type releaseBackend struct{ statusFakeBackend }

func (b *releaseBackend) AutomationDisabled(context.Context, config.Repository) (bool, error) {
	return true, nil
}

type releaseGitHub struct{ mergelane.GitHub }

func (releaseGitHub) GetPR(context.Context, string, string, int) (github.PullRequest, error) {
	pr := github.PullRequest{Number: 42, State: "open", MergeableState: "clean"}
	pr.Head.SHA = "head"
	pr.Base.Ref = "main"
	return pr, nil
}
func (releaseGitHub) BranchSHA(context.Context, string, string, string) (string, error) {
	return "main", nil
}
func (releaseGitHub) Compare(context.Context, string, string, string, string) (github.Comparison, error) {
	return github.Comparison{}, nil
}
func (releaseGitHub) CheckRuns(context.Context, string, string, string) ([]github.CheckRun, error) {
	return nil, nil
}
func TestServeWiresRelease(t *testing.T) {
	for _, elapsed := range []time.Duration{31 * time.Minute, 30 * time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
			now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
			backend := &releaseBackend{}
			s := &scheduler.Scheduler{Backend: backend, Now: func() time.Time { return now }, Config: config.Config{MaxInFlight: 4, TokenBucket: config.TokenBucket{Capacity: 4, RefillPerSecond: 1}}}
			repo := config.Repository{Path: dir, Env: map[string]string{"AGENT_GH_REPO": "owner/repo"}}
			s.Config.Repositories = []config.Repository{repo}
			w := &releaseWriter{}
			lane := &mergelane.Lane{Root: dir, Repositories: map[string]config.Repository{dir: repo}, GitHub: releaseGitHub{}}
			s.Merger = &serveReleases{scheduler: s, lane: lane, open: func(context.Context, config.Repository) (release.Source, release.PRWriter, error) {
				return releaseSource{now.Add(-elapsed)}, w, nil
			}}
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
			queued, err := lane.HasRelease("owner/repo", "demo")
			if err != nil {
				t.Fatal(err)
			}
			if backend.snapshots != 1 || len(w.plans) != want || queued != (want == 1) {
				t.Fatalf("snapshots=%d plans=%v queue=%v", backend.snapshots, w.plans, queued)
			}
			if want == 1 && (w.plans[0].Version != "1.0.1" || len(w.plans[0].Files) != 2) {
				t.Fatal(w.plans)
			}
		})
	}
}
