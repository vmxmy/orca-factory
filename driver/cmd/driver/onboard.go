package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func init() { Register("onboard", runOnboard) }

const (
	onboardBranch  = "feat/agent-graph"
	onboardTools   = "Bash,Read,Edit,Write,Grep,Glob"
	onboardTimeout = 2 * time.Hour
	// onboardSettings deny credential and state reads, environment dumps,
	// stash (shared across worktrees), force pushes, pushes to main and
	// merging: the onboarding PR is merged by a person.
	onboardSettings = `{"permissions":{"deny":["Read(~/.claude/**)","Read(~/.config/**)","Read(~/.local/state/**)","Read(~/.ssh/**)",` +
		`"Bash(env)","Bash(env:*)","Bash(printenv)","Bash(printenv:*)","Bash(git stash:*)","Bash(git push --force:*)","Bash(git push -f:*)",` +
		`"Bash(git push origin main:*)","Bash(git push origin HEAD:main:*)","Bash(gh pr merge:*)"]}}`
)

// onboardDefaultKeys are copied from the factory's own agent-graph.env into
// the onboarded repository's defaults; onboardPendingKeys only a person knows
// and stay empty, listed under 待确认 in the onboarding PR.
var (
	onboardDefaultKeys = []string{"AGENT_ISSUE_BACKEND", "AGENT_FEISHU_BASE", "AGENT_ROLE_INTAKE", "AGENT_TIER_FAST", "AGENT_TIER_DEFAULT", "AGENT_TIER_STRONG", "AGENT_TIER_ESCALATE", "AGENT_TIER_BY_ESTIMATE"}
	onboardPendingKeys = []string{"AGENT_DEPLOY_WORKFLOW", "AGENT_SMOKE_CMD", "AGENT_NOTIFY_FEISHU_PROFILE", "AGENT_NOTIFY_FEISHU_CHAT"}
	githubRemote       = regexp.MustCompile(`^github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)$`)
)

// onboardPlan is everything one onboarding will do, decided before it runs:
// `onboard --dry-run` prints it, and the real run executes it.
type onboardPlan struct {
	Repo     string            `json:"repo"`
	Remote   string            `json:"remote"`
	Name     string            `json:"name"`
	Prefix   string            `json:"prefix"`
	Branch   string            `json:"branch"`
	Worktree string            `json:"worktree"`
	Model    string            `json:"model"`
	Effort   string            `json:"effort"`
	Env      map[string]string `json:"env"`
	// RowExists is set when an earlier attempt already wrote this remote's
	// project-table row; the retry reuses its prefix and writes no new row.
	RowExists bool     `json:"row_exists"`
	Pending   []string `json:"pending"`
	Steps     []string `json:"steps"`
	// Preflight is OF-52's deterministic go/no-go judgments, filled in after
	// the plan itself: `onboard --dry-run` prints them and exits non-zero on
	// any failure; the real run (CLI or the picker card's 接入 button, which
	// share onboarder.run) refuses before writing the ledger row, creating
	// the worktree, or starting the onboarding agent when any fails.
	Preflight []preflightCheck `json:"preflight"`
}

// preflightCheck is one deterministic judgment an onboarding run must pass
// before it writes anything (OF-52). Detail is the fix a person applies on
// failure; it never carries a credential, environment variable value, or
// raw tool output — only key names, booleans and exit codes.
type preflightCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// preflightFuncs are onboarding's externally-checked preflight groups
// (OF-52): tools (a, reusing `orca-factory doctor --json` plus
// resolveClaude), factory (b, the factory's own install.sh --dry-run), repo
// (c, the target repository's push access) and scene (d, worktree/branch/
// lock residue). The fifth group, the ledger (e), is derived from the plan
// this onboarding already computed (a successful plan already proved the
// ledger readable with a non-conflicting prefix) and needs no check of its
// own. Each func takes the onboarder by value so a test can swap in a
// stand-in without running the real tool, while production wiring always
// sees the onboarder's current fields (claude, cfg, stateRoot).
type preflightFuncs struct {
	tools   func(ctx context.Context, o onboarder) preflightCheck
	factory func(ctx context.Context, o onboarder) preflightCheck
	repo    func(ctx context.Context, o onboarder, plan onboardPlan) preflightCheck
	scene   func(ctx context.Context, o onboarder, plan onboardPlan) preflightCheck
}

