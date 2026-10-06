package release

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// These are unmodified gh api receipts, not synthetic Source responses. The
// compare commits are repository-wide; a Source still needs path filtering.
func TestRecordedReleaseReads(t *testing.T) {
	read := func(name string, target any) {
		t.Helper()
		raw, err := os.ReadFile("../../testdata/recorded/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, target); err != nil {
			t.Fatal(err)
		}
	}
	var tags []Tag
	read("github-release-tags.json", &tags)
	if len(tags) != 1 || tags[0].Name != "swarm-board-v0.3.17" || tags[0].Commit.SHA != "b912a73e868bb09460df4f06e4687e007b394eb8" {
		t.Fatalf("unexpected baseline tags: %+v", tags)
	}
	src := &Repository{tags: tags}
	latest, err := src.LatestTag(context.Background(), "swarm-board")
	if err != nil || latest != "swarm-board-v0.3.17" {
		t.Fatalf("latest=%s err=%v", latest, err)
	}
	var comparison Comparison
	read("github-release-compare.json", &comparison)
	if comparison.Status != "ahead" || comparison.BaseCommit.SHA != tags[0].Commit.SHA || comparison.TotalCommits != len(comparison.Commits) {
		t.Fatalf("incomplete or mismatched comparison: %+v", comparison)
	}
	for _, c := range comparison.Commits {
		if strings.HasPrefix(c.SHA, "eeb82fd") && strings.HasPrefix(c.Commit.Message, "feat(driver): add ticket usage contract") {
			return
		}
	}
	t.Fatal("post-release change eeb82fd missing")
}
