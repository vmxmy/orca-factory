package executor

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
)

func TestExecutorStateRoot(t *testing.T) {
	for _, tc := range []struct {
		name, root, home string
		wantErr          bool
	}{
		{name: "explicit", root: "/custom/state", home: "/home/test"},
		{name: "explicit without home", root: "/custom/state"},
		{name: "default", home: "/home/test"},
		{name: "unavailable", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_DRIVER_STATE_ROOT", tc.root)
			t.Setenv("HOME", tc.home)
			root, err := executorStateRoot()
			if tc.wantErr {
				if err == nil {
					t.Fatal("missing home accepted")
				}
				return
			}
			want := tc.root
			if want == "" {
				want = filepath.Join(tc.home, ".local", "state", "orca-factory")
			}
			if err != nil || root != want {
				t.Fatalf("root=%q want=%q err=%v", root, want, err)
			}
		})
	}
}

func TestExecUsesRepositoryProfiles(t *testing.T) {
	for _, tc := range []struct{ name, env, wantDefault, wantStrong, wantEscalate string }{
		{"profile", "", "codex:daily:low", "claude:strong:high", "1"},
		{"override", "AGENT_TIER_DEFAULT=codex:repository:high\nAGENT_TIER_ESCALATE=0\n", "codex:repository:high", "claude:strong:high", "0"},
		{"missing profile", "AGENT_PROFILE=missing\nAGENT_TIER_DEFAULT=codex::low\n", "codex::low", "", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			repo := t.TempDir()
			for path, body := range map[string]string{
				filepath.Join(home, ".config/orca-factory/profiles.toml"):  "default='daily'\n[profile.daily]\nAGENT_TIER_DEFAULT='codex:daily:low'\nAGENT_TIER_STRONG='claude:strong:high'\nAGENT_TIER_ESCALATE='1'\n",
				filepath.Join(repo, "scripts/agent-gates/agent-graph.env"): "AGENT_GH_REPO=owner/repo\nAGENT_ROLE_REVIEWER_CMD='review command'\n" + tc.env,
			} {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			repository, err := config.LoadRepository(repo)
			if err != nil {
				t.Fatal(err)
			}
			executor, _, err := repositoryExecutor(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			exec := []string{executor.Config.TierSettings["default"], executor.Config.TierSettings["strong"], executor.Config.Escalate}
			if want := []string{tc.wantDefault, tc.wantStrong, tc.wantEscalate}; !reflect.DeepEqual(exec, want) {
				t.Fatalf("exec tiers=%q, want %q", exec, want)
			}
			if tc.name != "missing profile" {
				loaded := []string{repository.Env["AGENT_TIER_DEFAULT"], repository.Env["AGENT_TIER_STRONG"], repository.Env["AGENT_TIER_ESCALATE"]}
				if !reflect.DeepEqual(loaded, exec) {
					t.Fatalf("LoadRepository tiers=%q, exec tiers=%q", loaded, exec)
				}
			}
			if executor.Config.Agent != "codex" {
				t.Fatalf("agent=%q", executor.Config.Agent)
			}
			if tc.name == "missing profile" && (executor.Config.Model != "" || executor.Config.Effort != "") {
				t.Fatalf("default model must omit effort: %+v", executor.Config)
			}
		})
	}
}

func TestExecReadsRepositoryEnvWithBash(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	path := filepath.Join(repo, "scripts/agent-gates/agent-graph.env")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body := "AGENT_GH_REPO=owner/repo\nAGENT_ROLE_REVIEWER_CMD='review command'\nAGENT_TIER_DEFAULT=codex:model:high\nAGENT_INSTALL_CMD='printf '\\''ok'\\'''\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	executor, _, err := repositoryExecutor(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := executor.Tools.(*Commands).InstallCommand; got != "printf 'ok'" {
		t.Fatalf("install command=%q", got)
	}
}

func TestExecIgnoresInheritedProfileKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_PROFILE", "alternate")
	t.Setenv("AGENT_TIER_DEFAULT", "codex:inherited:high")
	repo := t.TempDir()
	for path, body := range map[string]string{
		filepath.Join(home, ".config/orca-factory/profiles.toml"):  "default='daily'\n[profile.daily]\nAGENT_TIER_DEFAULT='codex:daily:low'\n[profile.alternate]\nAGENT_TIER_DEFAULT='codex:alternate:low'\n",
		filepath.Join(repo, "scripts/agent-gates/agent-graph.env"): "AGENT_GH_REPO=owner/repo\nAGENT_ROLE_REVIEWER_CMD='review command'\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	executor, _, err := repositoryExecutor(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := config.LoadRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := executor.Config.TierSettings["default"]; got != "codex:daily:low" || repository.Env["AGENT_TIER_DEFAULT"] != got {
		t.Fatalf("exec default=%q, LoadRepository default=%q", got, repository.Env["AGENT_TIER_DEFAULT"])
	}
}

