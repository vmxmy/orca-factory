package release

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestRecordedReleaseReads(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile("../../testdata/recorded/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var tags []tagReceipt
	if err := json.Unmarshal(read("github-release-version-tags.json"), &tags); err != nil {
		t.Fatal(err)
	}
	if got := latestVersionTag(tags, "v"); got != "v2.1.289" {
		t.Fatalf("latest=%q", got)
	}
	tagSource := &RepositorySource{Repository: config.Repository{Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "vmxmy/orca-factory"}}}
	calls := 0
	tagSource.Exec = func(_ context.Context, s runx.Spec) ([]byte, error) {
		calls++
		if s.Path != "gh" || strings.Join(s.Args, " ") != "api repos/vmxmy/orca-factory/tags?per_page=100&page=1" || s.Dir != "/repo" || s.Timeout <= 0 {
			t.Fatalf("tag request=%+v", s)
		}
		return read("github-release-tags.json"), nil
	}
	if got, err := tagSource.LatestTag(context.Background(), "swarm-board"); err != nil || got != "" || calls != 1 {
		t.Fatalf("latest=%q calls=%d err=%v", got, calls, err)
	}

	source := &RepositorySource{Repository: config.Repository{Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "anthropics/claude-code"}}, Head: "2bfb629dfaff0c8318047a4beb93cf1dc5b58b18"}
	source.Exec = func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "git" {
			if strings.Join(s.Args, " ") == "log --format=%H 2bfb629dfaff0c8318047a4beb93cf1dc5b58b18 -- mods/swarm-board" {
				return nil, nil
			}
			if strings.Join(s.Args, " ") != "log --format=%H 2bfb629dfaff0c8318047a4beb93cf1dc5b58b18 -- CHANGELOG.md" {
				t.Fatal(s.Args)
			}
			return []byte("2bfb629dfaff0c8318047a4beb93cf1dc5b58b18\n"), nil
		}
		if strings.Join(s.Args, " ") != "api repos/anthropics/claude-code/compare/v2.1.288...2bfb629dfaff0c8318047a4beb93cf1dc5b58b18?per_page=100&page=1" {
			t.Fatal(s.Args)
		}
		return read("github-release-commits.json"), nil
	}
	commits, err := source.CommitsSince(context.Background(), "CHANGELOG.md", "v2.1.288")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || commits[0].Subject != "chore: Update CHANGELOG.md and feed.xml" || commits[0].Body != "" {
		t.Fatalf("commits=%+v", commits)
	}
	commits, err = source.CommitsSince(context.Background(), "mods/swarm-board", "v2.1.288")
	if err != nil || len(commits) != 0 {
		t.Fatalf("unrelated path commits=%+v err=%v", commits, err)
	}
}