// defaultPreflight is OF-52's production preflight checks, wired at both
// onboarder construction sites (the onboard command and the picker card's
// 接入 button, which share onboarder.run). A test driving runOnboard as a
// black box swaps this package var for a stand-in and restores it after.
var defaultPreflight = preflightFuncs{tools: checkOnboardTools, factory: checkOnboardFactory, repo: checkOnboardRepoAccess, scene: checkOnboardScene}

// onboarder onboards one Orca repository (OF-50): the agent works only in a
// fresh worktree of it; the driver itself writes the ledger 项目 row.
type onboarder struct {
	cfg                 config.Config
	orca                *orca.Client
	claude              string
	stateRoot, cacheDir string
	writeRow            func(context.Context, issues.LedgerProject) (issues.LedgerProject, error)
	preflight           preflightFuncs
}

// onboardKey names a repository's onboarding lock and worktree directory.
func onboardKey(remote string) string { return strings.ReplaceAll(remote, "/", "_") }

// plan validates that repo is an onboardable repository in the live
// discovery result and derives its defaults from the factory's own profile.
func (o onboarder) plan(ctx context.Context, repo string) (onboardPlan, error) {
	if o.cfg.FactoryRoot == "" {
		return onboardPlan{}, errors.New(string(projects.ReasonFactoryRootUnconfigured))
	}
	repo, err := filepath.Abs(repo)
	if err != nil {
		return onboardPlan{}, err
	}
	result, rows, err := discoverProjects(ctx, o.cfg, o.orca)
	if err != nil {
		return onboardPlan{}, fmt.Errorf("项目发现失败：%w", err)
	}
	var target *projects.Rejection
	for i, r := range result.Rejected {
		if r.Path == repo {
			target = &result.Rejected[i]
		}
	}
	for _, p := range result.Eligible {
		if p.Path == repo {
			return onboardPlan{}, fmt.Errorf("%s 不是可接入仓库：已接入", repo)
		}
	}
	if target == nil {
		return onboardPlan{}, fmt.Errorf("%s 不在 Orca 仓库清单", repo)
	}
	if target.Reason != projects.ReasonOnboardable {
		return onboardPlan{}, fmt.Errorf("%s 不是可接入仓库：%s", repo, target.Reason)
	}
	m := githubRemote.FindStringSubmatch(target.Remote)
	if m == nil {
		return onboardPlan{}, fmt.Errorf("%s 的远端 %q 不是 GitHub 仓库", repo, target.Remote)
	}
	factory, err := config.LoadRepository(o.cfg.FactoryRoot)
	if err != nil {
		return onboardPlan{}, fmt.Errorf("factory 项目配置不可读：%w", err)
	}
	model, effort, err := tierParts(factory.Env)
	if err != nil {
		return onboardPlan{}, err
	}
	env := map[string]string{"AGENT_FEISHU_PROJECT": target.Remote, "AGENT_GH_REPO": m[1]}
	for _, key := range onboardDefaultKeys {
		if v := factory.Env[key]; v != "" {
			env[key] = v
		}
	}
	taken := make([]string, 0, len(rows))
	existing := ""
	for _, row := range rows {
		taken = append(taken, row.Prefix)
		if row.Remote == target.Remote {
			existing = row.Prefix
		}
	}
	name := filepath.Base(repo)
	prefix := existing
	if prefix == "" {
		prefix = projects.OnboardPrefix(name, taken)
	}
	rowStep := fmt.Sprintf("driver 在飞书项目表新增行：名称=%s 仓库远端=%s 编号前缀=%s 启用=false（先于接入 Agent，install.sh --dry-run 校验需要该行）", name, target.Remote, prefix)
	if existing != "" {
		rowStep = fmt.Sprintf("飞书项目表已有 %s 的行（编号前缀=%s），沿用不新增", target.Remote, prefix)
	}
	worktree := filepath.Join(o.cacheDir, "orca-factory", "onboard", onboardKey(target.Remote), "worktree")
	steps := []string{
		rowStep,
		fmt.Sprintf("git -C %s fetch origin main && git -C %s worktree add -b %s %s origin/main", repo, repo, onboardBranch, worktree),
		fmt.Sprintf("接入 Agent（claude %s:%s，工作目录 %s）运行 %s/bin/orca-factory init %s", model, effort, worktree, o.cfg.FactoryRoot, worktree),
		"接入 Agent 只读调研并填写 gate.project.sh、COORDINATOR.project.md、agent-graph.env（默认值见 env，pending 留空列入 PR 待确认）",
		"接入 Agent 跑 ONBOARD 5a～5d：gate.test.sh、install.sh --dry-run、gate 绿、注入错误 gate 红并撤干净",
		fmt.Sprintf("接入 Agent 推送 %s 并在 %s 开接入 PR（不合并）", onboardBranch, m[1]),
	}
	return onboardPlan{Repo: repo, Remote: target.Remote, Name: name, Prefix: prefix, Branch: onboardBranch, Worktree: worktree,
		Model: model, Effort: effort, Env: env, RowExists: existing != "", Pending: slices.Clone(onboardPendingKeys), Steps: steps}, nil
}

