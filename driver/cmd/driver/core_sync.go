package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
// a second PR. reported holds, per project path, the last auto-merge refusal
// returned, so discovery warns once per PR and reason rather than every pass.
type coreSyncer struct {
	factoryRoot, cacheDir string
	reported              map[string]string
}

func runGit(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	out, err := runx.Run(ctx, runx.Spec{Path: "git", Args: append([]string{"-C", dir}, args...), Timeout: timeout})
	return strings.TrimSpace(string(out)), err
}

func (c *coreSyncer) sync(ctx context.Context, p projects.Project) (err error) {
	version, err := runGit(ctx, c.factoryRoot, 10*time.Second, "rev-parse", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("读取 factory 版本失败: %w", err)
	}
	branch := coreSyncBranchPrefix + version
	target, err := parseGitHubRepo(p.Remote)
	if err != nil {
		return err
	}
	open, err := syncPending(ctx, p.Path, target, branch)
	if err != nil {
		return err
	}
	if open {
		err := ensureSyncPR(ctx, p, target, branch, version)
		if errors.Is(err, errSyncWaitingCI) {
			runx.LoggerFrom(ctx).Info("core sync 等待 CI", "path", p.Path, "reason", err)
			err = nil
		}
		return c.reportOnce(p.Path, err)
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
	if _, err := runGit(ctx, worktree, time.Minute, "commit", "--quiet", "-m", "chore: 同步 orca-factory core 到 "+version); err != nil {
		return fmt.Errorf("提交同步改动失败: %w", err)
	}
	if _, err := runGit(ctx, worktree, 2*time.Minute, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("推送 %s 失败: %w", branch, err)
	}
	return createSyncPR(ctx, p, branch, version)
}

// ensureSyncPR opens the PR for a sync branch already on origin when none
// from target exists yet (OF-159): the push succeeded but `gh pr create`
// failed, and the branch alone would otherwise skip every later sync.
func ensureSyncPR(ctx context.Context, p projects.Project, target githubRepo, branch, version string) error {
	out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "list", "--head", branch, "--state", "all", "--json", syncPRFields, "--limit", "100"}, Dir: p.Path, Timeout: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("gh pr list --head %s 失败: %w", branch, err)
	}
	var prs []syncPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return fmt.Errorf("gh pr list --head %s 输出不可解析: %w", branch, err)
	}
	if !slices.ContainsFunc(prs, func(pr syncPR) bool { return pr.fromRepo(target) && pr.HeadRefName == branch }) {
		if err := createSyncPR(ctx, p, branch, version); err != nil {
			return err
		}
	}
	return autoMergeSyncPR(ctx, p.Path, target, branch)
}

func createSyncPR(ctx context.Context, p projects.Project, branch, version string) error {
	title := "chore: 同步 orca-factory core 到 " + version
	body := fmt.Sprintf("orca-factory driver 自动开启（OF-53）：本仓库 `scripts/agent-gates/` 的 core 安装副本落后 factory %s。\n\n落后文件：\n\n- `%s`\n\n本 PR 只含 `orca-factory update` 对 `scripts/agent-gates/` 的改动。必需检查全部通过且可合并后，driver 在下一次发现时自动 squash 合并（OF-133）。\n",
		version, strings.Join(p.CoreBehind, "`\n- `"))
	if _, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "create", "--head", branch, "--base", "main", "--title", title, "--body", body}, Dir: p.Path, Timeout: time.Minute}); err != nil {
		return fmt.Errorf("gh pr create 失败: %w", err)
	}
	return nil
}

func (c *coreSyncer) reportOnce(path string, err error) error {
	if err == nil {
		delete(c.reported, path)
		return nil
	}
	if c.reported[path] == err.Error() {
		return nil
	}
	if c.reported == nil {
		c.reported = map[string]string{}
	}
	c.reported[path] = err.Error()
	return err
}

type githubRepo struct{ owner, name string }

// parseGitHubRepo reads owner and name from a project's normalized remote,
// host/owner/name.
func parseGitHubRepo(remote string) (githubRepo, error) {
	parts := strings.Split(remote, "/")
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return githubRepo{}, fmt.Errorf("远端 %q 不是 host/owner/name", remote)
	}
	return githubRepo{owner: parts[1], name: parts[2]}, nil
}

