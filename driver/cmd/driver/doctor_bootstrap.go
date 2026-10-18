package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goversion "go/version"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// bootstrapChecks fixes the order `doctor --bootstrap` checks an empty machine in.
var bootstrapChecks = []string{"go", "gh", "git", "jq", "claude", "gh_auth", "systemd_user", "linger", "orca", "model_proxy", "driver_env", "driver_toml"}

var bootstrapToolFixes = map[string]string{
	"gh":     "sudo apt install -y gh",
	"git":    "sudo apt install -y git",
	"jq":     "sudo apt install -y jq",
	"claude": "curl -fsSL https://claude.ai/install.sh | bash",
}

const bootstrapDriverTOMLFix = `mkdir -p ~/.config/orca-factory && printf 'factory_root = "%s"\n' "$PWD" >> ~/.config/orca-factory/driver.toml  # 在 orca-factory 仓库根运行`

var errBootstrapNotOnPath = errors.New("not on PATH")

// bootstrapProbe reads an unprepared machine without changing it. go.mod is
// relative to the working directory, so bootstrap runs from the repository root.
type bootstrapProbe struct {
	path, home, user, goMod string
	getenv                  func(string) string
	reachable               func(context.Context, string) error
}

func runBootstrap(args []string) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	current, err := user.Current()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	probe := bootstrapProbe{
		path:      os.Getenv("PATH"),
		home:      home,
		user:      current.Username,
		goMod:     filepath.Join("driver", "go.mod"),
		getenv:    os.Getenv,
		reachable: httpReachable,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return probe.main(ctx, args, os.Stdout)
}

func (b bootstrapProbe) main(ctx context.Context, args []string, out io.Writer) int {
	asJSON := slices.Contains(args, "--json")
	want := 1
	if asJSON {
		want = 2
	}
	if len(args) != want || !slices.Contains(args, "--bootstrap") {
		fmt.Fprintln(os.Stderr, doctorUsage)
		return 2
	}
	report := b.run(ctx)
	if err := writeHealth(out, report, asJSON, bootstrapChecks); err != nil {
		fmt.Fprintln(os.Stderr, "cannot output doctor report")
		return 1
	}
	for _, check := range report.Checks {
		if check.Status == healthFail || check.Status == healthHuman {
			return 1
		}
	}
	return 0
}

func (b bootstrapProbe) run(ctx context.Context) healthReport {
	env, envErr := b.driverEnv()
	checks := map[string]healthCheck{
		"go":           b.checkGo(ctx),
		"gh_auth":      b.checkGHAuth(ctx),
		"systemd_user": b.checkSystemdUser(ctx),
		"linger":       b.checkLinger(ctx),
		"orca":         b.checkOrca(ctx),
		"model_proxy":  b.checkModelProxy(ctx, env),
		"driver_env":   checkDriverEnv(env, envErr),
		"driver_toml":  b.checkDriverTOML(),
	}
	for tool, fix := range bootstrapToolFixes {
		checks[tool] = b.checkTool(tool, fix)
	}
	report := healthReport{Checks: checks, Errors: []string{}}
	for _, name := range bootstrapChecks {
		if checks[name].unprobed {
			report.Errors = append(report.Errors, name+": "+checks[name].Reason)
		}
	}
	return report
}

func (b bootstrapProbe) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	path := lookPathIn(b.path, name)
	if path == "" {
		return nil, errBootstrapNotOnPath
	}
	return runx.Run(ctx, runx.Spec{Path: path, Args: args, Timeout: doctorCommandTimeout})
}

func bootstrapFinding(status, reason, fix string) healthCheck {
	return healthCheck{Status: status, Reason: reason, Fix: fix, unprobed: status == healthUnknown}
}

// goModRequirement returns the go directive and the toolchain to install.
func goModRequirement(path string) (minimum, toolchain string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" {
			minimum = "go" + fields[1]
		}
		if len(fields) == 2 && fields[0] == "toolchain" {
			toolchain = fields[1]
		}
	}
	if minimum == "" {
		return "", "", errors.New("go.mod has no go directive")
	}
	return minimum, cmp.Or(toolchain, minimum), nil
}

