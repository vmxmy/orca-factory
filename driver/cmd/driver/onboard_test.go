package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

const onboardRemote = "github.com/vmxmy/ziikoo-control-plane"

// onboardFixture is one machine with a factory checkout (its own
// agent-graph.env supplies the ledger and tier defaults) and Orca listing one
// repository at repo, which has never been onboarded.
type onboardFixture struct {
	home, factory, repo, state string
}

func newOnboardFixture(t *testing.T) onboardFixture {
	t.Helper()
	root := t.TempDir()
	f := onboardFixture{home: filepath.Join(root, "home"), factory: filepath.Join(root, "orca-factory"), repo: filepath.Join(root, "ziikoo-control-plane"), state: filepath.Join(root, "state")}
	writeEnv := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeEnv(filepath.Join(f.home, ".config/orca-factory/driver.toml"), "factory_root = \""+f.factory+"\"\n")
	writeEnv(filepath.Join(f.factory, "scripts/agent-gates/agent-graph.env"), strings.Join([]string{
		"AGENT_GH_REPO=vmxmy/orca-factory",
		"AGENT_ISSUE_BACKEND=feishu",
		"AGENT_FEISHU_BASE=BASE_TOKEN",
		"AGENT_FEISHU_PROJECT=github.com/vmxmy/orca-factory",
		"AGENT_TIER_FAST='claude:claude-sonnet-5-5:high'",
		"AGENT_TIER_DEFAULT='claude:claude-sonnet-5-5:high'",
		"AGENT_TIER_STRONG='claude:claude-opus-5-5:high'",
		"AGENT_TIER_ESCALATE=1",
		"AGENT_TIER_BY_ESTIMATE='1-2:fast 3-5:default 8-:strong'",
		"AGENT_DEPLOY_WORKFLOW='Deploy'",
		"",
	}, "\n"))
	if err := os.MkdirAll(f.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", f.home)
	t.Setenv("AGENT_DRIVER_STATE_ROOT", f.state)
	t.Setenv("AGENT_ORCA_BIN", fakeOrcaRepoList(t, f.repo, onboardRemote))
	return f
}

// TestOnboardDryRun covers OF-50's `onboard --repo <path> --dry-run`: for an
// onboardable repository it prints the plan (steps and the agent-graph.env
// defaults taken from the factory's own profile, the remote and a fresh
// 编号前缀) without taking the lock or creating the worktree.
func TestOnboardDryRun(t *testing.T) {
	f := newOnboardFixture(t)
	stdout := captureStdout(t, func() {
		if code := runOnboard([]string{"--repo", f.repo, "--dry-run"}); code != 0 {
			t.Fatalf("exit code = %d", code)
		}
	})
	var plan onboardPlan
	if err := json.Unmarshal(stdout, &plan); err != nil {
		t.Fatalf("invalid JSON %s: %v", stdout, err)
	}
	if plan.Repo != f.repo || plan.Remote != onboardRemote || plan.Name != "ziikoo-control-plane" || plan.Prefix != "ZCP" || plan.Branch != "feat/agent-graph" {
		t.Fatalf("plan=%+v", plan)
	}
	wantEnv := map[string]string{
		"AGENT_ISSUE_BACKEND":    "feishu",
		"AGENT_FEISHU_BASE":      "BASE_TOKEN",
		"AGENT_FEISHU_PROJECT":   onboardRemote,
		"AGENT_GH_REPO":          "vmxmy/ziikoo-control-plane",
		"AGENT_TIER_FAST":        "claude:claude-sonnet-5-5:high",
		"AGENT_TIER_DEFAULT":     "claude:claude-sonnet-5-5:high",
		"AGENT_TIER_STRONG":      "claude:claude-opus-5-5:high",
		"AGENT_TIER_ESCALATE":    "1",
		"AGENT_TIER_BY_ESTIMATE": "1-2:fast 3-5:default 8-:strong",
	}
	if !mapsEqual(plan.Env, wantEnv) {
		t.Fatalf("env=%v\nwant %v", plan.Env, wantEnv)
	}
	if !slices.Equal(plan.Pending, []string{"AGENT_DEPLOY_WORKFLOW", "AGENT_SMOKE_CMD", "AGENT_NOTIFY_FEISHU_PROFILE", "AGENT_NOTIFY_FEISHU_CHAT"}) {
		t.Fatalf("pending=%v", plan.Pending)
	}
	steps := strings.Join(plan.Steps, "\n")
	for _, want := range []string{"worktree add -b feat/agent-graph " + plan.Worktree + " origin/main", f.factory + "/bin/orca-factory init " + plan.Worktree, "5a", "5d", "启用=false"} {
		if !strings.Contains(steps, want) {
			t.Fatalf("steps=%s\nwant %q", steps, want)
		}
	}
	for _, path := range []string{plan.Worktree, filepath.Join(f.state, "onboard")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("dry run created %s (err=%v)", path, err)
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestOnboardRefusesNonOnboardable covers OF-50: a repository that already
// has scripts/agent-gates/, or a path Orca does not list, is refused.
func TestOnboardRefusesNonOnboardable(t *testing.T) {
	f := newOnboardFixture(t)
	other := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home)
	if err != nil {
		t.Fatal(err)
	}
	o := onboarder{cfg: cfg, orca: &orca.Client{}, stateRoot: f.state, cacheDir: t.TempDir()}
	if _, err := o.plan(context.Background(), other); err == nil || !strings.Contains(err.Error(), "不在 Orca 仓库清单") {
		t.Fatalf("unknown repo err=%v", err)
	}
	if err := os.MkdirAll(filepath.Join(f.repo, "scripts/agent-gates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := o.plan(context.Background(), f.repo); err == nil || !strings.Contains(err.Error(), "不是可接入仓库") {
		t.Fatalf("onboarded repo err=%v", err)
	}
	if code := runOnboard([]string{"--repo", f.repo, "--dry-run"}); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestOnboardRunsAgentInWorktree covers OF-50's real run with a stand-in
// claude: the agent runs inside a fresh worktree of the target repository on
// feat/agent-graph at origin/main, with the onboard role, Bash and file tools
// under --restricted, only HOME/PATH (no Feishu or other credentials) in its
// environment; the PR link it reports and the 启用=false row the driver
// writes come back as the result. A second run while one holds the
// repository's lock is refused.
func TestOnboardRunsAgentInWorktree(t *testing.T) {
	f := newOnboardFixture(t)
	t.Setenv("AGENT_FEISHU_APP_SECRET", "must-not-leak")
	t.Setenv("GH_TOKEN", "must-not-leak")
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, filepath.Dir(origin), "init", "--quiet", "--bare", "-b", "main", origin)
	if err := os.RemoveAll(f.repo); err != nil {
		t.Fatal(err)
	}
	gitIn(t, filepath.Dir(f.repo), "clone", "--quiet", origin, f.repo)
	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "add", "README.md")
	gitIn(t, f.repo, "commit", "--quiet", "-m", "init")
	gitIn(t, f.repo, "push", "--quiet", "origin", "HEAD:main")
	mainSHA := gitIn(t, f.repo, "rev-parse", "HEAD")

	record := filepath.Join(t.TempDir(), "claude.record")
	rowMarker := filepath.Join(t.TempDir(), "row.written")
	claude := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n{ if [ -e " + rowMarker + " ]; then echo row=before-agent; else echo row=missing; fi; printf 'cwd=%s\\n' \"$(pwd -P)\"; printf 'home=%s\\n' \"$HOME\"; printf 'secret=%s\\n' \"${AGENT_FEISHU_APP_SECRET-unset}\"; printf 'gh=%s\\n' \"${GH_TOKEN-unset}\"; } > " + record + "\n" +
		"for a in \"$@\"; do printf '%s\\0' \"$a\"; done > " + record + ".args\n" +
		`printf '%s' '{"type":"result","subtype":"success","is_error":false,"result":"已开接入 PR：https://github.com/vmxmy/ziikoo-control-plane/pull/7"}'` + "\n"
	if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home)
	if err != nil {
		t.Fatal(err)
	}
	var rows []issues.LedgerProject
	o := onboarder{cfg: cfg, orca: &orca.Client{}, claude: claude, stateRoot: f.state, cacheDir: t.TempDir(),
		writeRow: func(_ context.Context, row issues.LedgerProject) (issues.LedgerProject, error) {
			rows = append(rows, row)
			return row, os.WriteFile(rowMarker, nil, 0o600)
		}}
	req := intake.OnboardRequest{Repo: f.repo, Remote: onboardRemote, Requester: "ou_APPROVER", Thread: "om_ROOT"}

	held, err := flock.TryLock(filepath.Join(f.state, "onboard", "github.com_vmxmy_ziikoo-control-plane.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.run(context.Background(), req); err == nil || !strings.Contains(err.Error(), "进行中") {
		t.Fatalf("run under a held lock err=%v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := o.run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	wantRow := issues.LedgerProject{Name: "ziikoo-control-plane", Remote: onboardRemote, Prefix: "ZCP"}
	if result.PR != "https://github.com/vmxmy/ziikoo-control-plane/pull/7" || result.Row != wantRow || len(rows) != 1 || rows[0] != wantRow {
		t.Fatalf("result=%+v rows=%+v", result, rows)
	}
	worktree := filepath.Join(o.cacheDir, "orca-factory", "onboard", "github.com_vmxmy_ziikoo-control-plane", "worktree")
	if got := gitIn(t, worktree, "rev-parse", "--abbrev-ref", "HEAD"); got != "feat/agent-graph" {
		t.Fatalf("worktree branch=%s", got)
	}
	if got := gitIn(t, worktree, "rev-parse", "HEAD"); got != mainSHA {
		t.Fatalf("worktree head=%s, want origin/main %s", got, mainSHA)
	}
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	realWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"row=before-agent", "cwd=" + realWorktree, "home=" + f.home, "secret=unset", "gh=unset"} {
		if !slices.Contains(lines, want) {
			t.Fatalf("record=%v\nwant %q", lines, want)
		}
	}
	rawArgs, err := os.ReadFile(record + ".args")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(rawArgs), "\x00"), "\x00")
	flag := func(name string) string {
		i := slices.Index(args, name)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("args=%v, missing %s", args, name)
		}
		return args[i+1]
	}
	if args[0] != "-p" || !strings.Contains(args[1], worktree) || !strings.Contains(args[1], "ZCP") {
		t.Fatalf("prompt args=%v", args[:2])
	}
	for name, want := range map[string]string{
		"--append-system-prompt-file": filepath.Join(f.factory, "scripts/agent-gates/roles/onboard.md"),
		"--model":                     "claude-opus-5-5",
		"--effort":                    "high",
		"--tools":                     "Bash,Read,Edit,Write,Grep,Glob",
		"--allowedTools":              "Bash,Read,Edit,Write,Grep,Glob",
		"--permission-mode":           "dontAsk",
		"--output-format":             "json",
	} {
		if got := flag(name); got != want {
			t.Errorf("%s=%q, want %q", name, got, want)
		}
	}
	if !slices.Contains(args, "--restricted") || !slices.Contains(args, "--strict-mcp-config") || !strings.Contains(flag("--settings"), "Bash(printenv:*)") {
		t.Fatalf("args=%v", args)
	}
}

// TestOnboardReportsAgentResultWhenPRMissing covers OF-51: when the
// onboarding agent finishes successfully but its final text carries no PR
// link, the error must carry that final explanation instead of discarding
// it, so a person reading the CLI error or the card's topic reply (which
// forwards the same error) knows where the agent got stuck.
func TestOnboardReportsAgentResultWhenPRMissing(t *testing.T) {
	f := newOnboardFixture(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, filepath.Dir(origin), "init", "--quiet", "--bare", "-b", "main", origin)
	if err := os.RemoveAll(f.repo); err != nil {
		t.Fatal(err)
	}
	gitIn(t, filepath.Dir(f.repo), "clone", "--quiet", origin, f.repo)
	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "add", "README.md")
	gitIn(t, f.repo, "commit", "--quiet", "-m", "init")
	gitIn(t, f.repo, "push", "--quiet", "origin", "HEAD:main")

	claude := filepath.Join(t.TempDir(), "claude")
	explanation := "卡在 ONBOARD 5b：install.sh --dry-run 报 unsupported AGENT_ISSUE_BACKEND，需要人工确认账本后端配置后重试"
	script := "#!/bin/sh\n" + `printf '%s' '{"type":"result","subtype":"success","is_error":false,"result":"` + explanation + `"}'` + "\n"
	if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home)
	if err != nil {
		t.Fatal(err)
	}
	o := onboarder{cfg: cfg, orca: &orca.Client{}, claude: claude, stateRoot: f.state, cacheDir: t.TempDir(),
		writeRow: func(_ context.Context, row issues.LedgerProject) (issues.LedgerProject, error) {
			return row, nil
		}}
	req := intake.OnboardRequest{Repo: f.repo, Remote: onboardRemote}
	if _, err := o.run(context.Background(), req); err == nil || !strings.Contains(err.Error(), explanation) {
		t.Fatalf("err=%v, want it to carry the agent's final explanation %q", err, explanation)
	}
}
