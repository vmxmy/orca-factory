package release

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Real git repositories exercise snapshot/path filtering, isolated PR commits,
// exact-diff merge validation, and publication. Only the HTTP boundary is local.
func TestRepositoryReleaseLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := filepath.Join(root, "remote.git")
	local := filepath.Join(root, "checkout")
	git := func(dir string, args ...string) string {
		t.Helper()
		raw, err := runx.Run(ctx, runx.Spec{Path: "git", Dir: dir, Args: args, Timeout: time.Minute})
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(raw))
	}
	git(root, "init", "--bare", origin)
	git(root, "init", "-b", "main", local)
	git(local, "config", "user.name", "Test")
	git(local, "config", "user.email", "test@example.test")
	write := func(path, body string) {
		t.Helper()
		target := filepath.Join(local, path)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("mods/demo/.claude-plugin/plugin.json", `{"name":"demo","version":"1.0.0"}`)
	write("mods/demo/hooks/register.tsx", "const MOD_VERSION = '1.0.0'\n")
	write("mods/demo/feature", "before")
	git(local, "add", ".")
	git(local, "commit", "-m", "base")
	base := git(local, "rev-parse", "HEAD")
	git(local, "tag", "demo-v1.0.0")
	write("mods/demo/feature", "after")
	git(local, "add", ".")
	git(local, "commit", "-m", "fix: plugin bug")
	original := git(local, "rev-parse", "HEAD")
	git(local, "remote", "add", "origin", origin)
	git(local, "push", "origin", "main", "--tags")
	var pr github.PullRequest
	var releases []github.Release
	tags := []Tag{{Name: "demo-v1.0.0"}}
	tags[0].Commit.SHA = base
	created, merged, published := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		encode := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		switch {
		case req.URL.Path == "/repos/o/r/pulls" && req.Method == "GET":
			if pr.Number == 0 || pr.Merged {
				encode([]github.PullRequest{})
			} else {
				encode([]github.PullRequest{pr})
			}
		case req.URL.Path == "/repos/o/r/pulls" && req.Method == "POST":
			var input github.CreatePRRequest
			if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			created++
			pr.Number = 7
			pr.State = "open"
			no, yes := false, true
			pr.Draft = &no
			pr.Mergeable = &yes
			pr.MergeableState = "clean"
			pr.Head.Ref = input.Head
			pr.Head.SHA = git(origin, "rev-parse", "refs/heads/"+input.Head)
			pr.Head.Repo.FullName = "o/r"
			pr.Base.Ref = "main"
			pr.Base.Repo.FullName = "o/r"
			encode(pr)
		case req.URL.Path == "/repos/o/r/pulls/7":
			encode(pr)
		case req.URL.Path == "/repos/o/r/git/ref/heads/main":
			encode(map[string]any{"object": map[string]string{"sha": git(origin, "rev-parse", "refs/heads/main")}})
		case strings.HasSuffix(req.URL.Path, "/check-runs"):
			encode(map[string]any{"total_count": 1, "check_runs": []map[string]any{{"id": 1, "head_sha": pr.Head.SHA, "status": "completed", "conclusion": "success"}}})
		case strings.HasSuffix(req.URL.Path, "/status"):
			encode(map[string]any{"total_count": 0, "statuses": []any{}})
		case req.URL.Path == "/repos/o/r/pulls/7/merge":
			var input map[string]string
			if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input["sha"] != pr.Head.SHA {
				t.Error("unfixed merge")
			}
			merged++
			pr.Merged = true
			pr.MergeSHA = pr.Head.SHA
			git(origin, "update-ref", "refs/heads/main", pr.MergeSHA)
			encode(map[string]bool{"merged": true})
		case req.URL.Path == "/repos/o/r/git/refs":
			var input map[string]string
			if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			git(origin, "update-ref", input["ref"], input["sha"])
			tag := Tag{Name: strings.TrimPrefix(input["ref"], "refs/tags/")}
			tag.Commit.SHA = input["sha"]
			tags = append(tags, tag)
			encode(map[string]string{"ref": input["ref"]})
		case req.URL.Path == "/repos/o/r/releases":
			published++
			releases = append(releases, github.Release{ID: 1, TagName: "demo-v1.0.1"})
			encode(releases[0])
		default:
			t.Errorf("unexpected HTTP %s %s", req.Method, req.URL)
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	// Use a clock that advances for write throttling without real sleeping.
	tick := time.Now()
	gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }, Now: func() time.Time { tick = tick.Add(2 * time.Second); return tick }})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &Repository{Path: local, Name: "o/r", GitHub: gh}
	adapter.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "gh" {
			endpoint := s.Args[len(s.Args)-1]
			switch {
			case strings.Contains(endpoint, "/pulls?"):
				return []byte(`[[]]`), nil
			case strings.Contains(endpoint, "/tags?"):
				return json.Marshal([][]Tag{tags})
			case strings.Contains(endpoint, "/releases?"):
				return json.Marshal([][]github.Release{releases})
			case strings.Contains(endpoint, "/compare/"):
				return []byte(`[{"status":"ahead"}]`), nil
			}
			return nil, fmt.Errorf("unexpected endpoint")
		}
		if len(s.Args) > 2 && s.Args[0] == "remote" && s.Args[1] == "get-url" {
			return []byte("https://github.com/o/r.git"), nil
		}
		if len(s.Args) > 3 && s.Args[0] == "remote" && s.Args[1] == "set-url" {
			s.Args = append([]string(nil), s.Args...)
			s.Args[3] = origin
		}
		return runx.Run(ctx, s)
	}
	manager := &Manager{File: filepath.Join(root, "release.json"), Request: merge.Request{Repository: "o/r", Path: local}, Source: adapter.Snapshot, Writer: adapter, Publisher: adapter, Now: func() time.Time { return time.Now().Add(time.Hour) }}
	manager.Merger = &MergeAdapter{Repository: adapter, Manager: manager}
	if err := manager.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatal("PR missing")
	}
	if got := git(local, "rev-parse", "HEAD"); got != original {
		t.Fatal("checkout moved")
	}
	if got := git(local, "status", "--porcelain"); got != "" {
		t.Fatal("checkout modified", got)
	}
	if err := manager.Advance(ctx); err != nil {
		t.Fatal(err)
	}
	if merged != 1 || published != 1 {
		t.Fatalf("merges=%d releases=%d", merged, published)
	}
	restored := *manager
	if err := restored.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restored.Advance(ctx); err != nil {
		t.Fatal(err)
	}
	if created != 1 || merged != 1 || published != 1 {
		t.Fatal("duplicate publication")
	}
}