// run onboards req.Repo (OF-52: only after every preflight check passes,
// so a blocking tool, factory or target-repository problem never writes
// the ledger row, creates the worktree, or starts the onboarding agent)
// under its repository's single-instance lock, <state root>/onboard/<remote
// key>.lock, held across processes (the onboard command and the intake
// gateway) until the run ends.
func (o onboarder) run(ctx context.Context, req intake.OnboardRequest) (intake.OnboardResult, error) {
	plan, err := o.plan(ctx, req.Repo)
	if err != nil {
		return intake.OnboardResult{}, err
	}
	if plan.Remote != req.Remote {
		return intake.OnboardResult{}, fmt.Errorf("%s 的远端已变为 %s", req.Repo, plan.Remote)
	}
	if summary := failedPreflightSummary(o.runPreflight(ctx, plan)); summary != "" {
		return intake.OnboardResult{}, fmt.Errorf("接入前置检查未通过：%s", summary)
	}
	lock, err := flock.TryLock(filepath.Join(o.stateRoot, "onboard", onboardKey(req.Remote)+".lock"))
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return intake.OnboardResult{}, fmt.Errorf("%s 已有进行中的接入", req.Remote)
	}
	if err != nil {
		return intake.OnboardResult{}, fmt.Errorf("接入锁不可用：%w", err)
	}
	defer func() { _ = lock.Close() }()
	row := issues.LedgerProject{Name: plan.Name, Remote: plan.Remote, Prefix: plan.Prefix}
	if !plan.RowExists {
		if row, err = o.writeRow(ctx, row); err != nil {
			return intake.OnboardResult{}, fmt.Errorf("项目表写入失败：%w", err)
		}
	}
	if err := o.addWorktree(ctx, plan); err != nil {
		return intake.OnboardResult{}, err
	}
	pr, err := o.runAgent(ctx, plan)
	if err != nil {
		return intake.OnboardResult{}, err
	}
	return intake.OnboardResult{PR: pr, Row: row}, nil
}

// runPreflight runs OF-52's five preflight groups in order (a–e) and
// returns every result; it never stops at the first failure, so a person
// reading `onboard --dry-run` or a failed run's error sees every blocking
// problem at once instead of discovering the next one five minutes later.
func (o onboarder) runPreflight(ctx context.Context, plan onboardPlan) []preflightCheck {
	return []preflightCheck{
		o.preflight.tools(ctx, o),
		o.preflight.factory(ctx, o),
		o.preflight.repo(ctx, o, plan),
		o.preflight.scene(ctx, o, plan),
		ledgerPreflightCheck(plan),
	}
}

// failedPreflightSummary joins every failing check's name and detail, or
// "" when all passed.
func failedPreflightSummary(checks []preflightCheck) string {
	var failed []string
	for _, c := range checks {
		if !c.OK {
			failed = append(failed, c.Name+"："+c.Detail)
		}
	}
	return strings.Join(failed, "；")
}

// ledgerPreflightCheck is preflight group (e): the plan this onboarding
// already computed only exists because discoverProjects read the Feishu
// ledger's project table and resolved a prefix that does not conflict
// (reusing the remote's existing row, or generating a fresh one from the
// rows already taken) — a ledger failure surfaces as a plan() error before
// preflight ever runs, so by the time this executes the group is already
// proven.
func ledgerPreflightCheck(plan onboardPlan) preflightCheck {
	detail := "飞书项目表已读取，编号前缀 " + plan.Prefix + " 不冲突"
	if plan.RowExists {
		detail += "（沿用该远端已有行）"
	} else {
		detail += "（新行）"
	}
	return preflightCheck{Name: "账本", OK: true, Detail: detail}
}

