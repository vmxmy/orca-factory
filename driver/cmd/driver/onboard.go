package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	onboardDefaultKeys = []string{"AGENT_ISSUE_BACKEND", "AGENT_FEISHU_BASE", "AGENT_ROLE_INTAKE", "AGENT_TIER_DEFAULT", "AGENT_TIER_STRONG", "AGENT_TIER_ESCALATE"}
	onboardPendingKeys = []string{"AGENT_DEPLOY_WORKFLOW", "AGENT_SMOKE_CMD", "AGENT_FEISHU_NOTIFY_CHAT"}
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
	// Resume is set only by the wizard when deriveStage said Resume: the
	// scene check then accepts our own leftover worktree and session skips
	// addWorktree. The gateway never sets it, so its check stays exactly as
	// strict as before (OF-197).
	Resume bool `json:"-"`
	// Preflight is OF-52's deterministic go/no-go judgments, filled in after
	// the plan itself: `onboard --dry-run` prints them and exits non-zero on
	// any failure; the real run (the wizard's session stage or the picker
	// card's 接入 button) refuses before writing the ledger row, creating
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
// own. OF-131 adds service (f, the driver service's PATH) and protection (g,
// the target repository's main branch protection). Each func takes the onboarder by value so a test can swap in a
// stand-in without running the real tool, while production wiring always
// sees the onboarder's current fields (claude, cfg, stateRoot).
type preflightFuncs struct {
	tools      func(ctx context.Context, o onboarder) preflightCheck
	factory    func(ctx context.Context, o onboarder) preflightCheck
	repo       func(ctx context.Context, o onboarder, plan onboardPlan) preflightCheck
	scene      func(ctx context.Context, o onboarder, plan onboardPlan) preflightCheck
	service    func(ctx context.Context, o onboarder) preflightCheck
	protection func(ctx context.Context, o onboarder, plan onboardPlan) preflightCheck
}

// defaultPreflight is OF-52's production preflight checks, wired at both
// onboarder construction sites (the onboard wizard and the picker card's
// 接入 button). A test driving runOnboard as a
// black box swaps this package var for a stand-in and restores it after.
var defaultPreflight = preflightFuncs{tools: checkOnboardTools, factory: checkOnboardFactory, repo: checkOnboardRepoAccess, scene: checkOnboardScene,
	service: checkOnboardServicePath, protection: checkOnboardBranchProtection}

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

// onboardWorktree is the single source of the onboarding worktree path:
// plan(), enable's cleanup and the wizard's scene probe all read it here, so
// the path one run created is exactly the path the next run resumes.
func onboardWorktree(cacheDir, remote string) string {
	return filepath.Join(cacheDir, "orca-factory", "onboard", onboardKey(remote), "worktree")
}

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
	worktree := onboardWorktree(o.cacheDir, target.Remote)
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

// run is the picker card's 接入 path: it onboards req.Repo through session
// only after every preflight check passes (OF-52), so a blocking tool,
// factory or target-repository problem never writes the ledger row, creates
// the worktree, or starts the onboarding agent.
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
	return o.session(ctx, plan, o.runAgent)
}

// session is run()'s write tail under the repository's single-instance lock,
// <state root>/onboard/<remote key>.lock, held across processes (the gateway
// and the wizard) until the agent returns: row write (skipped when the plan
// reuses one), worktree (skipped when plan.Resume continues ours), then the
// agent. The wizard passes its own agent and discards the returned PR — its
// PR fact comes only from the gh read-back in observe.
func (o onboarder) session(ctx context.Context, plan onboardPlan, agent func(context.Context, onboardPlan) (string, error)) (intake.OnboardResult, error) {
	lock, err := flock.TryLock(filepath.Join(o.stateRoot, "onboard", onboardKey(plan.Remote)+".lock"))
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return intake.OnboardResult{}, fmt.Errorf("%s 已有进行中的接入", plan.Remote)
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
	if !plan.Resume {
		if err := o.addWorktree(ctx, plan); err != nil {
			return intake.OnboardResult{}, err
		}
	}
	pr, err := agent(ctx, plan)
	if err != nil {
		return intake.OnboardResult{}, err
	}
	return intake.OnboardResult{PR: pr, Row: row}, nil
}

