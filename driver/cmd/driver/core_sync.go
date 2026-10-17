package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

const coreSyncBranchPrefix = "chore/orca-factory-core-sync-"

// coreSyncer opens the core sync PR for a taken-over project whose core copy
// lags the factory (OF-53): `orca-factory update` in a throwaway worktree of
// the project at origin/main, only scripts/agent-gates/ committed, pushed as
// chore/orca-factory-core-sync-<factory short SHA>. The remote branch or its
// PR turns a later call for the same version into autoMergeSyncPR instead of
// a second PR.
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
	open, err := c.syncPending(ctx, p.Path, branch)
	if err != nil {
		return err
	}
	if open {
		return autoMergeSyncPR(ctx, p.Path, branch)
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
	body := fmt.Sprintf("orca-factory driver 自动开启（OF-53）：本仓库 `scripts/agent-gates/` 的 core 安装副本落后 factory %s。\n\n落后文件：\n\n- `%s`\n\n本 PR 只含 `orca-factory update` 对 `scripts/agent-gates/` 的改动。必需检查全部通过且可合并后，driver 在下一次发现时自动 squash 合并（OF-133）。\n",
		version, strings.Join(p.CoreBehind, "`\n- `"))
	if _, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "create", "--head", branch, "--base", "main", "--title", title, "--body", body}, Dir: worktree, Timeout: time.Minute}); err != nil {
		return fmt.Errorf("gh pr create 失败: %w", err)
	}
	return nil
}

// syncPending reports whether this factory version's sync branch already
// exists on origin or has an open PR. An open sync PR for an older factory
// version is closed with its branch: nobody merges it once core moves on, and
// it would otherwise keep the project on stale core.
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
	for _, pr := range prs {
		if pr.HeadRefName == branch {
			return true, nil
		}
		if !strings.HasPrefix(pr.HeadRefName, coreSyncBranchPrefix) {
			continue
		}
		if _, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "close", pr.HeadRefName, "--delete-branch", "--comment", "已被 " + branch + " 取代。"}, Dir: repo, Timeout: time.Minute}); err != nil {
			return false, fmt.Errorf("关闭过期同步 PR %s 失败: %w", pr.HeadRefName, err)
		}
	}
	return false, nil
}

type syncPRView struct {
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	Mergeable   string `json:"mergeable"`
	Files       []struct {
		Path string `json:"path"`
	} `json:"files"`
}

func viewSyncPR(ctx context.Context, repo, branch string) (syncPRView, error) {
	var pr syncPRView
	out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "view", branch, "--json", "state,headRefName,headRefOid,mergeable,files"}, Dir: repo, Timeout: 30 * time.Second})
	if err != nil {
		return pr, fmt.Errorf("gh pr view %s 失败: %w", branch, err)
	}
	if err := json.Unmarshal(out, &pr); err != nil {
		return pr, fmt.Errorf("gh pr view %s 输出不可解析: %w", branch, err)
	}
	return pr, nil
}

// autoMergeSyncPR squash-merges this factory version's sync PR (OF-133) only
// when it touches nothing outside scripts/agent-gates/, is mergeable, and has
// at least one required check with every one passed. Any unmet condition is
// returned as an error so discovery logs a WARN and retries next time; an
// already merged PR is a no-op.
func autoMergeSyncPR(ctx context.Context, repo, branch string) error {
	pr, err := viewSyncPR(ctx, repo, branch)
	if err != nil {
		return err
	}
	if pr.State == "MERGED" {
		return nil
	}
	if pr.State != "OPEN" || pr.HeadRefName != branch {
		return fmt.Errorf("同步 PR %s 不自动合并: state=%s head=%s", branch, pr.State, pr.HeadRefName)
	}
	for _, f := range pr.Files {
		if !strings.HasPrefix(f.Path, "scripts/agent-gates/") {
			return fmt.Errorf("同步 PR %s 不自动合并: 改动了 scripts/agent-gates/ 以外的 %s", branch, f.Path)
		}
	}
	if pr.Mergeable != "MERGEABLE" {
		return fmt.Errorf("同步 PR %s 不自动合并: mergeable=%s", branch, pr.Mergeable)
	}
	// gh exits non-zero while a required check is pending or failed and when
	// the base branch requires none; all of these mean "not passed".
	out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "checks", branch, "--required", "--json", "name,bucket"}, Dir: repo, Timeout: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("同步 PR %s 不自动合并: 必需检查未全部通过: %w", branch, err)
	}
	var checks []struct{ Name, Bucket string }
	if err := json.Unmarshal(out, &checks); err != nil {
		return fmt.Errorf("gh pr checks %s 输出不可解析: %w", branch, err)
	}
	if len(checks) == 0 {
		return fmt.Errorf("同步 PR %s 不自动合并: 没有必需检查结果", branch)
	}
	for _, check := range checks {
		if check.Bucket != "pass" {
			return fmt.Errorf("同步 PR %s 不自动合并: 必需检查 %s 为 %s", branch, check.Name, check.Bucket)
		}
	}
	if _, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "merge", branch, "--squash", "--match-head-commit", pr.HeadRefOid}, Dir: repo, Timeout: time.Minute}); err != nil {
		return fmt.Errorf("gh pr merge %s 失败: %w", branch, err)
	}
	if pr, err = viewSyncPR(ctx, repo, branch); err != nil {
		return err
	}
	if pr.State != "MERGED" {
		return fmt.Errorf("同步 PR %s 合并后回读 state=%s", branch, pr.State)
	}
	return nil
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