// exitCode reports a failed runx.Run's process exit code, or -1 when the
// command never produced one (lookup failure, timeout).
func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// doctorReport is the subset of `orca-factory doctor --json` preflight (a)
// reads: only booleans, never raw tool output or credentials.
type doctorReport struct {
	Tools map[string]struct {
		Present  bool `json:"present"`
		Ready    bool `json:"ready"`
		LoggedIn bool `json:"logged_in"`
	} `json:"tools"`
}

// checkOnboardTools is preflight group (a): reuses the factory's own
// `doctor --json` for orca (running), gh (logged in), git and jq, plus
// resolveClaude for the onboarding agent's binary.
func checkOnboardTools(ctx context.Context, o onboarder) preflightCheck {
	const name = "本机工具"
	out, err := runx.Run(ctx, runx.Spec{Path: filepath.Join(o.cfg.FactoryRoot, "bin", "orca-factory"), Args: []string{"doctor", "--json"}, Timeout: 30 * time.Second})
	if err != nil {
		return preflightCheck{Name: name, OK: false, Detail: fmt.Sprintf("orca-factory doctor --json 退出码 %d；补法：手动运行 orca-factory doctor 排查", exitCode(err))}
	}
	var report doctorReport
	if json.Unmarshal(out, &report) != nil {
		return preflightCheck{Name: name, OK: false, Detail: "orca-factory doctor --json 输出不可解析；补法：手动运行 orca-factory doctor 排查"}
	}
	var missing []string
	if t := report.Tools["orca"]; !t.Present || !t.Ready {
		missing = append(missing, "orca")
	}
	if t := report.Tools["gh"]; !t.Present || !t.LoggedIn {
		missing = append(missing, "gh")
	}
	if !report.Tools["git"].Present {
		missing = append(missing, "git")
	}
	if !report.Tools["jq"].Present {
		missing = append(missing, "jq")
	}
	claude := o.claude
	if claude == "" {
		claude, err = resolveClaude(os.Getenv("AGENT_INTAKE_CLAUDE"))
	}
	if claude == "" || err != nil {
		missing = append(missing, "claude")
	}
	if len(missing) > 0 {
		return preflightCheck{Name: name, OK: false, Detail: "未就绪：" + strings.Join(missing, ",") + "；补法：运行 orca-factory doctor 核对并修复"}
	}
	return preflightCheck{Name: name, OK: true, Detail: "orca 运行中、gh 已登录、git/jq 就绪、claude 可解析"}
}

// checkOnboardFactory is preflight group (b): the factory's own install.sh
// --dry-run, run in factory_root, must exit 0.
func checkOnboardFactory(ctx context.Context, o onboarder) preflightCheck {
	const name = "factory 自身"
	script := filepath.Join(o.cfg.FactoryRoot, "scripts/agent-gates/install.sh")
	if _, err := runx.Run(ctx, runx.Spec{Path: script, Args: []string{"--dry-run"}, Dir: o.cfg.FactoryRoot, Timeout: 2 * time.Minute}); err != nil {
		return preflightCheck{Name: name, OK: false, Detail: fmt.Sprintf("install.sh --dry-run 退出码 %d；补法：核对 factory 自身 agent-graph.env 与账本后端配置", exitCode(err))}
	}
	return preflightCheck{Name: name, OK: true, Detail: "install.sh --dry-run 退出码 0"}
}

// checkOnboardRepoAccess is preflight group (c): the target repository's
// GitHub remote (already confirmed by plan()) must give this machine push
// access, and its main branch must be fetchable.
func checkOnboardRepoAccess(ctx context.Context, o onboarder, plan onboardPlan) preflightCheck {
	const name = "目标仓库"
	m := githubRemote.FindStringSubmatch(plan.Remote)
	if m == nil {
		return preflightCheck{Name: name, OK: false, Detail: "远端不是 GitHub 仓库"}
	}
	out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"api", "repos/" + m[1]}, Timeout: 30 * time.Second})
	if err != nil {
		return preflightCheck{Name: name, OK: false, Detail: fmt.Sprintf("gh api repos/%s 退出码 %d；补法：确认 gh 已登录且有该仓库访问权限", m[1], exitCode(err))}
	}
	var repo struct {
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if json.Unmarshal(out, &repo) != nil || !repo.Permissions.Push {
		return preflightCheck{Name: name, OK: false, Detail: "gh api repos/" + m[1] + " 的 permissions.push 不为 true；补法：申请该仓库的 push 权限"}
	}
	if _, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", plan.Repo, "ls-remote", "--exit-code", "origin", "main"}, Timeout: 30 * time.Second}); err != nil {
		return preflightCheck{Name: name, OK: false, Detail: fmt.Sprintf("git ls-remote origin main 退出码 %d；补法：确认本机能访问该仓库远端", exitCode(err))}
	}
	return preflightCheck{Name: name, OK: true, Detail: "gh 有 push 权限，origin main 可达"}
}

