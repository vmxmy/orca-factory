package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeServiceEnvironment puts a systemctl stand-in on PATH that prints the
// given `systemctl --user show -p Environment` line for the driver service.
func fakeServiceEnvironment(t *testing.T, environment string) {
	t.Helper()
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "systemctl"), `[ "$*" = "--user show -p Environment orca-factory-driver.service" ] || exit 9
echo '`+environment+`'
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestOnboardPreflightSystemdPath(t *testing.T) {
	tools := t.TempDir()
	brew := t.TempDir()
	goGit := t.TempDir()
	orcaDir := t.TempDir()
	for _, tool := range []string{"go", "git", "orca"} {
		writeExecutable(t, filepath.Join(tools, tool), "exit 0\n")
	}
	for _, tool := range []string{"go", "git"} {
		writeExecutable(t, filepath.Join(goGit, tool), "exit 0\n")
	}
	writeExecutable(t, filepath.Join(brew, "gh"), "exit 0\n")
	orcaBin := filepath.Join(orcaDir, "orca")
	writeExecutable(t, orcaBin, "exit 0\n")

	t.Run("every tool resolves on the service PATH", func(t *testing.T) {
		fakeServiceEnvironment(t, "Environment=PATH="+tools+":"+brew+" GIT_CONFIG_VALUE_0=TEST-PLACEHOLDER-CRED")
		check := checkOnboardServicePath(context.Background(), onboarder{})
		if !check.OK || check.Name != "服务环境" {
			t.Fatalf("check=%+v, want a passing 服务环境 check", check)
		}
	})

	t.Run("a tool missing from the service PATH fails", func(t *testing.T) {
		fakeServiceEnvironment(t, "Environment=PATH="+tools+" GIT_CONFIG_VALUE_0=TEST-PLACEHOLDER-CRED")
		check := checkOnboardServicePath(context.Background(), onboarder{})
		if check.OK || check.Detail != "orca-factory-driver.service 的 PATH 找不到：gh；补法：在该服务的 drop-in 中把这些工具所在目录加入 Environment=PATH，然后 systemctl --user daemon-reload 并重启服务" {
			t.Fatalf("check=%+v, want gh reported missing", check)
		}
		if strings.Contains(check.Detail, "TEST-PLACEHOLDER-CRED") {
			t.Fatalf("detail=%q leaks another environment value", check.Detail)
		}
	})

	t.Run("AGENT_ORCA_BIN locates orca outside the service PATH", func(t *testing.T) {
		fakeServiceEnvironment(t, "Environment=AGENT_ORCA_BIN="+orcaBin+" PATH="+brew+":"+goGit)
		check := checkOnboardServicePath(context.Background(), onboarder{})
		if !check.OK {
			t.Fatalf("check=%+v, want orca found through AGENT_ORCA_BIN", check)
		}
	})

	t.Run("a non-executable AGENT_ORCA_BIN fails even with orca on PATH", func(t *testing.T) {
		missing := filepath.Join(orcaDir, "absent-orca")
		fakeServiceEnvironment(t, "Environment=AGENT_ORCA_BIN="+missing+" PATH="+tools+":"+brew+" GIT_CONFIG_VALUE_0=TEST-PLACEHOLDER-CRED")
		check := checkOnboardServicePath(context.Background(), onboarder{})
		if check.OK || check.Detail != "orca-factory-driver.service 的 AGENT_ORCA_BIN="+missing+" 不可执行；补法：在该服务的 drop-in 中把 Environment=AGENT_ORCA_BIN 指向可执行的 orca，然后 systemctl --user daemon-reload 并重启服务" {
			t.Fatalf("check=%+v, want the AGENT_ORCA_BIN path reported", check)
		}
		if strings.Contains(check.Detail, "TEST-PLACEHOLDER-CRED") {
			t.Fatalf("detail=%q leaks another environment value", check.Detail)
		}
	})

	t.Run("an unreachable systemd user instance names the cause", func(t *testing.T) {
		bin := t.TempDir()
		writeExecutable(t, filepath.Join(bin, "systemctl"), "echo 'Failed to connect to user scope bus via local transport' >&2\nexit 1\n")
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		check := checkOnboardServicePath(context.Background(), onboarder{})
		if check.OK || check.Detail != "无法连接 systemd user 实例（systemctl --user 退出码 1）；补法：在该用户的登录会话中运行预检，或确认 XDG_RUNTIME_DIR 指向 /run/user/<uid> 且已执行 loginctl enable-linger" {
			t.Fatalf("check=%+v, want the user bus failure reported", check)
		}
	})
}

// fakeGH puts a gh stand-in on PATH that answers
// `gh api repos/vmxmy/dujiao-next/branches/main` with branch and
// `gh api repos/vmxmy/dujiao-next/rules/branches/main` with rules.
func fakeGH(t *testing.T, branch, rules string) {
	t.Helper()
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "gh"), `case "$*" in
"api repos/vmxmy/dujiao-next/branches/main") echo '`+branch+`' ;;
"api repos/vmxmy/dujiao-next/rules/branches/main") echo '`+rules+`' ;;
*) exit 9 ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestOnboardPreflightBranchProtection(t *testing.T) {
	plan := onboardPlan{Remote: "github.com/vmxmy/dujiao-next"}
	const unprotected = `{"protected":false,"protection":{"enabled":false,"required_status_checks":{"enforcement_level":"off","contexts":[]}}}`
	const noRequiredChecks = "vmxmy/dujiao-next 的 main 没有必需检查，合并通道等不到成功检查无法 squash；补法：在 main 的分支保护中开启 Require status checks 并选中 CI 作业，或为 main 建 Ruleset 并开启 Require status checks to pass"

	t.Run("classic protection with required checks", func(t *testing.T) {
		fakeGH(t, `{"protected":true,"protection":{"enabled":true,"required_status_checks":{"enforcement_level":"everyone","contexts":["build","test"]}}}`, `[]`)
		check := checkOnboardBranchProtection(context.Background(), onboarder{}, plan)
		if !check.OK || check.Name != "分支保护" || check.Detail != "main 必需检查：build,test" {
			t.Fatalf("check=%+v, want a passing 分支保护 check naming the required checks", check)
		}
	})

	t.Run("a ruleset with required checks", func(t *testing.T) {
		fakeGH(t, unprotected, `[{"type":"deletion"},{"type":"required_status_checks","ruleset_id":7,"parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"ci","integration_id":15368}]}}]`)
		check := checkOnboardBranchProtection(context.Background(), onboarder{}, plan)
		if !check.OK || check.Detail != "main 必需检查：ci" {
			t.Fatalf("check=%+v, want the ruleset's required check accepted", check)
		}
	})

	t.Run("neither classic protection nor a ruleset fails", func(t *testing.T) {
		fakeGH(t, unprotected, `[{"type":"deletion"}]`)
		check := checkOnboardBranchProtection(context.Background(), onboarder{}, plan)
		if check.OK || check.Detail != noRequiredChecks {
			t.Fatalf("check=%+v, want missing required checks reported", check)
		}
	})

	t.Run("protected without required checks fails", func(t *testing.T) {
		fakeGH(t, `{"protected":true,"protection":{"enabled":true,"required_status_checks":{"enforcement_level":"off","contexts":["build"]}}}`, `[]`)
		check := checkOnboardBranchProtection(context.Background(), onboarder{}, plan)
		if check.OK || check.Detail != noRequiredChecks {
			t.Fatalf("check=%+v, want missing required checks reported", check)
		}
	})

	t.Run("a failing gh call names the command and exit code", func(t *testing.T) {
		bin := t.TempDir()
		writeExecutable(t, filepath.Join(bin, "gh"), "exit 4\n")
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		check := checkOnboardBranchProtection(context.Background(), onboarder{}, plan)
		if check.OK || check.Detail != "gh api repos/vmxmy/dujiao-next/branches/main 退出码 4；补法：确认 gh 已登录且能读取该仓库" {
			t.Fatalf("check=%+v, want the gh failure reported", check)
		}
	})

	t.Run("unparseable rules output fails", func(t *testing.T) {
		fakeGH(t, unprotected, `not json`)
		check := checkOnboardBranchProtection(context.Background(), onboarder{}, plan)
		if check.OK || check.Detail != "gh api repos/vmxmy/dujiao-next/rules/branches/main 输出不可解析；补法：手动运行该命令排查" {
			t.Fatalf("check=%+v, want the unparseable output reported", check)
		}
	})
}
