package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
)

// runOnboard is the onboard command (OF-197): the terminal wizard over one
// repository's onboarding lifecycle, entering at the first stage its external
// facts still leave unfinished. `--repo` skips the pick list; `--repo` with
// `--dry-run` keeps printing the JSON plan and exits. The headless run the
// CLI had before is gone: unattended onboarding is the Feishu card's path
// through onboarder.run, which shares only session() with the wizard.
func runOnboard(args []string) int {
	flags := flag.NewFlagSet("onboard", flag.ContinueOnError)
	repo := flags.String("repo", "", "目标仓库本机检出路径")
	dryRun := flags.Bool("dry-run", false, "只打印步骤与默认值")
	if flags.Parse(args) != nil || (*repo == "" && *dryRun) || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: orca-factory-driver onboard [--repo <path>] [--dry-run]")
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
	// No signal handling here: at a line prompt every signal keeps its default
	// kill and exits at once, since nothing is held then. wizard.session
	// catches them only while the agent runs.
	ctx := context.Background()
	o := onboarder{cfg: cfg, orca: &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout}, stateRoot: logStateRoot(), cacheDir: cacheDir, writeRow: factoryLedgerRow(cfg), preflight: defaultPreflight}
	if *dryRun {
		plan, err := o.plan(ctx, *repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
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
	w := wizard{o: o, in: bufio.NewReader(os.Stdin)}
	if w.o.claude, err = resolveClaude(os.Getenv("AGENT_INTAKE_CLAUDE")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	target := *repo
	if target == "" {
		result, _, err := discoverProjects(ctx, cfg, o.orca)
		if err != nil {
			fmt.Fprintln(os.Stderr, fmt.Errorf("项目发现失败：%w", err))
			return 1
		}
		picked, ok := w.pick(result)
		if !ok {
			return 0
		}
		target = picked
	}
	return w.loop(ctx, target)
}

// wizard is the terminal loop: observe → deriveStage → act → re-observe.
// I/O is bufio line prompts on os.Stdin/os.Stdout only; every stage decision
// comes from external facts, so an interrupted wizard is resumed by running
// it again, never by a progress file.
type wizard struct {
	o  onboarder
	in *bufio.Reader
}

// pick lists every discovered repository a person could onboard now — 可接入
// (never onboarded) and 项目未启用 (onboarded, not yet enabled) — and returns
// the chosen checkout path.
func (w wizard) pick(result projects.Result) (string, bool) {
	type candidate struct {
		path, name, remote, label string
	}
	var candidates []candidate
	for _, r := range result.Rejected {
		switch r.Reason {
		case projects.ReasonOnboardable:
			candidates = append(candidates, candidate{path: r.Path, name: filepath.Base(r.Path), remote: r.Remote, label: "可接入"})
		case projects.ReasonProjectDisabled:
			candidates = append(candidates, candidate{path: r.Path, name: filepath.Base(r.Path), remote: r.Remote, label: "待启用"})
		}
	}
	if len(candidates) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "没有可接入或待启用的仓库。")
		return "", false
	}
	_, _ = fmt.Fprintln(os.Stdout, "发现 Orca 仓库…")
	for i, c := range candidates {
		_, _ = fmt.Fprintf(os.Stdout, "  %d  %-24s %s  %s\n", i+1, c.name, c.path, c.label)
	}
	for {
		line, ok := w.prompt("选择编号（q 退出）")
		if !ok || line == "q" || line == "Q" {
			return "", false
		}
		n := 0
		if _, err := fmt.Sscanf(line, "%d", &n); err != nil || n < 1 || n > len(candidates) {
			_, _ = fmt.Fprintf(os.Stdout, "无效编号 %q\n", line)
			continue
		}
		return candidates[n-1].path, true
	}
}

// loop drives one repository until a terminal stage: Done exits 0, Blocked
// prints its reason and exits 1, and a q at a stage prompt exits that stage's
// own code.
func (w wizard) loop(ctx context.Context, repo string) int {
	for {
		facts, err := w.o.observe(ctx, repo)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stdout, err)
			return 1
		}
		verdict := deriveStage(facts)
		switch verdict.Stage {
		case stageDone:
			_, _ = fmt.Fprintf(os.Stdout, "完成：%s 已启用，driver 下次发现时接管。\n", facts.Remote)
			return 0
		case stageBlocked:
			_, _ = fmt.Fprintf(os.Stdout, "[受阻] %s\n", verdict.Why)
			return 1
		case stageWaitMerge:
			if code := w.waitMerge(facts, verdict.PR); code >= 0 {
				return code
			}
		case stageEnable:
			if code := w.enable(ctx, facts); code >= 0 {
				return code
			}
		case stageStart, stageResume:
			again, code := w.session(ctx, repo, verdict.Stage == stageResume)
			if code >= 0 {
				return code
			}
			if !again {
				return 1
			}
		}
	}
}

