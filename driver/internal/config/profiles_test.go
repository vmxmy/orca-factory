package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRepositoryProfiles(t *testing.T) {
	const profiles = `default = "daily"
[profile.daily]
AGENT_TIER_DEFAULT = "codex:daily:low"
AGENT_TIER_STRONG = "claude:strong:high"
AGENT_ROLE_RESOLVER = "codex:resolver:high"
AGENT_ROLE_INTAKE = "claude:intake:medium"
AGENT_TIER_ESCALATE = "1"
[profile.alternate]
AGENT_TIER_DEFAULT = "codex:alternate:high"
`
	for _, tc := range []struct {
		name, file, env string
		want            map[string]string
		wantErr         string
	}{
		{name: "missing file", env: "AGENT_TIER_DEFAULT=codex:repo:high\n", want: map[string]string{"AGENT_TIER_DEFAULT": "codex:repo:high"}},
		{name: "fills roles", file: profiles, want: map[string]string{"AGENT_TIER_DEFAULT": "codex:daily:low", "AGENT_TIER_STRONG": "claude:strong:high", "AGENT_ROLE_RESOLVER": "codex:resolver:high", "AGENT_ROLE_INTAKE": "claude:intake:medium", "AGENT_TIER_ESCALATE": "1"}},
		{name: "repository wins including empty", file: profiles, env: "AGENT_PROFILE=alternate\nAGENT_TIER_DEFAULT=\nAGENT_ROLE_INTAKE=claude:repo:high\n", want: map[string]string{"AGENT_PROFILE": "alternate", "AGENT_TIER_DEFAULT": "", "AGENT_ROLE_INTAKE": "claude:repo:high"}},
		{name: "repository selects profile", file: profiles, env: "AGENT_PROFILE=alternate\n", want: map[string]string{"AGENT_PROFILE": "alternate", "AGENT_TIER_DEFAULT": "codex:alternate:high"}},
		{name: "missing selected profile", file: profiles, env: "AGENT_PROFILE=missing\n", want: map[string]string{"AGENT_PROFILE": "missing"}},
		{name: "no selection", file: "[profile.daily]\nAGENT_TIER_DEFAULT='codex:daily:low'\n", want: map[string]string{}},
		{name: "unknown key in unselected profile", file: profiles + "SECRET='hidden'\n", wantErr: "SECRET"},
		{name: "unknown top level", file: "unknown='hidden'\n" + profiles, wantErr: "unknown"},
		{name: "invalid syntax", file: "default = [", wantErr: "profiles.toml"},
		{name: "invalid role value", file: "[profile.daily]\nAGENT_TIER_DEFAULT='broken'\n", wantErr: "AGENT_TIER_DEFAULT"},
		{name: "invalid escalation", file: "[profile.daily]\nAGENT_TIER_ESCALATE='codex:model:high'\n", wantErr: "AGENT_TIER_ESCALATE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			repo := t.TempDir()
			writeProfileTestFile(t, filepath.Join(repo, "scripts/agent-gates/agent-graph.env"), tc.env)
			if tc.file != "" {
				writeProfileTestFile(t, filepath.Join(home, ".config/orca-factory/profiles.toml"), tc.file)
			}
			got, err := LoadRepository(repo)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Env, tc.want) {
				t.Fatalf("env=%v, want %v", got.Env, tc.want)
			}
		})
	}
}

func writeProfileTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProfilesRequireRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	writeProfileTestFile(t, filepath.Join(repo, "scripts/agent-gates/agent-graph.env"), "")
	path := filepath.Join(home, ".config/orca-factory/profiles.toml")
	writeProfileTestFile(t, path, "default='daily'\n[profile.daily]\nAGENT_TIER_DEFAULT='codex:original:low'\n")
	first, err := LoadRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	writeProfileTestFile(t, path, "default='daily'\n[profile.daily]\nAGENT_TIER_DEFAULT='codex:changed:high'\n")
	second, err := LoadRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	if first.Env["AGENT_TIER_DEFAULT"] != "codex:original:low" || second.Env["AGENT_TIER_DEFAULT"] != "codex:original:low" {
		t.Fatalf("profile changed without restart: first=%v second=%v", first.Env, second.Env)
	}
}

func TestMachineConfigRejectsInvalidProfiles(t *testing.T) {
	home := t.TempDir()
	writeProfileTestFile(t, filepath.Join(home, ".config/orca-factory/profiles.toml"), "[profile.unselected]\nAGENT_ROLE_REVIEWER_CMD='secret command'\n")
	_, err := Load(home)
	if err == nil || !strings.Contains(err.Error(), "AGENT_ROLE_REVIEWER_CMD") {
		t.Fatalf("invalid machine profiles accepted: %v", err)
	}
	if strings.Contains(err.Error(), "secret command") {
		t.Fatalf("error exposes profile value: %v", err)
	}
}

func TestAssignedKeysMatchParseEnv(t *testing.T) {
	data := "PLAIN=1\n  QUOTED='codex:repository:high' # comment\nexport EXPORTED=2\ndeclare -x DECLARED=3\n# COMMENTED=4\nsource /etc/profile\nunset PLAIN\n"
	env, err := ParseEnv(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	parsed := map[string]bool{}
	for key := range env {
		parsed[key] = true
	}
	if got := AssignedKeys([]byte(data)); !reflect.DeepEqual(got, parsed) {
		t.Fatalf("assigned=%v, ParseEnv keys=%v", got, parsed)
	}
}
