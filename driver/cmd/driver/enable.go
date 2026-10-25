package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// mergedAgentGraphInstalled reports whether req's target repository's
// origin/main (not the local checkout's current branch) already carries
// scripts/agent-gates/agent-graph.env, read via `git ls-tree` after a fetch
// (OF-54): the onboarding PR's merge into main is the fact that unlocks
// enabling, and the local checkout still predates that merge until
// enabler.run fast-forwards it (OF-195).
func mergedAgentGraphInstalled(ctx context.Context, repo string) (bool, error) {
	if _, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "fetch", "--quiet", "origin", "main"}, Timeout: 2 * time.Minute}); err != nil {
		return false, fmt.Errorf("git fetch origin main 失败：%w", err)
	}
	out, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "ls-tree", "origin/main", "--", "scripts/agent-gates/agent-graph.env"}, Timeout: 30 * time.Second})
	if err != nil {
		return false, fmt.Errorf("git ls-tree origin/main 失败：%w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// enabler builds intake.Enabler (OF-54): an approver's "启用" press
// re-validates live, rather than trusting a card's rendering (which can be
// stale, or absent entirely right after OF-50's onboarding completes), that
// the onboarding PR merged into origin/main and that the repository's own
// pipeline check still passes, before writing the ledger row's 启用=true.
type enabler struct {
	checker  func(ctx context.Context, path string) ([]string, error)
	writeRow func(ctx context.Context, remote, operator string) (issues.LedgerProject, error)
	// cleanup removes the onboarding scene an enabled repository's own
	// OF-50 onboarding run left behind (OF-57): nil skips it, matching
	// every enabler built before this field existed.
	cleanup func(ctx context.Context, repo, remote string) string
}

func (e enabler) run(ctx context.Context, req intake.EnableRequest) (intake.EnableResult, error) {
	merged, err := mergedAgentGraphInstalled(ctx, req.Repo)
	if err != nil {
		return intake.EnableResult{}, err
	}
	if !merged {
		return intake.EnableResult{}, errors.New("接入 PR 未合并（origin/main 上没有 scripts/agent-gates/agent-graph.env）")
	}
	// The merge fact lives on origin/main, but checker reads the local
	// checkout, which nothing else moves after the onboarding PR merges
	// (OF-195): fast-forward it first, or check inspects a checkout that
	// still lacks scripts/agent-gates and enable always fails.
	if err := fastForwardCheckout(ctx, req.Repo); err != nil {
		return intake.EnableResult{}, fmt.Errorf("主检出未快进到 origin/main：%w", err)
	}
	if _, err := e.checker(ctx, req.Repo); err != nil {
		return intake.EnableResult{}, fmt.Errorf("check 失败：%w", err)
	}
	row, err := e.writeRow(ctx, req.Remote, req.Requester)
	if err != nil {
		return intake.EnableResult{}, fmt.Errorf("项目表写入失败：%w", err)
	}
	result := intake.EnableResult{Row: row}
	if e.cleanup != nil {
		result.Cleanup = e.cleanup(ctx, req.Repo, req.Remote)
	}
	return result, nil
}

func init() { Register("enable", runEnable) }

// runEnable is the CLI enable entry (OF-195), the Feishu 启用 button's
// shell-driven counterpart over the same enabler.run. The remote comes from
// the Orca repo list, not project discovery: discovery reads the very
// checkout this command is about to fast-forward, so before that it still
// lacks scripts/agent-gates and would reject the repository it is here to
// enable.
func runEnable(args []string) int {
	flags := flag.NewFlagSet("enable", flag.ContinueOnError)
	repo := flags.String("repo", "", "目标仓库本机检出路径")
	if flags.Parse(args) != nil || *repo == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: orca-factory-driver enable --repo <path>")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg, err := config.Load(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if cfg.FactoryRoot == "" {
		fmt.Fprintln(os.Stderr, string(projects.ReasonFactoryRootUnconfigured))
		return 1
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	abs, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	repos, err := (&orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout}).RepoList(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	remote := ""
	for _, r := range repos {
		if r.Path == abs {
			remote = r.CanonicalKey
			break
		}
	}
	if remote == "" {
		fmt.Fprintf(os.Stderr, "%s 不在 Orca 仓库清单\n", abs)
		return 1
	}
	operator, err := operatorIdentity()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	result, err := enabler{checker: factoryChecker(cfg.FactoryRoot), writeRow: factoryEnabledWriter(cfg), cleanup: onboardSceneCleaner(cacheDir)}.run(ctx, intake.EnableRequest{Repo: abs, Remote: remote, Requester: operator})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// cleanupOnboardScene removes the OF-50 onboarding worktree at
// <cacheDir>/orca-factory/onboard/<remote key>/worktree, and, only when
// safe, the local feat/agent-graph branch, right after an OF-54 enable
// succeeds (OF-57). "Safe" means the branch is an ancestor of the
// already-fetched origin/main (an ordinary merge), or its remote
// counterpart no longer exists (a squash merge leaves a different local
// commit that is never an ancestor of main). It touches only this one
// worktree path and this one branch name — never another worktree, another
// branch, or the remote branch itself — and a failure here never fails the
// caller: it returns a short note naming what is left and why, or "" when
// the scene is fully clean.
func cleanupOnboardScene(ctx context.Context, repo, remote, cacheDir string) string {
	var notes []string
	worktree := onboardWorktree(cacheDir, remote)
	switch _, err := os.Stat(worktree); {
	case err == nil:
		if _, rmErr := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "worktree", "remove", worktree}, Timeout: 30 * time.Second}); rmErr != nil {
			notes = append(notes, fmt.Sprintf("接入 worktree 未清理（%s）：%v", worktree, rmErr))
		}
	case !errors.Is(err, os.ErrNotExist):
		notes = append(notes, fmt.Sprintf("接入 worktree 状态不可读（%s）：%v", worktree, err))
	}

	if _, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "show-ref", "--quiet", "refs/heads/" + onboardBranch}, Timeout: 10 * time.Second}); err != nil {
		return strings.Join(notes, "；")
	}
	deletable, reason, err := branchCleanupDecision(ctx, repo, onboardBranch)
	switch {
	case err != nil:
		notes = append(notes, fmt.Sprintf("本地分支 %s 合入状态检查失败：%v", onboardBranch, err))
	case !deletable:
		notes = append(notes, fmt.Sprintf("本地分支 %s 保留（%s）", onboardBranch, reason))
	default:
		if _, delErr := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "branch", "-D", onboardBranch}, Timeout: 10 * time.Second}); delErr != nil {
			notes = append(notes, fmt.Sprintf("本地分支 %s 删除失败（%s）：%v", onboardBranch, reason, delErr))
		}
	}
	return strings.Join(notes, "；")
}

