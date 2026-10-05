package release

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestPublisherRecoversPartialWrites(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing tag and release", true: "tag points elsewhere"}[mismatch], func(t *testing.T) {
			client := &fakePublisher{}
			p := &RecoveringPublisher{Client: client, Source: &RepositorySource{Exec: func(_ context.Context, s runx.Spec) ([]byte, error) {
				if strings.Contains(s.Args[1], "/tags?") {
					if mismatch {
						return []byte(`[{"name":"demo-v1.0.1","commit":{"sha":"other"}}]`), nil
					}
					return []byte(`[{"name":"demo-v1.0.1","commit":{"sha":"merged"}}]`), nil
				}
				if strings.Contains(s.Args[1], "/releases?") {
					return []byte(`[{"id":7,"tag_name":"demo-v1.0.1"}]`), nil
				}
				return nil, errors.New("unexpected command")
			}}}
			err := p.CreateTag(context.Background(), "owner", "repo", "demo-v1.0.1", "merged")
			if mismatch {
				if err == nil {
					t.Fatal("accepted unrelated tag")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.CreateRelease(context.Background(), "owner", "repo", github.ReleaseRequest{TagName: "demo-v1.0.1"})
			if err != nil || r.ID != 7 || len(client.calls) != 0 {
				t.Fatalf("result=%+v err=%v writes=%v", r, err, client.calls)
			}
		})
	}
}

type prClient struct {
	prs     []github.PullRequest
	created []github.CreatePRRequest
}

func (c *prClient) FindPRs(context.Context, string, string, string) ([]github.PullRequest, error) {
	return c.prs, nil
}
func (c *prClient) CreatePR(_ context.Context, _, _ string, r github.CreatePRRequest) (github.PullRequest, error) {
	c.created = append(c.created, r)
	return github.PullRequest{Number: 42}, nil
}
func TestWriterRefusesDifferentRecoveredPlan(t *testing.T) {
	source := &RepositorySource{Exec: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if len(s.Args) > 0 && s.Args[0] == "fetch" {
			return nil, nil
		}
		if len(s.Args) > 0 && s.Args[0] == "show" {
			return []byte("different remote files"), nil
		}
		return nil, errors.New("unexpected command")
	}}
	client := &prClient{prs: []github.PullRequest{{Number: 42, State: "open"}}}
	p := Plan{Plugin: "demo", Version: "1.0.1", Files: map[string][]byte{"mods/demo/CHANGELOG.md": []byte("planned contents"), "mods/demo/.claude-plugin/plugin.json": []byte(`{"version":"1.0.1"}`)}}
	if _, err := (&Writer{Source: source, GitHub: client}).CreateReleasePR(context.Background(), merge.Request{Repository: "owner/repo"}, p); err == nil {
		t.Fatal("recovered unrelated release content")
	}
}
func TestWriterCreatesOnlyReleaseFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	git := func(dir string, args ...string) string {
		t.Helper()
		b, err := runx.Run(ctx, runx.Spec{Path: "git", Args: args, Dir: dir, Timeout: time.Minute})
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(b))
	}
	git(root, "init", "--bare", origin)
	git(root, "init", "-b", "main", repo)
	git(repo, "config", "user.email", "test@example.invalid")
	git(repo, "config", "user.name", "Test")
	manifest := filepath.Join(repo, "mods/demo/.claude-plugin/plugin.json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"version":"1.0.0"}`), 0644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", ".")
	git(repo, "commit", "-m", "feat: plugin")
	git(repo, "remote", "add", "origin", origin)
	git(repo, "push", "origin", "main")
	head := git(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	source := &RepositorySource{Repository: config.Repository{Path: repo}, Head: head}
	client := &prClient{}
	plan, err := Generate("demo", []byte(`{"version":"1.0.0"}`), nil, []Commit{{Subject: "fix: corrected"}})
	if err != nil {
		t.Fatal(err)
	}
	number, err := (&Writer{Source: source, GitHub: client}).CreateReleasePR(ctx, merge.Request{Repository: "owner/repo", Path: repo}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if number != 42 || len(client.created) != 1 || client.created[0].Head != "agent/release/demo-v1.0.1" {
		t.Fatalf("number=%d creates=%v", number, client.created)
	}
	changed := git(origin, "diff", "--name-only", head, "refs/heads/agent/release/demo-v1.0.1")
	if changed != "mods/demo/.claude-plugin/plugin.json\nmods/demo/CHANGELOG.md" {
		t.Fatalf("changed=%q", changed)
	}
	if git(repo, "rev-parse", "HEAD") != head {
		t.Fatal("source checkout moved")
	}
	if count := strings.Count(git(repo, "worktree", "list", "--porcelain"), "worktree "); count != 1 {
		t.Fatalf("worktrees=%d", count)
	}
	for path, want := range plan.Files {
		if got := git(origin, "show", "refs/heads/agent/release/demo-v1.0.1:"+path); got != strings.TrimSpace(string(want)) {
			t.Fatalf("%s=%q", path, got)
		}
	}
}
