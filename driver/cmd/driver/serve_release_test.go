package main

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/mergelane"
	"github.com/vmxmy/orca-factory/driver/internal/pause"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type releaseSource struct{ last time.Time }

type failedReleaseSource struct{ releaseSource }

func (failedReleaseSource) Env(context.Context, string) (string, error) {
	return "", errors.New("release scan read unavailable")
}

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

type releaseBackend struct {
	statusFakeBackend
	snapshotErr error
	applyErr    error
}

func (b *releaseBackend) Apply(context.Context, scheduler.Ticket, scheduler.Action) error {
	return b.applyErr
}

func (b *releaseBackend) Snapshot(ctx context.Context, repos []config.Repository) ([]scheduler.Ticket, error) {
	tickets, _ := b.statusFakeBackend.Snapshot(ctx, repos)
	return tickets, b.snapshotErr
}

func (b *releaseBackend) AutomationDisabled(context.Context, config.Repository) (bool, error) {
	return true, nil
}

type releaseGitHub struct {
	mergelane.GitHub
	ready   bool
	merges  int
	findErr error
}

func (g *releaseGitHub) FindPRs(context.Context, string, string, string) ([]github.PullRequest, error) {
	return nil, g.findErr
}

type releaseTickets struct{}

func (releaseTickets) Apply(context.Context, scheduler.Ticket, scheduler.Action) error { return nil }
func (releaseTickets) Finish(context.Context, scheduler.Ticket, string) error          { return nil }

func (g *releaseGitHub) MergePR(context.Context, string, string, int, string) (github.PullRequest, error) {
	g.merges++
	return github.PullRequest{Merged: true, MergeCommitSHA: "squash"}, nil
}

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
func (g *releaseGitHub) CheckRuns(context.Context, string, string, string) ([]github.CheckRun, error) {
	if g.ready {
		return []github.CheckRun{{HeadSHA: "head", Status: "completed", Conclusion: "success"}}, nil
	}
	return nil, nil
}

type releasePublisher struct{ tags, releases int }

func (p *releasePublisher) CreateTag(context.Context, string, string, string, string) error {
	p.tags++
	return nil
}
func (p *releasePublisher) CreateRelease(context.Context, string, string, github.ReleaseRequest) (github.Release, error) {
	p.releases++
	return github.Release{}, nil
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
			lane := &mergelane.Lane{Root: dir, Repositories: map[string]config.Repository{dir: repo}, GitHub: &releaseGitHub{}}
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
	for _, failure := range []string{"snapshot", "admission", "missing feature PR", "ticket update", "release snapshot", "release scan", "paused"} {
		t.Run(failure+" blocks existing release", func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
			now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
			last := now.Add(-30 * time.Minute)
			backend, gh, pub := &releaseBackend{}, &releaseGitHub{}, &releasePublisher{}
			repo := config.Repository{Path: dir, MaxInFlight: 4, StaleHours: 24, Env: map[string]string{"AGENT_GH_REPO": "owner/repo"}}
			s := &scheduler.Scheduler{Backend: backend, Now: func() time.Time { return now }, Config: config.Config{Repositories: []config.Repository{repo}, MaxInFlight: 4, TokenBucket: config.TokenBucket{Capacity: 4, RefillPerSecond: 1}}}
			lane := &mergelane.Lane{Root: dir, Repositories: map[string]config.Repository{dir: repo}, GitHub: gh, Publisher: pub, Tickets: releaseTickets{}}
			if err := lane.SubmitRelease(context.Background(), &releaseWriter{}, merge.Request{Repository: "owner/repo", Path: dir}, release.Plan{Plugin: "queued", Version: "1.0.1"}); err != nil {
				t.Fatal(err)
			}
			w := &releaseWriter{}
			var openErr error
			var scanFailed bool
			s.Merger = &serveReleases{scheduler: s, lane: lane, open: func(context.Context, config.Repository) (release.Source, release.PRWriter, error) {
				if openErr != nil {
					return nil, nil, openErr
				}
				if scanFailed {
					return failedReleaseSource{releaseSource{last}}, w, nil
				}
				return releaseSource{last}, w, nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waits := 0
			var reported []error
			if err := serveWithStatus(ctx, filepath.Join(dir, "lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
				return s, &scheduler.Runtime{}, nil
			}, func(context.Context) error {
				waits++
				if waits == 1 {
					now = now.Add(time.Minute)
					gh.ready = true
					switch failure {
					case "snapshot":
						backend.snapshotErr = errors.New("snapshot unavailable")
					case "admission", "missing feature PR":
						backend.tickets = []scheduler.Ticket{{ID: "feature", Identifier: "ZII-1", Repo: dir, Owner: machine.OwnerMerge, Phase: machine.PhaseMergeQueue, UpdatedAt: now}}
						if failure == "admission" {
							gh.findErr = errors.New("feature PR unavailable")
						}
					case "paused":
						s.Pauses.Pause(pause.Key{Dependency: pause.GitHub, Repo: dir, Kind: pause.Credentials}, time.Time{})
					case "ticket update":
						backend.tickets = []scheduler.Ticket{{ID: "feature", Identifier: "ZII-1", Repo: dir, Candidate: true, UpdatedAt: now}}
						backend.applyErr = errors.New("ticket claim unavailable")
					case "release snapshot":
						openErr = errors.New("release snapshot unavailable")
					case "release scan":
						scanFailed = true
					}
				} else {
					cancel()
				}
				return nil
			}, func(err error) { reported = append(reported, err) }); err != nil {
				t.Fatal(err)
			}
			if len(w.plans) != 0 || gh.merges != 0 || pub.tags != 0 || pub.releases != 0 {
				t.Fatalf("failed tick wrote: PRs=%d merges=%d tags=%d releases=%d", len(w.plans), gh.merges, pub.tags, pub.releases)
			}
			if failure != "paused" && len(reported) == 0 {
				t.Fatal("failed tick was not reported")
			}
		})
	}
}
