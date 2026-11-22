package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
)

func TestEligibleMode(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	ledger := map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_PROJECT": "host/one"}
	project := func(path, mode string) projects.Project {
		env := map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_PROJECT": "host/one"}
		if mode != "" {
			env["AGENT_MODE"] = mode
		}
		return projects.Project{Path: path, Repository: config.Repository{Path: path, Env: env}}
	}
	rejected := project(root, "auto").Repository
	moved := project(root, "auto")
	moved.Repository.Env["AGENT_FEISHU_PROJECT"] = "host/two"
	for _, tc := range []struct {
		name   string
		result projects.Result
		want   string
	}{
		{"auto", projects.Result{Eligible: []projects.Project{project(other, "manual"), project(link, "auto")}}, "auto"},
		{"manual", projects.Result{Eligible: []projects.Project{project(other, "auto"), project(root, "manual")}}, "manual"},
		{"default", projects.Result{Eligible: []projects.Project{project(root, "")}}, "manual"},
		{"rejected", projects.Result{Eligible: []projects.Project{project(other, "auto")}, Rejected: []projects.Rejection{{Path: root, Repository: &rejected}}}, ""},
		{"another ledger project", projects.Result{Eligible: []projects.Project{moved}}, ""},
		{"ambiguous", projects.Result{Eligible: []projects.Project{project(root, "auto"), project(link, "manual")}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode, err := eligibleMode(tc.result, root, ledger)
			if mode != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("mode=%q err=%v, want %q", mode, err, tc.want)
			}
		})
	}
}

func TestRepositoryModeOutsideDiscovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct{ name, env, want string }{
		{"backend from the environment", "AGENT_GH_REPO=acme/one\nAGENT_MODE=manual\n", "manual"},
		{"another repository", "AGENT_ISSUE_BACKEND=github\nAGENT_GH_REPO=acme/two\nAGENT_MODE=auto\n", ""},
		{"feishu profile", "AGENT_ISSUE_BACKEND=feishu\nAGENT_MODE=auto\n", ""},
		{"github auto", "AGENT_ISSUE_BACKEND=github\nAGENT_MODE=auto\n", "auto"},
		{"github default", "AGENT_ISSUE_BACKEND=github\n", "manual"},
		{"invalid mode", "AGENT_ISSUE_BACKEND=github\nAGENT_MODE=turbo\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			gates := filepath.Join(root, "scripts", "agent-gates")
			if err := os.MkdirAll(gates, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(gates, "agent-graph.env"), []byte(tc.env), 0o600); err != nil {
				t.Fatal(err)
			}
			mode, err := repositoryMode(t.Context(), root, map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "acme/one"})
			if mode != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("mode=%q err=%v, want %q", mode, err, tc.want)
			}
		})
	}
}

func TestExecCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		ticket string
	}{
		{"empty", []string{"exec"}, 2, "unknown"},
		{"ordinary", []string{"exec", "ENG-1"}, 1, "ENG-1"},
		{"merging", []string{"exec", "--merging", "ENG-1"}, 1, "ENG-1"},
		{"missing ticket", []string{"exec", "--merging"}, 2, "unknown"},
		{"dispatched", []string{"exec", "--dispatched", "LAUNCH1", "ENG-1"}, 1, "ENG-1"},
		{"dispatched missing launch", []string{"exec", "--dispatched", "ENG-1"}, 2, "unknown"},
		{"operator mode", []string{"exec", "--auto", "ENG-1"}, 2, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--"}, tc.args...)...)
			cmd.Dir = home
			cmd.Env = append(os.Environ(), "DRIVER_TEST_PROCESS=1", "HOME="+home, "AGENT_DRIVER_STATE_ROOT="+filepath.Join(home, ".local", "state", "orca-factory"), "ORCA_PANE_KEY=", "ORCA_TERMINAL_HANDLE=test-terminal", "PATH="+home)
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					code = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code {
				t.Fatalf("exit=%d want=%d output=%s", code, tc.code, out)
			}
			if tc.code == 2 && !strings.Contains(string(out), "usage:") {
				t.Fatalf("missing usage: %s", out)
			}
			if tc.code == 1 && strings.Contains(string(out), "usage:") {
				t.Fatalf("valid args rejected: %s", out)
			}
			path := filepath.Join(home, ".local", "state", "orca-factory", "unknown", tc.ticket, "executor.log")
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.code == 1 && !strings.Contains(string(raw), `"executable":"git"`) {
				t.Fatalf("missing external command log: %s", raw)
			}
			if tc.code == 1 {
				var viewed bytes.Buffer
				if err := streamLogs(context.Background(), &viewed, filepath.Join(home, ".local", "state", "orca-factory"), tc.ticket, time.Time{}, true, false); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(viewed.String(), `"msg":"executor failed"`) || !strings.Contains(viewed.String(), `"phase":"initialization"`) {
					t.Fatalf("driver logs missing executor initialization error: %s", &viewed)
				}
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("wrong log path: %v", err)
			}
		})
	}
}