// runPreflight runs every preflight group in order (OF-52's a–e, OF-131's f–g) and
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
		o.preflight.service(ctx, o),
		o.preflight.protection(ctx, o, plan),
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
	if ok, detail := remoteIdentityCheck(ctx, plan.Repo, plan.Remote); !ok {
		return preflightCheck{Name: name, OK: false, Detail: detail}
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
	return preflightCheck{Name: name, OK: true, Detail: "gh 有 push 权限，origin main 可达，Orca 标识与 origin 一致"}
}

// remoteIdentityCheck is OF-56's half of preflight group (c): Orca caches a
// repository's canonicalKey at registration and never refreshes it when the
// local origin changes later, so project discovery, AGENT_GH_REPO and the
// onboarding PR target can all silently drift to a stale remote. It compares
// plan.Remote (the canonicalKey discovery already confirmed) against a live
// `git -C path remote get-url origin`, normalized the same way (OF-56:
// orca.NormalizeRemote). ok is false whenever agreement cannot be
// confirmed — the git call failing, the URL not normalizing, or the two
// normalized values differing — and detail never carries the raw URL, only
// the two normalized values or a generic reason, so a URL embedding
// credentials never reaches a person.
func remoteIdentityCheck(ctx context.Context, path, canonicalKey string) (ok bool, detail string) {
	out, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", path, "remote", "get-url", "origin"}, Timeout: 10 * time.Second})
	if err != nil {
		return false, fmt.Sprintf("git remote get-url origin 退出码 %d；补法：确认仓库已配置 origin 远端", exitCode(err))
	}
	live, err := orca.NormalizeRemote(string(out))
	if err != nil {
		return false, "origin 远端 URL 无法规范化；补法：核对 origin 远端配置"
	}
	if live != canonicalKey {
		return false, fmt.Sprintf("Orca 标识 %s 与 origin 规范化后的 %s 不一致；补法：重启 Orca 或在 Orca 中重新登记该仓库", canonicalKey, live)
	}
	return true, "Orca 标识与 origin 一致：" + canonicalKey
}

// checkOnboardScene is preflight group (d): the scene must be clear — or,
// only when the wizard set plan.Resume (OF-197), this machine's own leftover
// worktree counts as ours to continue. classifyScene is the same interpreter
// deriveStage uses, so the two can never disagree on what residue is. Any
// other residue is a person's job to clean up, never this run's; the lock
// must be free either way.
func checkOnboardScene(ctx context.Context, o onboarder, plan onboardPlan) preflightCheck {
	const name = "现场"
	probe, err := probeScene(ctx, o, plan.Repo, plan.Remote)
	if err != nil {
		return preflightCheck{Name: name, OK: false, Detail: err.Error()}
	}
	problems := ""
	if scene := classifyScene(probe); scene != sceneClear && (!plan.Resume || scene != sceneOurs) {
		problems = sceneResidueDetail(probe)
	}
	if lock, err := flock.TryLock(filepath.Join(o.stateRoot, "onboard", onboardKey(plan.Remote)+".lock")); err != nil {
		busy := "接入锁被占用：已有进行中的接入"
		if problems != "" {
			problems += "；" + busy
		} else {
			problems = busy
		}
	} else {
		_ = lock.Close()
	}
	if problems != "" {
		return preflightCheck{Name: name, OK: false, Detail: problems + "；补法：清理残留 worktree/分支，或等待进行中的接入结束"}
	}
	if plan.Resume {
		return preflightCheck{Name: name, OK: true, Detail: "续接本机接入 worktree，接入锁空闲"}
	}
	return preflightCheck{Name: name, OK: true, Detail: "worktree 与分支均不存在，接入锁空闲"}
}

// onboardServiceTools are the binaries the driver service's gates, merge
// lane and executor run from its own PATH; a drop-in that drops one (the
// linuxbrew gh once) only surfaces later as a failed ticket. orca is checked
// separately because runx.OrcaPath prefers AGENT_ORCA_BIN over PATH.
var onboardServiceTools = []string{"go", "gh", "git"}

