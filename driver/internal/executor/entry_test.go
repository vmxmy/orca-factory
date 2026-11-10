package executor

import (
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
			executor, loaded, err := repositoryExecutor(repo)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.Env, repository.Env) {
				t.Fatalf("exec env=%v, repository env=%v", loaded.Env, repository.Env)
			}
			if executor.Config.TierSettings["default"] != tc.wantDefault || executor.Config.TierSettings["strong"] != tc.wantStrong || executor.Config.Escalate != tc.wantEscalate {
				t.Fatalf("executor config=%+v", executor.Config)
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