func (b bootstrapProbe) checkGo(ctx context.Context) healthCheck {
	minimum, toolchain, err := goModRequirement(b.goMod)
	if err != nil {
		return bootstrapFinding(healthUnknown, "读不到 driver/go.mod", "cd <orca-factory 仓库根> && orca-factory-driver doctor --bootstrap")
	}
	fix := fmt.Sprintf("curl -fsSL https://go.dev/dl/%s.%s-%s.tar.gz | sudo tar -C /usr/local -xz && export PATH=/usr/local/go/bin:$PATH", toolchain, runtime.GOOS, runtime.GOARCH)
	out, err := b.output(ctx, "go", "version")
	switch {
	case errors.Is(err, errBootstrapNotOnPath):
		return bootstrapFinding(healthFail, "PATH 中没有 go", fix)
	case err != nil:
		return bootstrapFinding(healthUnknown, commandFailure("go version", err), "go version")
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 || !goversion.IsValid(fields[2]) {
		return bootstrapFinding(healthUnknown, "go version 输出不可解析", "go version")
	}
	if goversion.Compare(fields[2], minimum) < 0 {
		return bootstrapFinding(healthFail, fmt.Sprintf("go 版本 %s 低于 go.mod 要求 %s", fields[2], minimum), fix)
	}
	return bootstrapFinding(healthOK, fmt.Sprintf("go 版本 %s 满足 go.mod 要求 %s", fields[2], minimum), "")
}

func (b bootstrapProbe) checkTool(tool, fix string) healthCheck {
	if lookPathIn(b.path, tool) == "" {
		return bootstrapFinding(healthFail, "PATH 中没有 "+tool, fix)
	}
	return bootstrapFinding(healthOK, "PATH 中有 "+tool, "")
}

// checkGHAuth separates a locked keyring (gh reports the stored token invalid,
// OF-138) from no login at all; both need a person at the desktop.
func (b bootstrapProbe) checkGHAuth(ctx context.Context) healthCheck {
	out, err := b.output(ctx, "gh", "auth", "status")
	switch {
	case errors.Is(err, errBootstrapNotOnPath):
		return bootstrapFinding(healthHuman, "gh 未安装，无法确认 GitHub 登录", "安装 gh 后运行 gh auth login")
	case err == nil:
		return bootstrapFinding(healthOK, "gh 已认证", "")
	case ghTokenInvalid(out, err):
		return bootstrapFinding(healthHuman, "gh 读不到 keyring 中的凭证（keyring 可能未解锁）", "登录桌面会话解锁 login keyring，再运行 gh auth status")
	case ghNotLoggedIn(out, err):
		return bootstrapFinding(healthHuman, "gh 未登录 GitHub", "gh auth login")
	}
	return bootstrapFinding(healthUnknown, commandFailure("gh auth status", err), "gh auth status")
}

func ghNotLoggedIn(stdout []byte, err error) bool {
	return strings.Contains(commandText(stdout, err), "You are not logged into any GitHub hosts")
}

func (b bootstrapProbe) checkSystemdUser(ctx context.Context) healthCheck {
	out, err := b.output(ctx, "systemctl", "--user", "show", "--property=SystemState", "--value")
	switch {
	case errors.Is(err, errBootstrapNotOnPath):
		return bootstrapFinding(healthFail, "PATH 中没有 systemctl", "在使用 systemd 的 Linux 上运行流水线")
	case err != nil:
		return bootstrapFinding(healthFail, commandFailure("systemctl --user show", err)+"，连不上 systemd 用户实例", `export XDG_RUNTIME_DIR=/run/user/$(id -u)  # 或在该用户的登录会话中运行`)
	}
	return bootstrapFinding(healthOK, "systemd 用户实例状态 "+strings.TrimSpace(string(out)), "")
}

// checkLinger treats a loginctl error as linger off: loginctl only knows a
// user that is logged in or lingering.
func (b bootstrapProbe) checkLinger(ctx context.Context) healthCheck {
	const fix = `loginctl enable-linger "$USER"`
	out, err := b.output(ctx, "loginctl", "show-user", b.user, "--property=Linger", "--value")
	switch {
	case errors.Is(err, errBootstrapNotOnPath):
		return bootstrapFinding(healthFail, "PATH 中没有 loginctl", "在使用 systemd-logind 的 Linux 上运行流水线")
	case err != nil:
		return bootstrapFinding(healthFail, commandFailure("loginctl show-user", err)+"，linger 未启用", fix)
	case strings.TrimSpace(string(out)) != "yes":
		return bootstrapFinding(healthFail, "linger 未启用，注销后用户服务会停止", fix)
	}
	return bootstrapFinding(healthOK, "linger 已启用", "")
}

func (b bootstrapProbe) checkOrca(ctx context.Context) healthCheck {
	out, err := b.output(ctx, "orca", "status", "--json")
	switch {
	case errors.Is(err, errBootstrapNotOnPath):
		return bootstrapFinding(healthHuman, "PATH 中没有 orca", "安装 Orca 桌面端并启动")
	case err != nil:
		return bootstrapFinding(healthHuman, commandFailure("orca status", err)+"，Orca 可能未运行", "启动 Orca 桌面端")
	}
	var status struct {
		Result struct {
			App struct {
				Running bool `json:"running"`
			} `json:"app"`
			Runtime struct {
				State string `json:"state"`
			} `json:"runtime"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &status) != nil {
		return bootstrapFinding(healthUnknown, "orca status 输出不可解析", "orca status --json")
	}
	if !status.Result.App.Running || status.Result.Runtime.State != "ready" {
		return bootstrapFinding(healthHuman, "Orca 运行时未就绪（state "+status.Result.Runtime.State+"）", "启动 Orca 桌面端并等待运行时就绪")
	}
	return bootstrapFinding(healthOK, "Orca 运行中，运行时就绪", "")
}

// checkModelProxy prefers driver.env, which the service loads, over the
// caller's shell. The URL can embed a credential, so no output carries it.
func (b bootstrapProbe) checkModelProxy(ctx context.Context, env map[string]string) healthCheck {
	// AGENT_MODEL_PROXY_URL is what the executor injects for the reviewer (OF-174).
	var source, name, url string
	for _, key := range []string{"AGENT_MODEL_PROXY_URL", "ANTHROPIC_BASE_URL"} {
		if url = env[key]; url != "" {
			source, name = "driver.env 中", key
			break
		}
		if url = b.getenv(key); url != "" {
			source, name = "当前环境变量中", key
			break
		}
	}
	if url == "" {
		return bootstrapFinding(healthOK, "未配置 AGENT_MODEL_PROXY_URL 或 ANTHROPIC_BASE_URL，不经模型代理", "")
	}
	if err := b.reachable(ctx, url); err != nil {
		return bootstrapFinding(healthFail, source+" "+name+" 指向的模型代理不可达", "启动模型代理，或修正"+source+"的 "+name)
	}
	return bootstrapFinding(healthOK, source+" "+name+" 指向的模型代理可达", "")
}

func (b bootstrapProbe) driverEnv() (env map[string]string, err error) {
	file, err := os.Open(filepath.Join(b.home, ".config", "orca-factory", "driver.env"))
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	return config.ParseEnv(file)
}

func checkDriverEnv(env map[string]string, err error) healthCheck {
	const path = "~/.config/orca-factory/driver.env"
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return bootstrapFinding(healthFail, path+" 不可读或格式无效", "chmod 600 "+path+"，并按 KEY=VALUE 逐行改写")
	}
	var missing []string
	for _, name := range doctorCredentialNames {
		if env[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return bootstrapFinding(healthOK, path+" 含全部必需变量", "")
	}
	reason := path + " 缺少 " + strings.Join(missing, ", ")
	if err != nil {
		reason = "没有 " + path + "，缺少 " + strings.Join(missing, ", ")
	}
	return bootstrapFinding(healthHuman, reason, "向飞书应用管理员取得凭证，在 "+path+"（权限 600）写入 "+strings.Join(missing, "=…, ")+"=…")
}

func (b bootstrapProbe) checkDriverTOML() healthCheck {
	if _, err := os.Stat(filepath.Join(b.home, ".config", "orca-factory", "driver.toml")); errors.Is(err, os.ErrNotExist) {
		return bootstrapFinding(healthFail, "没有 ~/.config/orca-factory/driver.toml", bootstrapDriverTOMLFix)
	}
	cfg, err := config.Load(b.home)
	if err != nil {
		return bootstrapFinding(healthFail, "driver.toml 无效: "+err.Error(), "按 driver/README.md 的“可调参数”修正 ~/.config/orca-factory/driver.toml")
	}
	if cfg.FactoryRoot == "" {
		return bootstrapFinding(healthFail, "driver.toml 未设置 factory_root", bootstrapDriverTOMLFix)
	}
	return bootstrapFinding(healthOK, "driver.toml 已设置 factory_root", "")
}