// checkOnboardServicePath is preflight group (f): every onboardServiceTools
// entry must resolve on orca-factory-driver.service's effective PATH, and orca
// must be executable at AGENT_ORCA_BIN when the service sets it, else on PATH.
// `systemctl --user show -p Environment` reports the environment after merging
// the unit and its drop-ins. Only PATH and AGENT_ORCA_BIN are read; other
// values never reach detail.
func checkOnboardServicePath(ctx context.Context, _ onboarder) preflightCheck {
	const name = "服务环境"
	const reload = "，然后 systemctl --user daemon-reload 并重启服务"
	out, err := runx.Run(ctx, runx.Spec{Path: "systemctl", Args: []string{"--user", "show", "-p", "Environment", "orca-factory-driver.service"}, Timeout: 10 * time.Second})
	if errors.Is(err, exec.ErrNotFound) {
		return preflightCheck{Name: name, OK: false, Detail: "找不到 systemctl；补法：在运行 driver 服务的 systemd 主机上执行预检"}
	}
	if err != nil {
		// show exits 0 for a unit that is not installed, so a failure here is
		// the user instance itself being unreachable.
		return preflightCheck{Name: name, OK: false, Detail: fmt.Sprintf("无法连接 systemd user 实例（systemctl --user 退出码 %d）；补法：在该用户的登录会话中运行预检，或确认 XDG_RUNTIME_DIR 指向 /run/user/<uid> 且已执行 loginctl enable-linger", exitCode(err))}
	}
	var dirs []string
	var orcaBin string
	for _, field := range strings.Fields(strings.TrimPrefix(strings.TrimSpace(string(out)), "Environment=")) {
		if value, ok := strings.CutPrefix(field, "PATH="); ok {
			dirs = filepath.SplitList(value)
		}
		if value, ok := strings.CutPrefix(field, "AGENT_ORCA_BIN="); ok {
			orcaBin = value
		}
	}
	if len(dirs) == 0 {
		return preflightCheck{Name: name, OK: false, Detail: "orca-factory-driver.service 未设置 PATH；补法：在该服务的 drop-in 中设置 Environment=PATH" + reload}
	}
	onPath := func(tool string) bool {
		return slices.ContainsFunc(dirs, func(dir string) bool { return isExecutableFile(filepath.Join(dir, tool)) })
	}
	var missing []string
	for _, tool := range onboardServiceTools {
		if !onPath(tool) {
			missing = append(missing, tool)
		}
	}
	if orcaBin == "" && !onPath("orca") {
		missing = append(missing, "orca")
	}
	if len(missing) > 0 {
		return preflightCheck{Name: name, OK: false, Detail: "orca-factory-driver.service 的 PATH 找不到：" + strings.Join(missing, ",") + "；补法：在该服务的 drop-in 中把这些工具所在目录加入 Environment=PATH" + reload}
	}
	if orcaBin != "" && !isExecutableFile(orcaBin) {
		return preflightCheck{Name: name, OK: false, Detail: "orca-factory-driver.service 的 AGENT_ORCA_BIN=" + orcaBin + " 不可执行；补法：在该服务的 drop-in 中把 Environment=AGENT_ORCA_BIN 指向可执行的 orca" + reload}
	}
	return preflightCheck{Name: name, OK: true, Detail: "orca-factory-driver.service 能找到 " + strings.Join(onboardServiceTools, ",") + ",orca"}
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// checkOnboardBranchProtection is preflight group (g): the target
// repository's main must require at least one status check, through classic
// branch protection or a ruleset, because the merge lane only squashes once a
// nonempty set of checks succeeded at the pinned head. Both endpoints need
// only read access (the /protection endpoint needs admin), and the check
// never changes repository settings.
func checkOnboardBranchProtection(ctx context.Context, _ onboarder, plan onboardPlan) preflightCheck {
	const name = "分支保护"
	m := githubRemote.FindStringSubmatch(plan.Remote)
	if m == nil {
		return preflightCheck{Name: name, OK: false, Detail: "远端不是 GitHub 仓库"}
	}
	var branch struct {
		Protected  bool `json:"protected"`
		Protection struct {
			RequiredStatusChecks struct {
				EnforcementLevel string   `json:"enforcement_level"`
				Contexts         []string `json:"contexts"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	for _, read := range []struct {
		endpoint string
		into     any
	}{{"repos/" + m[1] + "/branches/main", &branch}, {"repos/" + m[1] + "/rules/branches/main", &rules}} {
		out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"api", read.endpoint}, Timeout: 30 * time.Second})
		if err != nil {
			return preflightCheck{Name: name, OK: false, Detail: fmt.Sprintf("gh api %s 退出码 %d；补法：确认 gh 已登录且能读取该仓库", read.endpoint, exitCode(err))}
		}
		if json.Unmarshal(out, read.into) != nil {
			return preflightCheck{Name: name, OK: false, Detail: "gh api " + read.endpoint + " 输出不可解析；补法：手动运行该命令排查"}
		}
	}
	var required []string
	if classic := branch.Protection.RequiredStatusChecks; branch.Protected && classic.EnforcementLevel != "off" {
		required = append(required, classic.Contexts...)
	}
	for _, rule := range rules {
		if rule.Type == "required_status_checks" {
			for _, check := range rule.Parameters.RequiredStatusChecks {
				required = append(required, check.Context)
			}
		}
	}
	slices.Sort(required)
	required = slices.Compact(required)
	if len(required) == 0 {
		return preflightCheck{Name: name, OK: false, Detail: m[1] + " 的 main 没有必需检查，合并通道等不到成功检查无法 squash；补法：在 main 的分支保护中开启 Require status checks 并选中 CI 作业，或为 main 建 Ruleset 并开启 Require status checks to pass"}
	}
	return preflightCheck{Name: name, OK: true, Detail: "main 必需检查：" + strings.Join(required, ",")}
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

// runAgent runs the unattended onboarding agent (the gateway's `claude -p`)
// in plan.Worktree and returns the PR it reports.
func (o onboarder) runAgent(ctx context.Context, plan onboardPlan) (string, error) {
	out, err := runx.Run(ctx, runx.Spec{Path: o.claude, Args: onboardAgentArgs(plan, o.cfg.FactoryRoot, false), Dir: plan.Worktree, Env: onboardAgentEnv(false), Timeout: onboardTimeout})
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

// onboardAgentArgs is the single source of the onboarding agent's argv in
// both modes (OF-201). The prompt comes before every option because
// --add-dir, --tools and --allowedTools take variadic values that would
// swallow a trailing positional prompt.
func onboardAgentArgs(plan onboardPlan, root string, attended bool) []string {
	prompt := onboardPrompt(plan, root, attended)
	args, tools, mode := []string{"-p", prompt, "--output-format", "json"}, onboardTools, "dontAsk"
	if attended {
		// dontAsk denies AskUserQuestion; default lets the person answer it.
		args, tools, mode = []string{prompt}, onboardTools+",AskUserQuestion", "default"
	}
	if plan.Model != "" {
		args = append(args, "--model", plan.Model)
	}
	if plan.Effort != "" {
		args = append(args, "--effort", plan.Effort)
	}
	return append(args, "--append-system-prompt-file", filepath.Join(root, "scripts/agent-gates/roles/onboard.md"), "--add-dir", root,
		"--tools", tools, "--allowedTools", tools, "--permission-mode", mode, "--restricted", "--strict-mcp-config", "--settings", onboardSettings)
}

// onboardAgentEnv is the onboarding agent's whole environment: HOME and PATH,
// plus the session bus gh's keyring login needs, and for the attended
// session the terminal and locale its TUI needs. Never the Feishu app or any
// other credential.
func onboardAgentEnv(attended bool) []string {
	env := []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	optional := []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}
	if attended {
		env = append(env, "TERM="+os.Getenv("TERM"), "LANG="+os.Getenv("LANG"))
		optional = append(optional, "LC_ALL", "COLORTERM")
	}
	for _, key := range optional {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func onboardPrompt(plan onboardPlan, factoryRoot string, attended bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "按 roles/onboard.md 接入目标仓库。\n工作目录（目标仓库独立 worktree，分支 %s，基于 origin/main）：%s\n", plan.Branch, plan.Worktree)
	if attended {
		b.WriteString("交互模式：人在终端前，按角色文件的交互模式条款先调研再请人确认；下列待确认键人给了值就写入。\n")
	}
	if plan.Resume {
		b.WriteString("续接：工作目录里可能已有上一轮的提交与产物，先按角色文件的续接条款判断从哪一步继续，不重做已有工作。\n")
	}
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