// prompt prints one line prompt and reads a line; ok is false on EOF. An
// empty line returns "" (the Enter-continue answers), anything else is
// trimmed.
func (w wizard) prompt(text string) (string, bool) {
	_, _ = fmt.Fprintf(os.Stdout, "%s：", text)
	line, err := w.in.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

// confirm prints text, then waits for Enter (ok) or q (not ok, exit 0). Any
// other answer re-prompts; EOF exits 1, since nothing was confirmed.
func (w wizard) confirm(text string) (ok bool, quitCode int) {
	for {
		line, readable := w.prompt(text + "（回车继续，q 退出）")
		if !readable {
			return false, 1
		}
		if line == "q" || line == "Q" {
			return false, 0
		}
		if line == "" {
			return true, 0
		}
		_, _ = fmt.Fprintf(os.Stdout, "输入回车或 q。\n")
	}
}

// session runs the agent stage once: the 7 preflight checks, a summary of
// what the session will do, then session() with the headless agent. After
// the agent returns, the caller re-observes; still no PR means the run ends
// with the resume hint instead of restarting the agent.
func (w wizard) session(ctx context.Context, repo string, resume bool) (again bool, code int) {
	plan, err := w.o.plan(ctx, repo)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stdout, err)
		return false, 1
	}
	plan.Resume = resume
	for {
		checks := w.o.runPreflight(ctx, plan)
		_, _ = fmt.Fprintf(os.Stdout, "\n[预检] %s\n", plan.Remote)
		for _, c := range checks {
			status := "ok  "
			if !c.OK {
				status = "FAIL"
			}
			_, _ = fmt.Fprintf(os.Stdout, "  %s  %s  %s\n", status, c.Name, c.Detail)
		}
		if failedPreflightSummary(checks) == "" {
			break
		}
		line, readable := w.prompt("修好后回车重查（q 退出）")
		if !readable || line == "q" || line == "Q" {
			return false, 1
		}
	}
	if resume {
		_, _ = fmt.Fprintf(os.Stdout, "\n[接入会话·续接] worktree：%s（分支 %s，沿用现有提交）\n", plan.Worktree, plan.Branch)
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "\n[接入会话] 项目表写行：%s %s 启用=false；worktree：%s（分支 %s，基于 origin/main）\n", plan.Name, plan.Prefix, plan.Worktree, plan.Branch)
	}
	if ok, code := w.confirm("进入接入会话"); !ok {
		return false, code
	}
	_, _ = fmt.Fprintln(os.Stdout, "无人值守接入 Agent 运行中，最长 2h，Ctrl-C 中断。")
	// The agent runs in its own process group (runx), out of reach of the
	// terminal's Ctrl-C and of a kill aimed at the driver. Cancelling agentCtx
	// is what makes runx kill that group; a default-killed driver would orphan
	// the agent and free the lock.
	agentCtx, stopAgent := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	_, err = w.o.session(agentCtx, plan, w.o.runAgent)
	interrupted := agentCtx.Err() != nil
	stopAgent()
	if interrupted {
		_, _ = fmt.Fprintln(os.Stdout, "\n[接入会话] 已中断，Agent 已停止。重跑 `orca-factory-driver onboard --repo <path>` 会续接 worktree。")
		return false, 1
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stdout, err)
		if ok, _ := w.confirm("重试接入会话"); !ok {
			return false, 1
		}
		return true, -1
	}
	facts, err := w.o.observe(ctx, repo)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stdout, err)
		return false, 1
	}
	if facts.PR.State != prNone {
		return true, -1
	}
	_, _ = fmt.Fprintln(os.Stdout, "[接入会话] Agent 已结束，gh 没有读到本仓库 feat/agent-graph 的 PR。")
	_, _ = fmt.Fprintln(os.Stdout, "  worktree 保留。重跑 `orca-factory-driver onboard --repo <path>` 会续接这个 worktree。")
	return false, 1
}

// waitMerge shows the onboarding PR and waits: Enter re-observes (returns -1
// to continue the loop), q exits 0 with the hint that a rerun continues at
// the enable stage.
func (w wizard) waitMerge(facts onboardFacts, pr onboardPR) int {
	_, _ = fmt.Fprintf(os.Stdout, "\n[等待合并] 接入 PR：https://github.com/%s/pull/%d（OPEN）\n", strings.TrimPrefix(facts.Remote, "github.com/"), pr.Number)
	line, readable := w.prompt("合并后回车重查（q 退出）")
	if !readable || line == "q" || line == "Q" {
		_, _ = fmt.Fprintln(os.Stdout, "合并后再运行 `orca-factory-driver onboard`，会从启用阶段继续。")
		return 0
	}
	return -1
}

// enable runs the same enabler the Feishu button and `enable --repo` use
// (OF-195), as this terminal's operator. Enter retries after a failure, q
// exits 1.
func (w wizard) enable(ctx context.Context, facts onboardFacts) int {
	for {
		_, _ = fmt.Fprintf(os.Stdout, "\n[启用] %s：合并复核 → 快进主检出 → check → 项目表启用 → 清理现场\n", facts.Remote)
		operator, err := operatorIdentity()
		if err != nil {
			_, _ = fmt.Fprintln(os.Stdout, err)
			return 1
		}
		if _, err := (enabler{checker: factoryChecker(w.o.cfg.FactoryRoot), writeRow: factoryEnabledWriter(w.o.cfg), cleanup: onboardSceneCleaner(w.o.cacheDir)}).
			run(ctx, intake.EnableRequest{Repo: facts.Repo, Remote: facts.Remote, Requester: operator}); err != nil {
			_, _ = fmt.Fprintln(os.Stdout, err)
			line, readable := w.prompt("回车重试（q 退出）")
			if !readable || line == "q" || line == "Q" {
				return 1
			}
			continue
		}
		return -1
	}
}
