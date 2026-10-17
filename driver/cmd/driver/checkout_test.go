package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A clean main checkout follows origin/main so roles read from it and the core
// check see what was merged; a checkout with local edits is left alone.
func TestFastForwardCheckout(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(key, "t")
	}
	for _, key := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(key, "t@example.com")
	}
	origin, repo, other := filepath.Join(root, "origin.git"), filepath.Join(root, "repo"), filepath.Join(root, "other")
	gitIn(t, root, "init", "--quiet", "--bare", "-b", "main", origin)
	gitIn(t, root, "clone", "--quiet", origin, repo)
	gitIn(t, repo, "commit", "--quiet", "--allow-empty", "-m", "init")
	gitIn(t, repo, "push", "--quiet", "origin", "HEAD:main")
	gitIn(t, root, "clone", "--quiet", origin, other)
	gitIn(t, other, "commit", "--quiet", "--allow-empty", "-m", "merged")
	gitIn(t, other, "push", "--quiet", "origin", "HEAD:main")
	want := gitIn(t, other, "rev-parse", "HEAD")

	if err := fastForwardCheckout(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, repo, "rev-parse", "HEAD"); got != want {
		t.Fatalf("checkout at %s, want origin/main %s", got, want)
	}

	gitIn(t, other, "commit", "--quiet", "--allow-empty", "-m", "next")
	gitIn(t, other, "push", "--quiet", "origin", "HEAD:main")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "tracked.txt")
	if err := fastForwardCheckout(context.Background(), repo); err == nil {
		t.Fatal("dirty checkout fast-forwarded")
	}
	if got := gitIn(t, repo, "rev-parse", "HEAD"); got != want {
		t.Fatalf("dirty checkout moved to %s", got)
	}
}
