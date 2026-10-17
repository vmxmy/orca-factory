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
	for _, tool := range []string{"go", "git", "orca"} {
		writeExecutable(t, filepath.Join(tools, tool), "exit 0\n")
	}
	writeExecutable(t, filepath.Join(brew, "gh"), "exit 0\n")

	t.Run("every tool resolves on the service PATH", func(t *testing.T) {
		fakeServiceEnvironment(t, "Environment=AGENT_ORCA_BIN=/x/orca PATH="+tools+":"+brew+" GIT_CONFIG_VALUE_0=TEST-PLACEHOLDER-CRED")
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
}

// fakeGHBranch puts a gh stand-in on PATH that answers
// `gh api repos/vmxmy/dujiao-next/branches/main` with body.
func fakeGHBranch(t *testing.T, body string) {
	t.Helper()
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "gh"), `[ "$*" = "api repos/vmxmy/dujiao-next/branches/main" ] || exit 9
echo '`+body+`'
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestOnboardPreflightBranchProtection(t *testing.T) {
	plan := onboardPlan{Remote: "github.com/vmxmy/dujiao-next"}

	t.Run("protected with required checks", func(t *testing.T) {
		fakeGHBranch(t, `{"protected":true,"protection":{"enabled":true,"required_status_checks":{"enforcement_level":"everyone","contexts":["build","test"]}}}`)
		check := checkOnboardBranchProtection(context.Background(), onboarder{}, plan)
		if !check.OK || check.Name != "分支保护" || check.Detail != "main 已保护，必需检查：build,test" {
			t.Fatalf("check=%+v, want a passing 分支保护 check naming the required checks", check)
		}
	})

	t.Run("unprotected main fails", func(t *testing.T) {
		fakeGHBranch(t, `{"protected":false,"protection":{"enabled":false,"required_status_checks":{"enforcement_level":"off","contexts":[]}}}`)
		check := checkOnboardBranchProtection(context.Background(), onboarder{}, plan)
		if check.OK || check.Detail != "vmxmy/dujiao-next 的 main 没有分支保护；补法：在 GitHub 仓库设置为 main 添加分支保护，并把 CI 作业设为必需检查" {
			t.Fatalf("check=%+v, want missing protection reported", check)
		}
	})

	t.Run("protected without required checks fails", func(t *testing.T) {
		fakeGHBranch(t, `{"protected":true,"protection":{"enabled":true,"required_status_checks":{"enforcement_level":"off","contexts":[]}}}`)
		check := checkOnboardBranchProtection(context.Background(), onboarder{}, plan)
		if check.OK || check.Detail != "vmxmy/dujiao-next 的 main 没有必需检查，合并通道等不到成功检查无法 squash；补法：在 main 的分支保护中开启 Require status checks 并选中 CI 作业" {
			t.Fatalf("check=%+v, want missing required checks reported", check)
		}
	})
}
