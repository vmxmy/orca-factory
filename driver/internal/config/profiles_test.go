package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
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
		{name: "fills tier chain", file: "default='daily'\n[profile.daily]\nAGENT_TIER_CHAIN='default strong max'\nAGENT_TIER_MAX='codex:max:high'\n", want: map[string]string{"AGENT_TIER_CHAIN": "default strong max", "AGENT_TIER_MAX": "codex:max:high"}},
		{name: "invalid chain tier value", file: "[profile.daily]\nAGENT_TIER_MAX='broken'\n", wantErr: "AGENT_TIER_MAX"},
		{name: "invalid chain name", file: "[profile.daily]\nAGENT_TIER_CHAIN='default Max!'\n", wantErr: "AGENT_TIER_CHAIN"},
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

func TestFallbackKeysParse(t *testing.T) {
	got, err := ParseFallback(" claude:claude-opus-5-5:max  codex:gpt-6-astra:xhigh ")
	want := []modelhealth.Ref{{Agent: "claude", Model: "claude-opus-5-5", Effort: "max"}, {Agent: "codex", Model: "gpt-6-astra", Effort: "xhigh"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseFallback = %+v, %v; want %+v", got, err, want)
	}
	if got, err := ParseFallback(""); err != nil || got != nil {
		t.Fatalf("empty fallback = %+v, %v", got, err)
	}
	if _, err := ParseFallback("codex:gpt-6-astra"); err == nil {
		t.Fatal("candidate without effort accepted")
	}
	if got, err := ParseFallback("codex::low"); err != nil || len(got) != 1 {
		t.Fatalf("default-model candidate = %+v, %v", got, err)
	}
	for _, invalid := range []string{"gpt-secret-model", "nosuch:gpt-secret-model:high"} {
		value := "codex:gpt-6-astra:high " + invalid
		_, err := ParseFallback(value)
		if invalid == "gpt-secret-model" && err == nil {
			t.Fatalf("invalid candidate %q accepted", invalid)
		}
		if err == nil {
			err = validFallback("AGENT_ROLE_REVIEWER", value)
		}
		if err == nil || strings.Contains(err.Error(), "gpt-secret-model") || strings.Contains(err.Error(), "nosuch") {
			t.Fatalf("fallback error for %q = %v, want an error without the configured value", invalid, err)
		}
	}
	for key, want := range map[string]bool{"AGENT_TIER_MAX_FALLBACK": true, "AGENT_ROLE_RESOLVER_TOP_FALLBACK": true, "AGENT_ROLE_INTAKE_FALLBACK": true, "GITHUB_TOKEN_FALLBACK": false, "AGENT_TIER_CHAIN_FALLBACK": false, "AGENT_TIER_MAX": false} {
		if IsFallbackKey(key) != want {
			t.Errorf("IsFallbackKey(%q) = %v, want %v", key, !want, want)
		}
	}
	if FallbackKey("AGENT_ROLE_RESOLVER_TOP") != "AGENT_ROLE_RESOLVER_TOP_FALLBACK" {
		t.Fatal("fallback key name changed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	profiles := "[profile.daily]\nAGENT_TIER_DEFAULT_FALLBACK = \"codex:gpt-6.1-sol:high claude:claude-opus-5-5:high\"\nAGENT_ROLE_REVIEWER_FALLBACK = \"codex:gpt-6-astra:high\"\n"
	if err := os.WriteFile(filepath.Join(dir, "profiles.toml"), []byte(profiles), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProfiles(filepath.Join(dir, "profiles.toml")); err != nil {
		t.Fatalf("profile fallback keys rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles.toml"), []byte("[profile.daily]\nAGENT_ROLE_INTAKE_FALLBACK = \"codex:gpt-6-astra:high\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProfiles(filepath.Join(dir, "profiles.toml")); err == nil {
		t.Fatal("non-claude intake fallback accepted")
	}
}
