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
	if err := json.Unmarshal(read("github-release-tags.json"), &tags); err != nil {
		t.Fatal(err)
	}
	if got := latestVersionTag(tags, "swarm-board-v"); got != "" {
		t.Fatalf("unexpected initial tag=%q", got)
	}
	source := &RepositorySource{Repository: config.Repository{Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "anthropics/claude-code"}}, Head: "2bfb629dfaff0c8318047a4beb93cf1dc5b58b18"}
	source.Exec = func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "git" {
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
}
