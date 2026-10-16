package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

const coreSyncBranchPrefix = "chore/orca-factory-core-sync-"

// coreSyncer opens the core sync PR for a taken-over project whose core copy
// lags the factory (OF-53): `orca-factory update` in a throwaway worktree of
// the project at origin/main, only scripts/agent-gates/ committed, pushed as
// chore/orca-factory-core-sync-<factory short SHA>, never merged. The remote
// branch or any open sync PR makes a later call for the same project a no-op.
type coreSyncer struct{ factoryRoot, cacheDir string }

func runGit(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	out, err := runx.Run(ctx, runx.Spec{Path: "git", Args: append([]string{"-C", dir}, args...), Timeout: timeout})
	return strings.TrimSpace(string(out)), err
}

func (c coreSyncer) sync(ctx context.Context, p projects.Project) (err error) {
	version, err := runGit(ctx, c.factoryRoot, 10*time.Second, "rev-parse", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("读取 factory 版本失败: %w", err)
	}
	branch := coreSyncBranchPrefix + version
	if open, err := c.syncPending(ctx, p.Path, branch); err != nil || open {
		return err
	}
	worktree := filepath.Join(c.cacheDir, "orca-factory", "core-sync", onboardKey(p.Remote))
	if err := removeSyncWorktree(ctx, p.Path, worktree); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, removeSyncWorktree(context.WithoutCancel(ctx), p.Path, worktree)) }()
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		return err
	}
	if _, err := runGit(ctx, p.Path, 2*time.Minute, "fetch", "--quiet", "origin", "main"); err != nil {
		return fmt.Errorf("fetch origin main 失败: %w", err)
	}
	if _, err := runGit(ctx, p.Path, 2*time.Minute, "worktree", "add", "--quiet", "--detach", worktree, "origin/main"); err != nil {
		return fmt.Errorf("创建同步 worktree 失败: %w", err)
	}
	if _, err := runx.Run(ctx, runx.Spec{Path: filepath.Join(c.factoryRoot, "bin", "orca-factory"), Args: []string{"update", worktree}, Timeout: 2 * time.Minute}); err != nil {
		return fmt.Errorf("orca-factory update 失败: %w", err)
	}
	if _, err := runGit(ctx, worktree, time.Minute, "add", "--", "scripts/agent-gates"); err != nil {
		return err
	}
	if _, err := runGit(ctx, worktree, time.Minute, "diff", "--cached", "--quiet"); err == nil {
		return nil
	}
	title := "chore: 同步 orca-factory core 到 " + version
	if _, err := runGit(ctx, worktree, time.Minute, "commit", "--quiet", "-m", title); err != nil {
		return fmt.Errorf("提交同步改动失败: %w", err)
	}
	if _, err := runGit(ctx, worktree, 2*time.Minute, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("推送 %s 失败: %w", branch, err)
	}
	body := fmt.Sprintf("orca-factory driver 自动开启（OF-53）：本仓库 `scripts/agent-gates/` 的 core 安装副本落后 factory %s。\n\n落后文件：\n\n- `%s`\n\n本 PR 只含 `orca-factory update` 对 `scripts/agent-gates/` 的改动，driver 不会自动合并。\n",
		version, strings.Join(p.CoreBehind, "`\n- `"))
	if _, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "create", "--head", branch, "--base", "main", "--title", title, "--body", body}, Dir: worktree, Timeout: time.Minute}); err != nil {
		return fmt.Errorf("gh pr create 失败: %w", err)
	}
	return nil
}

// syncPending reports whether this factory version's sync branch already
// exists on origin, or any sync PR is still open.
func (c coreSyncer) syncPending(ctx context.Context, repo, branch string) (bool, error) {
	heads, err := runGit(ctx, repo, 30*time.Second, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		return false, fmt.Errorf("ls-remote origin 失败: %w", err)
	}
	if heads != "" {
		return true, nil
	}
	out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "list", "--state", "open", "--json", "headRefName", "--limit", "100"}, Dir: repo, Timeout: 30 * time.Second})
	if err != nil {
		return false, fmt.Errorf("gh pr list 失败: %w", err)
	}
	type openPR struct {
		HeadRefName string `json:"headRefName"`
	}
	var prs []openPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return false, fmt.Errorf("gh pr list 输出不可解析: %w", err)
	}
	return slices.ContainsFunc(prs, func(pr openPR) bool { return strings.HasPrefix(pr.HeadRefName, coreSyncBranchPrefix) }), nil
}

// removeSyncWorktree drops a sync worktree, including one a crashed earlier
// run left behind.
func removeSyncWorktree(ctx context.Context, repo, worktree string) error {
	if err := os.RemoveAll(worktree); err != nil {
		return err
	}
	_, err := runGit(ctx, repo, 30*time.Second, "worktree", "prune")
	return err
}
