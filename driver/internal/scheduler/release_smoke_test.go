package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type releaseSmokePublisher struct{}

func (releaseSmokePublisher) CreateTag(context.Context, string, string, string, string) error {
	return nil
}
func (releaseSmokePublisher) CreateRelease(context.Context, string, string, github.ReleaseRequest) (github.Release, error) {
	return github.Release{}, nil
}

// The remote advances only after the driver's clone exists: the authoritative
// squash commit must genuinely be absent locally, not aliased to the PR head.
func TestReleaseSmokeFetchesSquashAndRetriesPreparation(t *testing.T) {
	for _, failPreparation := range []bool{false, true} {
		t.Run(map[bool]string{false: "remote_squash", true: "preparation_retry"}[failPreparation], func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			git := func(dir string, args ...string) string {
				t.Helper()
				b, err := runx.Run(ctx, runx.Spec{Path: "git", Dir: dir, Args: args, Timeout: time.Minute})
				if err != nil {
					t.Fatalf("git %v: %v", args, err)
				}
				return strings.TrimSpace(string(b))
			}
			remote, local := filepath.Join(root, "remote"), filepath.Join(root, "local")
			git(root, "init", "-b", "main", remote)
			git(remote, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "base")
			git(root, "clone", "--no-local", remote, local)
			git(remote, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "release squash")
			sha := git(remote, "rev-parse", "HEAD")
			if _, err := runx.Run(ctx, runx.Spec{Path: "git", Dir: local, Args: []string{"cat-file", "-e", sha}, Timeout: time.Minute}); err == nil {
				t.Fatal("squash already present in driver checkout")
			}
			file := filepath.Join(root, "release.json")
			rows := []release.Entry{{EnqueuedAt: time.Now(), MergeSHA: sha, Pending: release.Pending{Request: merge.Request{Repository: "o/r", Path: local, Number: 7, SmokeCommand: "true"}, Plan: release.Plan{Plugin: "demo", Version: "1.0.1"}}}}
			raw, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			reject := failPreparation
			runs := 0
			runtime := &Runtime{Run: func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if s.Path == "git" && reject {
					return nil, errors.New("preparation unavailable")
				}
				if s.Path == "bash" {
					runs++
					if got := git(s.Dir, "rev-parse", "HEAD"); got != sha {
						t.Fatalf("smoke checked out %s want squash %s", got, sha)
					}
					b, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					var persisted []release.Entry
					if err = json.Unmarshal(b, &persisted); err != nil {
						t.Fatal(err)
					}
					if !persisted[0].SmokeStarted {
						t.Fatal("smoke ran before durable start receipt")
					}
				}
				return runx.Run(ctx, s)
			}}
			manager := &release.Manager{File: file, Publisher: releaseSmokePublisher{}, Deploy: &releaseDeployment{runtime: runtime}}
			if failPreparation {
				if err = manager.Advance(ctx); err == nil {
					t.Fatal("preparation failure not reported")
				}
				if runs != 0 {
					t.Fatal("smoke ran during failed preparation")
				}
				reject = false
			}
			restored := *manager
			if err = restored.Advance(ctx); err != nil {
				t.Fatalf("release smoke did not recover and run: %v", err)
			}
			if runs != 1 {
				t.Fatalf("smoke runs=%d want 1", runs)
			}
			if err = restored.Advance(ctx); err != nil {
				t.Fatal(err)
			}
			if runs != 1 {
				t.Fatal("successful smoke replayed")
			}
		})
	}
}

func TestReleaseSmokeEnvironment(t *testing.T) {
	t.Setenv("AGENT_MERGE_SHA", "stale-sha")
	t.Setenv("AGENT_GH_REPO", "stale/repo")
	t.Setenv("RELEASE_SMOKE_TEST_KEEP", "keep-me")
	sha := strings.Repeat("a", 40)
	var command *runx.Spec
	runtime := &Runtime{Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "bash" {
			command = &s
		}
		return nil, nil
	}}
	d := &releaseDeployment{runtime: runtime}
	if err := d.Smoke(context.Background(), merge.Request{Path: t.TempDir(), Repository: "o/r", SmokeCommand: "true"}, sha, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if command == nil {
		t.Fatal("smoke command missing")
	}
	for _, want := range []string{"AGENT_MERGE_SHA=" + sha, "AGENT_GH_REPO=o/r", "RELEASE_SMOKE_TEST_KEEP=keep-me"} {
		count := 0
		for _, value := range command.Env {
			if value == want {
				count++
			}
		}
		if count != 1 {
			t.Errorf("exact environment entry %q count=%d want 1", want, count)
		}
	}
	for _, value := range command.Env {
		if (strings.HasPrefix(value, "AGENT_MERGE_SHA=") && value != "AGENT_MERGE_SHA="+sha) || (strings.HasPrefix(value, "AGENT_GH_REPO=") && value != "AGENT_GH_REPO=o/r") {
			t.Errorf("stale environment entry %q", value)
		}
	}
	if command.Timeout != 30*time.Second {
		t.Errorf("smoke timeout=%s want documented 30s", command.Timeout)
	}
}