// checkOnboardScene is preflight group (d): the onboarding worktree path,
// the feat/agent-graph branch (local and remote), and the onboarding lock
// must all be clear — a residue from an earlier attempt is a person's job
// to clean up, never this run's.
func checkOnboardScene(ctx context.Context, o onboarder, plan onboardPlan) preflightCheck {
	const name = "现场"
	var problems []string
	if _, err := os.Stat(plan.Worktree); !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, "worktree 已存在")
	}
	if _, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", plan.Repo, "show-ref", "--quiet", "refs/heads/" + plan.Branch}, Timeout: 10 * time.Second}); err == nil {
		problems = append(problems, "本地分支 "+plan.Branch+" 已存在")
	}
	if out, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", plan.Repo, "ls-remote", "--heads", "origin", plan.Branch}, Timeout: 30 * time.Second}); err == nil && strings.TrimSpace(string(out)) != "" {
		problems = append(problems, "远端分支 "+plan.Branch+" 已存在")
	}
	if lock, err := flock.TryLock(filepath.Join(o.stateRoot, "onboard", onboardKey(plan.Remote)+".lock")); err != nil {
		problems = append(problems, "接入锁被占用：已有进行中的接入")
	} else {
		_ = lock.Close()
	}
	if len(problems) > 0 {
		return preflightCheck{Name: name, OK: false, Detail: strings.Join(problems, "；") + "；补法：清理残留 worktree/分支，或等待进行中的接入结束"}
	}
	return preflightCheck{Name: name, OK: true, Detail: "worktree 与分支均不存在，接入锁空闲"}
}

// addWorktree creates the agent's working directory; OF-52's 现场 preflight
// check already proved the path and branch are clear before run() ever
// reaches here.
func (o onboarder) addWorktree(ctx context.Context, plan onboardPlan) error {
	if err := os.MkdirAll(filepath.Dir(plan.Worktree), 0o700); err != nil {
		return err
	}
	if _, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", plan.Repo, "fetch", "--quiet", "origin", "main"}, Timeout: 2 * time.Minute}); err != nil {
		return errors.New("目标仓库 fetch origin main 失败")
	}
	if _, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", plan.Repo, "worktree", "add", "--quiet", "-b", plan.Branch, plan.Worktree, "origin/main"}, Timeout: 2 * time.Minute}); err != nil {
		return fmt.Errorf("创建接入 worktree 失败（分支 %s 可能已存在）", plan.Branch)
	}
	return nil
}

// runAgent runs the onboarding agent in plan.Worktree and returns the PR it
// reports. Its environment carries only HOME and PATH, plus the session bus
// gh's keyring login needs: no Feishu app or other credentials.
func (o onboarder) runAgent(ctx context.Context, plan onboardPlan) (string, error) {
	args := []string{"-p", onboardPrompt(plan, o.cfg.FactoryRoot), "--output-format", "json"}
	if plan.Model != "" {
		args = append(args, "--model", plan.Model)
	}
	if plan.Effort != "" {
		args = append(args, "--effort", plan.Effort)
	}
	args = append(args, "--append-system-prompt-file", filepath.Join(o.cfg.FactoryRoot, "scripts/agent-gates/roles/onboard.md"),
		"--tools", onboardTools, "--allowedTools", onboardTools, "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config", "--settings", onboardSettings)
	env := []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	for _, key := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	out, err := runx.Run(ctx, runx.Spec{Path: o.claude, Args: args, Dir: plan.Worktree, Env: env, Timeout: onboardTimeout})
	if err != nil {
		return "", fmt.Errorf("接入 Agent 失败：%w", err)
	}
	var result struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if json.Unmarshal(out, &result) != nil || result.Type != "result" || result.Subtype != "success" || result.IsError {
		return "", errors.New("接入 Agent 没有成功结果")
	}
	prs := regexp.MustCompile(`https://github\.com/`+regexp.QuoteMeta(plan.Env["AGENT_GH_REPO"])+`/pull/[0-9]+`).FindAllString(result.Result, -1)
	if len(prs) == 0 {
		return "", fmt.Errorf("接入 Agent 未报告接入 PR，Agent 最终说明：%s", truncateOnboardResult(result.Result))
	}
	return prs[len(prs)-1], nil
}

