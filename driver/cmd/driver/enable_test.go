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
}
