package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
)

// newEnableFixture builds a bare origin (branch main) and a local clone
// whose current branch is rooted before any onboarding commit, so its
// working tree never carries scripts/agent-gates/agent-graph.env even when
// merged is true and origin/main does: mergedAgentGraphInstalled must read
// origin/main, never the local checkout's current branch.
func newEnableFixture(t *testing.T, merged bool) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	gitIn(t, filepath.Dir(origin), "init", "--quiet", "--bare", "-b", "main", origin)
	repo := filepath.Join(root, "repo")
	gitIn(t, filepath.Dir(origin), "clone", "--quiet", origin, repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "README.md")
	gitIn(t, repo, "commit", "--quiet", "-m", "init")
	base := gitIn(t, repo, "rev-parse", "HEAD")
	if merged {
		if err := os.MkdirAll(filepath.Join(repo, "scripts/agent-gates"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "scripts/agent-gates/agent-graph.env"), []byte("AGENT_ISSUE_BACKEND=feishu\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, repo, "add", "scripts/agent-gates/agent-graph.env")
		gitIn(t, repo, "commit", "--quiet", "-m", "onboard")
	}
	gitIn(t, repo, "push", "--quiet", "origin", "HEAD:main")
	gitIn(t, repo, "checkout", "--quiet", "-b", "feat/agent-graph", base)
	return repo
}

// TestEnablerRun covers OF-54: enabler.run re-validates live that the
// onboarding PR merged into origin/main (via git ls-tree, not the local
// checkout's branch) and that the repository's own check still passes (or
// only lags on core, OF-53) before writing the ledger row; either check
// failing writes nothing and names the reason.
func TestEnablerRun(t *testing.T) {
	t.Run("merged and check passing enables and writes the row", func(t *testing.T) {
		repo := newEnableFixture(t, true)
		var written []string
		e := enabler{
			checker: func(context.Context, string) ([]string, error) { return nil, nil },
			writeRow: func(_ context.Context, remote, operator string) (issues.LedgerProject, error) {
				written = append(written, remote+" "+operator)
				return issues.LedgerProject{Name: "dujiao-next", Remote: remote, Prefix: "DJ", Enabled: true}, nil
			},
		}
		result, err := e.run(context.Background(), intake.EnableRequest{Repo: repo, Remote: "github.com/dujiao-next/dujiao-next", Requester: "ou_APPROVER"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Row.Prefix != "DJ" || !result.Row.Enabled {
			t.Fatalf("result=%+v", result)
		}
		if len(written) != 1 || written[0] != "github.com/dujiao-next/dujiao-next ou_APPROVER" {
			t.Fatalf("written=%v", written)
		}
	})

	t.Run("core-behind check still enables (OF-53)", func(t *testing.T) {
		repo := newEnableFixture(t, true)
		var written []string
		e := enabler{
			checker: func(context.Context, string) ([]string, error) { return []string{"core/gate.sh"}, nil },
			writeRow: func(_ context.Context, remote, _ string) (issues.LedgerProject, error) {
				written = append(written, remote)
				return issues.LedgerProject{Remote: remote}, nil
			},
		}
		if _, err := e.run(context.Background(), intake.EnableRequest{Repo: repo, Remote: "x"}); err != nil {
			t.Fatal(err)
		}
		if len(written) != 1 {
			t.Fatalf("written=%v", written)
		}
	})

	t.Run("not merged refuses without writing", func(t *testing.T) {
		repo := newEnableFixture(t, false)
		var written []string
		e := enabler{
			checker: func(context.Context, string) ([]string, error) { return nil, nil },
			writeRow: func(_ context.Context, remote, _ string) (issues.LedgerProject, error) {
				written = append(written, remote)
				return issues.LedgerProject{}, nil
			},
		}
		_, err := e.run(context.Background(), intake.EnableRequest{Repo: repo, Remote: "x"})
		if err == nil || !strings.Contains(err.Error(), "未合并") {
			t.Fatalf("err=%v", err)
		}
		if len(written) != 0 {
			t.Fatalf("written=%v, want no write", written)
		}
	})

	t.Run("check failure refuses without writing", func(t *testing.T) {
		repo := newEnableFixture(t, true)
		var written []string
		e := enabler{
			checker: func(context.Context, string) ([]string, error) { return nil, errors.New("locally modified core file") },
			writeRow: func(_ context.Context, remote, _ string) (issues.LedgerProject, error) {
				written = append(written, remote)
				return issues.LedgerProject{}, nil
			},
		}
		_, err := e.run(context.Background(), intake.EnableRequest{Repo: repo, Remote: "x"})
		if err == nil || !strings.Contains(err.Error(), "check 失败") {
			t.Fatalf("err=%v", err)
		}
		if len(written) != 0 {
			t.Fatalf("written=%v, want no write", written)
		}
	})

	t.Run("cleanup runs after a successful enable and its note reaches the result", func(t *testing.T) {
		repo := newEnableFixture(t, true)
		var calledWith []string
		e := enabler{
			checker: func(context.Context, string) ([]string, error) { return nil, nil },
			writeRow: func(_ context.Context, remote, _ string) (issues.LedgerProject, error) {
				return issues.LedgerProject{Remote: remote}, nil
			},
			cleanup: func(_ context.Context, repo, remote string) string {
				calledWith = []string{repo, remote}
				return "本地分支 feat/agent-graph 保留（尚未合入 origin/main 且远端分支仍存在）"
			},
		}
		result, err := e.run(context.Background(), intake.EnableRequest{Repo: repo, Remote: "github.com/acme/widget"})
		if err != nil {
			t.Fatal(err)
		}
		if len(calledWith) != 2 || calledWith[0] != repo || calledWith[1] != "github.com/acme/widget" {
			t.Fatalf("cleanup called with %v, want [%s github.com/acme/widget]", calledWith, repo)
		}
		if result.Cleanup == "" {
			t.Fatalf("result.Cleanup empty, want the cleanup note")
		}
	})

	t.Run("a nil cleanup field is a no-op", func(t *testing.T) {
		repo := newEnableFixture(t, true)
		e := enabler{
			checker: func(context.Context, string) ([]string, error) { return nil, nil },
			writeRow: func(_ context.Context, remote, _ string) (issues.LedgerProject, error) {
				return issues.LedgerProject{Remote: remote}, nil
			},
		}
		result, err := e.run(context.Background(), intake.EnableRequest{Repo: repo, Remote: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Cleanup != "" {
			t.Fatalf("Cleanup=%q, want empty", result.Cleanup)
		}
	})

	t.Run("cleanup failure never fails the enable", func(t *testing.T) {
		repo := newEnableFixture(t, true)
		e := enabler{
			checker: func(context.Context, string) ([]string, error) { return nil, nil },
			writeRow: func(_ context.Context, remote, _ string) (issues.LedgerProject, error) {
				return issues.LedgerProject{Remote: remote}, nil
			},
			cleanup: func(context.Context, string, string) string {
				return "接入 worktree 未清理（/x/worktree）：git worktree remove 失败"
			},
		}
		result, err := e.run(context.Background(), intake.EnableRequest{Repo: repo, Remote: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Cleanup, "/x/worktree") {
			t.Fatalf("Cleanup=%q, want it to name the residual path", result.Cleanup)
		}
	})
}

// cleanupFixture is a target repository with its own origin and an OF-50
// onboarding worktree checked out on feat/agent-graph, at the exact
// cacheDir-rooted path onboard.go's addWorktree computes
// (onboardKey(remote) under <cacheDir>/orca-factory/onboard), plus an
// unrelated worktree and branch that OF-57 cleanup must never touch.
type cleanupFixture struct {
	repo, cacheDir, remote, worktree string
	otherWorktree, otherBranch       string
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newCleanupFixture(t *testing.T) cleanupFixture {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	gitIn(t, filepath.Dir(origin), "init", "--quiet", "--bare", "-b", "main", origin)
	repo := filepath.Join(root, "repo")
	gitIn(t, filepath.Dir(origin), "clone", "--quiet", origin, repo)
	writeTestFile(t, filepath.Join(repo, "README.md"), "x\n")
	gitIn(t, repo, "add", "README.md")
	gitIn(t, repo, "commit", "--quiet", "-m", "init")
	gitIn(t, repo, "push", "--quiet", "origin", "HEAD:main")

	remote := "github.com/acme/widget"
	cacheDir := filepath.Join(root, "cache")
	worktree := filepath.Join(cacheDir, "orca-factory", "onboard", onboardKey(remote), "worktree")
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "worktree", "add", "--quiet", "-b", onboardBranch, worktree, "origin/main")
	writeTestFile(t, filepath.Join(worktree, "scripts/agent-gates/agent-graph.env"), "AGENT_ISSUE_BACKEND=feishu\n")
	gitIn(t, worktree, "add", "scripts/agent-gates/agent-graph.env")
	gitIn(t, worktree, "commit", "--quiet", "-m", "onboard")
	gitIn(t, worktree, "push", "--quiet", "origin", onboardBranch)

	otherBranch, otherWorktree := "run-unrelated", filepath.Join(root, "other-worktree")
	gitIn(t, repo, "worktree", "add", "--quiet", "-b", otherBranch, otherWorktree, "main")

	return cleanupFixture{repo: repo, cacheDir: cacheDir, remote: remote, worktree: worktree, otherWorktree: otherWorktree, otherBranch: otherBranch}
}

// mergeOnboardBranch simulates the onboarding PR's ordinary (non-squash)
// merge: the feat/agent-graph commit becomes a real ancestor of main, and
// the result is pushed and fetched back so origin/main is fresh.
func (f cleanupFixture) mergeOnboardBranch(t *testing.T) {
	t.Helper()
	gitIn(t, f.repo, "merge", "--no-ff", "--quiet", "-m", "merge onboarding", onboardBranch)
	gitIn(t, f.repo, "push", "--quiet", "origin", "HEAD:main")
	gitIn(t, f.repo, "fetch", "--quiet", "origin", "main")
}

// squashMergeOnboardBranch simulates GitHub's squash merge: main advances
// through an independent commit carrying the same file (never an ancestor
// of feat/agent-graph), and the remote feat/agent-graph branch is deleted —
// exactly what OF-57's cleanup must treat as "safe to delete locally" even
// though merge-base --is-ancestor says no.
func (f cleanupFixture) squashMergeOnboardBranch(t *testing.T) {
	t.Helper()
	writeTestFile(t, filepath.Join(f.repo, "scripts/agent-gates/agent-graph.env"), "AGENT_ISSUE_BACKEND=feishu\n")
	gitIn(t, f.repo, "add", "scripts/agent-gates/agent-graph.env")
	gitIn(t, f.repo, "commit", "--quiet", "-m", "squash onboard")
	gitIn(t, f.repo, "push", "--quiet", "origin", "HEAD:main")
	gitIn(t, f.repo, "fetch", "--quiet", "origin", "main")
	gitIn(t, f.repo, "push", "--quiet", "origin", "--delete", onboardBranch)
}

func (f cleanupFixture) assertOtherUntouched(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(f.otherWorktree); err != nil {
		t.Fatalf("other worktree %s gone: %v", f.otherWorktree, err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".git", "refs", "heads", f.otherBranch)); err != nil {
		if out := gitIn(t, f.repo, "branch", "--list", f.otherBranch); out == "" {
			t.Fatalf("other branch %s gone", f.otherBranch)
		}
	}
}

// TestCleanupOnboardScene covers OF-57: after a successful enable, driver
// removes the onboarding worktree it created and, only when safe, the local
// feat/agent-graph branch — merged into the freshly-fetched origin/main, or
// its remote counterpart already gone (a squash merge). It never touches
// any other worktree or branch, and never deletes the remote branch itself.
func TestCleanupOnboardScene(t *testing.T) {
	t.Run("merged branch: worktree and local branch are both removed", func(t *testing.T) {
		f := newCleanupFixture(t)
		f.mergeOnboardBranch(t)
		note := cleanupOnboardScene(context.Background(), f.repo, f.remote, f.cacheDir)
		if note != "" {
			t.Fatalf("note=%q, want empty (fully clean)", note)
		}
		if _, err := os.Stat(f.worktree); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("worktree %s still present (err=%v)", f.worktree, err)
		}
		if out := gitIn(t, f.repo, "branch", "--list", onboardBranch); out != "" {
			t.Fatalf("branch %s still present: %q", onboardBranch, out)
		}
		f.assertOtherUntouched(t)
	})

	t.Run("squash merged: remote branch gone makes the local branch deletable too", func(t *testing.T) {
		f := newCleanupFixture(t)
		f.squashMergeOnboardBranch(t)
		note := cleanupOnboardScene(context.Background(), f.repo, f.remote, f.cacheDir)
		if note != "" {
			t.Fatalf("note=%q, want empty (fully clean)", note)
		}
		if _, err := os.Stat(f.worktree); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("worktree %s still present (err=%v)", f.worktree, err)
		}
		if out := gitIn(t, f.repo, "branch", "--list", onboardBranch); out != "" {
			t.Fatalf("branch %s still present: %q", onboardBranch, out)
		}
		f.assertOtherUntouched(t)
	})

	t.Run("not merged and remote branch still live: local branch is kept and noted", func(t *testing.T) {
		f := newCleanupFixture(t)
		gitIn(t, f.repo, "fetch", "--quiet", "origin", "main")
		note := cleanupOnboardScene(context.Background(), f.repo, f.remote, f.cacheDir)
		if !strings.Contains(note, onboardBranch) || !strings.Contains(note, "保留") {
			t.Fatalf("note=%q, want it to say the branch is kept", note)
		}
		if _, err := os.Stat(f.worktree); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("worktree %s still present (err=%v)", f.worktree, err)
		}
		if out := gitIn(t, f.repo, "branch", "--list", onboardBranch); out == "" {
			t.Fatalf("branch %s was deleted, want it kept", onboardBranch)
		}
		f.assertOtherUntouched(t)
	})

	t.Run("dirty worktree: removal fails, residual path is named, enable is unaffected", func(t *testing.T) {
		f := newCleanupFixture(t)
		f.mergeOnboardBranch(t)
		writeTestFile(t, filepath.Join(f.worktree, "scratch.txt"), "uncommitted\n")
		note := cleanupOnboardScene(context.Background(), f.repo, f.remote, f.cacheDir)
		if !strings.Contains(note, f.worktree) {
			t.Fatalf("note=%q, want it to name the residual worktree path %s", note, f.worktree)
		}
		if _, err := os.Stat(f.worktree); err != nil {
			t.Fatalf("worktree %s gone, want it kept after a failed removal (err=%v)", f.worktree, err)
		}
		f.assertOtherUntouched(t)
	})
}