// branchCleanupDecision reports whether branch is safe to delete locally in
// repo: an ancestor of the already-fetched origin/main (ordinary merge), or
// lacking a remote counterpart at origin (deleted after a squash merge,
// whose local commit never becomes an ancestor of main). It never fetches
// — callers that need a fresh origin/main must fetch first, as
// mergedAgentGraphInstalled already does before enabler.run reaches this
// point — and it never inspects or deletes anything but branch itself.
func branchCleanupDecision(ctx context.Context, repo, branch string) (deletable bool, reason string, err error) {
	merged, err := branchIsAncestor(ctx, repo, branch, "origin/main")
	if err != nil {
		return false, "", err
	}
	if merged {
		return true, "已合入 origin/main", nil
	}
	exists, err := remoteBranchExists(ctx, repo, branch)
	if err != nil {
		return false, "", err
	}
	if !exists {
		return true, "远端分支已不存在（squash 合并）", nil
	}
	return false, "尚未合入 origin/main 且远端分支仍存在", nil
}

// branchIsAncestor reports whether branch is an ancestor of ref in repo, via
// `git merge-base --is-ancestor`: exit 0 is true, exit 1 is false, any other
// outcome (a missing ref, for instance) is an error.
func branchIsAncestor(ctx context.Context, repo, branch, ref string) (bool, error) {
	_, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "merge-base", "--is-ancestor", branch, ref}, Timeout: 10 * time.Second})
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s：%w", branch, ref, err)
}

// remoteBranchExists reports whether branch still exists on repo's origin,
// via `git ls-remote --exit-code --heads origin <branch>`: exit 0 with
// output is true, exit 2 (no matching ref) is false, any other outcome is
// an error.
func remoteBranchExists(ctx context.Context, repo, branch string) (bool, error) {
	out, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "ls-remote", "--exit-code", "--heads", "origin", branch}, Timeout: 30 * time.Second})
	if err == nil {
		return strings.TrimSpace(string(out)) != "", nil
	}
	if exitCode(err) == 2 {
		return false, nil
	}
	return false, fmt.Errorf("git ls-remote --heads origin %s：%w", branch, err)
}

// onboardSceneCleaner builds enabler.cleanup (OF-57) bound to this
// machine's cache directory, the same root onboard.go's addWorktree used to
// create the onboarding worktree.
func onboardSceneCleaner(cacheDir string) func(context.Context, string, string) string {
	return func(ctx context.Context, repo, remote string) string {
		return cleanupOnboardScene(ctx, repo, remote, cacheDir)
	}
}

// factoryEnabledWriter writes the enabled ledger row through the factory's
// own Feishu ledger profile, mirroring factoryLedgerRow (OF-50).
func factoryEnabledWriter(cfg config.Config) func(context.Context, string, string) (issues.LedgerProject, error) {
	return func(ctx context.Context, remote, operator string) (issues.LedgerProject, error) {
		repository, err := config.LoadRepository(cfg.FactoryRoot)
		if err != nil {
			return issues.LedgerProject{}, err
		}
		client, err := issues.New(ctx, repository.Env, cfg.Tuning)
		if err != nil {
			return issues.LedgerProject{}, err
		}
		ledger, ok := client.(*issues.Feishu)
		if !ok {
			return issues.LedgerProject{}, errors.New("factory 账本不是飞书")
		}
		return ledger.EnableLedgerProject(ctx, remote, operator)
	}
}