// onboardResultExcerptLimit bounds how much of the agent's final result text
// goes into an error a person reads (CLI stderr or the card's topic reply).
const onboardResultExcerptLimit = 2000

// truncateOnboardResult keeps a failed onboarding's agent explanation
// readable instead of discarding it.
func truncateOnboardResult(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) > onboardResultExcerptLimit {
		return string(runes[:onboardResultExcerptLimit]) + "…"
	}
	return s
}

func onboardPrompt(plan onboardPlan, factoryRoot string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "按 roles/onboard.md 接入目标仓库。\n工作目录（目标仓库独立 worktree，分支 %s，基于 origin/main）：%s\n", plan.Branch, plan.Worktree)
	fmt.Fprintf(&b, "目标仓库主检出（只读）：%s\norca-factory 根目录：%s\n仓库远端：%s\n编号前缀（driver 写项目表用，已定）：%s\n", plan.Repo, factoryRoot, plan.Remote, plan.Prefix)
	b.WriteString("agent-graph.env 默认值：\n")
	for _, key := range append([]string{"AGENT_GH_REPO", "AGENT_FEISHU_PROJECT"}, onboardDefaultKeys...) {
		if v, ok := plan.Env[key]; ok {
			fmt.Fprintf(&b, "%s=%s\n", key, v)
		}
	}
	fmt.Fprintf(&b, "留空并列入 PR 正文「待确认」：%s\n", strings.Join(plan.Pending, " "))
	fmt.Fprintf(&b, "完成后最后一行只输出接入 PR 链接 https://github.com/%s/pull/<号>。\n", plan.Env["AGENT_GH_REPO"])
	return b.String()
}

// factoryLedgerRow writes an onboarded repository's 项目 row through the
// factory's own Feishu ledger profile.
func factoryLedgerRow(cfg config.Config) func(context.Context, issues.LedgerProject) (issues.LedgerProject, error) {
	return func(ctx context.Context, row issues.LedgerProject) (issues.LedgerProject, error) {
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
		return ledger.CreateLedgerProject(ctx, row)
	}
}

// resolveClaude resolves the claude binary for the intake and onboarding
// agents: AGENT_INTAKE_CLAUDE, else claude on PATH.
func resolveClaude(configured string) (string, error) {
	if configured == "" {
		configured, _ = exec.LookPath("claude")
	}
	if !filepath.IsAbs(configured) {
		return "", errors.New("AGENT_INTAKE_CLAUDE must be an absolute path to claude")
	}
	return configured, nil
}

func runOnboard(args []string) int {
	flags := flag.NewFlagSet("onboard", flag.ContinueOnError)
	repo := flags.String("repo", "", "目标仓库本机检出路径")
	dryRun := flags.Bool("dry-run", false, "只打印步骤与默认值")
	if flags.Parse(args) != nil || *repo == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: orca-factory-driver onboard --repo <path> [--dry-run]")
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
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	o := onboarder{cfg: cfg, orca: &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout}, stateRoot: logStateRoot(), cacheDir: cacheDir, writeRow: factoryLedgerRow(cfg), preflight: defaultPreflight}
	plan, err := o.plan(ctx, *repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *dryRun {
		plan.Preflight = o.runPreflight(ctx, plan)
		if err := json.NewEncoder(os.Stdout).Encode(plan); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if failedPreflightSummary(plan.Preflight) != "" {
			return 1
		}
		return 0
	}
	if o.claude, err = resolveClaude(os.Getenv("AGENT_INTAKE_CLAUDE")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	result, err := o.run(ctx, intake.OnboardRequest{Repo: plan.Repo, Remote: plan.Remote})
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