func TestExecKeepsBaselineTiersWithoutProfile(t *testing.T) {
	for _, tc := range []struct{ name, env, wantDefault, wantEscalate string }{
		{"inherited", "", "codex:inherited:high", "1"},
		{"repository unset", "unset AGENT_TIER_ESCALATE\nAGENT_TIER_DEFAULT=codex:repository:low\n", "codex:repository:low", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("AGENT_TIER_DEFAULT", "codex:inherited:high")
			t.Setenv("AGENT_TIER_STRONG", "claude:inherited:high")
			t.Setenv("AGENT_TIER_ESCALATE", "1")
			repo := t.TempDir()
			path := filepath.Join(repo, "scripts/agent-gates/agent-graph.env")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("AGENT_GH_REPO=owner/repo\nAGENT_ROLE_REVIEWER_CMD='review command'\nprintf x >> sourced\n"+tc.env), 0600); err != nil {
				t.Fatal(err)
			}
			executor, _, err := repositoryExecutor(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			if sourced, err := os.ReadFile(filepath.Join(repo, "sourced")); err != nil || string(sourced) != "x" {
				t.Fatalf("repository env sourced %q times, want once: %v", sourced, err)
			}
			if got := []string{executor.Config.TierSettings["default"], executor.Config.Escalate}; !reflect.DeepEqual(got, []string{tc.wantDefault, tc.wantEscalate}) {
				t.Fatalf("exec tiers=%q, want %q without profiles.toml", got, []string{tc.wantDefault, tc.wantEscalate})
			}
		})
	}
}

func TestExecReadsLane3Globs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	path := filepath.Join(repo, "scripts/agent-gates/agent-graph.env")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body := "AGENT_GH_REPO=owner/repo\nAGENT_ROLE_REVIEWER_CMD='review command'\nAGENT_TIER_DEFAULT=codex:model:high\nAGENT_LANE3_GLOBS='.env* *secrets*'\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	executor, _, err := repositoryExecutor(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := executor.Config.Lane3Globs; !reflect.DeepEqual(got, []string{".env*", "*secrets*"}) {
		t.Fatalf("lane 3 globs=%q", got)
	}
}

func TestExecUsesProfileTierChain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	for path, body := range map[string]string{
		filepath.Join(home, ".config/orca-factory/profiles.toml"): "default='daily'\n[profile.daily]\n" +
			"AGENT_TIER_DEFAULT='codex:daily:low'\nAGENT_TIER_STRONG='claude:strong:high'\n" +
			"AGENT_TIER_CHAIN='default strong max'\nAGENT_TIER_MAX='codex:max:high'\nAGENT_TIER_ESCALATE='1'\n",
		filepath.Join(repo, "scripts/agent-gates/agent-graph.env"): "AGENT_GH_REPO=owner/repo\nAGENT_ROLE_REVIEWER_CMD='review command'\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	executor, _, err := repositoryExecutor(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{2, 3} {
		tier, agent, model, effort, err := correctionTier(executor.Config, n)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := []string{tier, agent, model, effort}, []string{"max", "codex", "max", "high"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("correction %d tier=%q, want %q", n, got, want)
		}
	}
}

func TestExecRejectsUnconfiguredChainTier(t *testing.T) {
	for _, tc := range []struct{ name, env, wantErr string }{
		{"configured", "AGENT_TIER_MAX='codex:max:high'\n", ""},
		{"missing tier", "", "AGENT_TIER_MAX"},
		{"escalation off", "AGENT_TIER_ESCALATE=0\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			repo := t.TempDir()
			path := filepath.Join(repo, "scripts/agent-gates/agent-graph.env")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			body := "AGENT_GH_REPO=owner/repo\nAGENT_ROLE_REVIEWER_CMD='review command'\nAGENT_TIER_DEFAULT='codex:daily:low'\n" +
				"AGENT_TIER_STRONG='claude:strong:high'\nAGENT_TIER_CHAIN='default strong max'\nAGENT_TIER_ESCALATE=1\n" + tc.env
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			executor, _, err := repositoryExecutor(context.Background(), repo)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := executor.Config.TierChain; !reflect.DeepEqual(got, []string{"default", "strong", "max"}) {
				t.Fatalf("chain=%q", got)
			}
		})
	}
}

func TestCorrectionTierFollowsChain(t *testing.T) {
	chain := Config{Tier: "default", Agent: "codex", Model: "daily", Effort: "low", Escalate: "1",
		TierChain:    []string{"default", "strong", "max"},
		TierSettings: map[string]string{"default": "codex:daily:low", "strong": "claude:strong:high", "max": "codex:max:xhigh"}}
	off := chain
	off.Escalate = "0"
	strong := chain
	strong.Tier = "strong"
	for _, tc := range []struct {
		name string
		c    Config
		n    int
		want []string
	}{
		{"first correction", chain, 1, []string{"strong", "claude", "strong", "high"}},
		{"second correction", chain, 2, []string{"max", "codex", "max", "xhigh"}},
		{"chain end holds", chain, 5, []string{"max", "codex", "max", "xhigh"}},
		{"strong entry", strong, 1, []string{"max", "codex", "max", "xhigh"}},
		{"escalation off", off, 2, []string{"default", "codex", "daily", "low"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tier, agent, model, effort, err := correctionTier(tc.c, tc.n)
			if err != nil {
				t.Fatal(err)
			}
			if got := []string{tier, agent, model, effort}; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