const syncPRFields = "number,state,headRefName,headRefOid,isCrossRepository,headRepository,headRepositoryOwner"

type syncPR struct {
	Number            int    `json:"number"`
	State             string `json:"state"`
	HeadRefName       string `json:"headRefName"`
	HeadRefOid        string `json:"headRefOid"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	HeadRepository    struct {
		Name string `json:"name"`
	} `json:"headRepository"`
	HeadRepositoryOwner struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
	Mergeable    string `json:"mergeable"`
	ChangedFiles int    `json:"changedFiles"`
	Files        []struct {
		Path string `json:"path"`
	} `json:"files"`
}

// fromRepo is true only for a PR whose head branch lives in target itself:
// gh selects PRs by head branch name alone, so a fork can open one named like
// a sync branch.
func (pr syncPR) fromRepo(target githubRepo) bool {
	return !pr.IsCrossRepository && strings.EqualFold(pr.HeadRepositoryOwner.Login, target.owner) && strings.EqualFold(pr.HeadRepository.Name, target.name)
}

// syncPending reports whether this factory version's sync branch already
// exists on origin or has an open PR from target. An open sync PR from target
// for an older factory version is closed with its branch: nobody merges it
// once core moves on, and it would otherwise keep the project on stale core.
func syncPending(ctx context.Context, repo string, target githubRepo, branch string) (bool, error) {
	heads, err := runGit(ctx, repo, 30*time.Second, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		return false, fmt.Errorf("ls-remote origin 失败: %w", err)
	}
	if heads != "" {
		return true, nil
	}
	out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "list", "--state", "open", "--json", syncPRFields, "--limit", "100"}, Dir: repo, Timeout: 30 * time.Second})
	if err != nil {
		return false, fmt.Errorf("gh pr list 失败: %w", err)
	}
	var prs []syncPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return false, fmt.Errorf("gh pr list 输出不可解析: %w", err)
	}
	for _, pr := range prs {
		if !pr.fromRepo(target) || !strings.HasPrefix(pr.HeadRefName, coreSyncBranchPrefix) {
			continue
		}
		if pr.HeadRefName == branch {
			return true, nil
		}
		if _, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "close", strconv.Itoa(pr.Number), "--delete-branch", "--comment", "已被 " + branch + " 取代。"}, Dir: repo, Timeout: time.Minute}); err != nil {
			return false, fmt.Errorf("关闭过期同步 PR %s 失败: %w", pr.HeadRefName, err)
		}
	}
	return false, nil
}

func viewSyncPR(ctx context.Context, repo, selector string) (syncPR, error) {
	var pr syncPR
	out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "view", selector, "--json", syncPRFields + ",mergeable,changedFiles,files"}, Dir: repo, Timeout: 30 * time.Second})
	if err != nil {
		return pr, fmt.Errorf("gh pr view %s 失败: %w", selector, err)
	}
	if err := json.Unmarshal(out, &pr); err != nil {
		return pr, fmt.Errorf("gh pr view %s 输出不可解析: %w", selector, err)
	}
	return pr, nil
}

// maxPRFiles is how many files `gh pr view --json files` returns at most.
const maxPRFiles = 100

// autoMergeSyncPR squash-merges this factory version's sync PR (OF-133) only
// when its head is in target itself, it touches nothing outside
// scripts/agent-gates/, is mergeable, and its checks passed
// (syncPRChecksPassed). Checks, merge and read-back address the PR by the
// number just verified. Any unmet condition is returned as an error so
// discovery logs a WARN and retries next time; an already merged PR is a
// no-op.
func autoMergeSyncPR(ctx context.Context, repo string, target githubRepo, branch string) error {
	pr, err := viewSyncPR(ctx, repo, branch)
	if err != nil {
		return err
	}
	if !pr.fromRepo(target) {
		return fmt.Errorf("同步 PR %s 不自动合并: head 不在 %s/%s（cross=%t head=%s/%s）", branch, target.owner, target.name, pr.IsCrossRepository, pr.HeadRepositoryOwner.Login, pr.HeadRepository.Name)
	}
	if pr.State == "MERGED" {
		return nil
	}
	if pr.State != "OPEN" || pr.HeadRefName != branch {
		return fmt.Errorf("同步 PR %s 不自动合并: state=%s head=%s", branch, pr.State, pr.HeadRefName)
	}
	if len(pr.Files) >= maxPRFiles || pr.ChangedFiles != len(pr.Files) {
		return fmt.Errorf("同步 PR %s 不自动合并: 文件列表不完整（changedFiles=%d 取到 %d）", branch, pr.ChangedFiles, len(pr.Files))
	}
	for _, f := range pr.Files {
		if !strings.HasPrefix(f.Path, "scripts/agent-gates/") {
			return fmt.Errorf("同步 PR %s 不自动合并: 改动了 scripts/agent-gates/ 以外的 %s", branch, f.Path)
		}
	}
	if pr.Mergeable != "MERGEABLE" {
		return fmt.Errorf("同步 PR %s 不自动合并: mergeable=%s", branch, pr.Mergeable)
	}
	number := strconv.Itoa(pr.Number)
	if err := syncPRChecksPassed(ctx, repo, branch, number); err != nil {
		return err
	}
	if _, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "merge", number, "--squash", "--match-head-commit", pr.HeadRefOid}, Dir: repo, Timeout: time.Minute}); err != nil {
		return fmt.Errorf("gh pr merge %s 失败: %w", branch, err)
	}
	if pr, err = viewSyncPR(ctx, repo, number); err != nil {
		return err
	}
	if pr.State != "MERGED" {
		return fmt.Errorf("同步 PR %s 合并后回读 state=%s", branch, pr.State)
	}
	return nil
}

var errSyncWaitingCI = errors.New("等待 CI")

// syncPRChecksPassed requires every required check of the PR to pass. gh
// exits non-zero while a required check is pending or failed; all of these
// mean "not passed". A base branch that requires no checks (OF-168: gh exits
// 1 with "no required checks reported") falls back to all of the PR's
// checks: at least one, each passed or skipped, and pending ones wait for CI
// rather than count as a refusal.
func syncPRChecksPassed(ctx context.Context, repo, branch, number string) error {
	out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "checks", number, "--required", "--json", "name,bucket"}, Dir: repo, Timeout: 30 * time.Second})
	var exit *exec.ExitError
	required := !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(exit.Stderr), "no required checks reported")
	if !required {
		out, err = runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "checks", number, "--json", "name,bucket"}, Dir: repo, Timeout: 30 * time.Second})
	}
	// gh exits 8 while checks are pending and 1 when one failed; the JSON still
	// lists every check, so the per-check verdict below names the cause.
	if errors.As(err, &exit) && (exit.ExitCode() == 8 || exit.ExitCode() == 1) && json.Valid(out) && len(bytes.TrimSpace(out)) > 0 {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("同步 PR %s 不自动合并: 检查未全部通过（required=%t）: %w", branch, required, err)
	}
	var checks []struct{ Name, Bucket string }
	if err := json.Unmarshal(out, &checks); err != nil {
		return fmt.Errorf("gh pr checks %s 输出不可解析: %w", branch, err)
	}
	if len(checks) == 0 {
		return fmt.Errorf("同步 PR %s 不自动合并: 没有检查结果（required=%t）", branch, required)
	}
	var pending []string
	for _, check := range checks {
		switch {
		case check.Bucket == "pass":
		case check.Bucket == "pending":
			pending = append(pending, check.Name)
		case required:
			return fmt.Errorf("同步 PR %s 不自动合并: 必需检查 %s 为 %s", branch, check.Name, check.Bucket)
		case check.Bucket == "skipping":
		default:
			return fmt.Errorf("同步 PR %s 不自动合并: 检查 %s 为 %s", branch, check.Name, check.Bucket)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("同步 PR %s %w: %s", branch, errSyncWaitingCI, strings.Join(pending, ", "))
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
