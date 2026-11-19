package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGearOverrideWinsOverRepository(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state := t.TempDir()
	t.Setenv("AGENT_DRIVER_STATE_ROOT", state)
	recording, err := os.ReadFile("../../testdata/recorded/git-remote-get-url-dujiao-next.txt")
	if err != nil {
		t.Fatal(err)
	}
	checkout := func(url string) string {
		t.Helper()
		path := t.TempDir()
		commands := [][]string{{"init", "--quiet"}}
		if url != "" {
			commands = append(commands, []string{"remote", "add", "origin", url})
		}
		for _, args := range commands {
			cmd := exec.Command("git", args...)
			cmd.Dir = path
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		return path
	}
	const origin = "github.com/vmxmy/dujiao-next"
	const upstream = "github.com/dujiao-next/dujiao-next"
	const otherOrigin = "github.com/vmxmy/orca-factory"
	repo := checkout(strings.TrimSpace(string(recording)))
	sameOrigin := checkout(strings.TrimSpace(string(recording)))
	otherRepo := checkout("https://github.com/vmxmy/orca-factory.git")
	formerPath := filepath.Join(t.TempDir(), "former")
	writeProfileTestFile(t, filepath.Join(repo, "scripts/agent-gates/agent-graph.env"), "AGENT_MODE=auto\n")
	file := filepath.Join(state, "gear.json")
	entry := func(origin, path string, mode any) map[string]any {
		return map[string]any{"origin": origin, "path": path, "mode": mode}
	}
	checkMode := func(t *testing.T, wantMode, wantSource string) {
		t.Helper()
		got, err := LoadRepository(repo)
		if err != nil {
			t.Fatal(err)
		}
		if got.Env["AGENT_MODE"] != wantMode || got.Sources["AGENT_MODE"] != wantSource {
			t.Fatalf("mode=%q source=%q, want %q %q", got.Env["AGENT_MODE"], got.Sources["AGENT_MODE"], wantMode, wantSource)
		}
	}
	for _, tc := range []struct {
		name                  string
		entries               map[string]any
		mode, source, wantErr string
	}{
		{"manual with unrelated malformed entries", map[string]any{
			upstream: entry(origin, repo, "manual"), otherOrigin: entry(otherOrigin, repo, nil),
			"github.com/test/scalar": 7, "github.com/test/path": map[string]any{"origin": origin, "path": 1},
			"github.com/test/origin": map[string]any{"origin": 2, "path": repo},
		}, "manual", "override", ""},
		{"auto", map[string]any{upstream: entry(origin, repo, "auto")}, "auto", "override", ""},
		{"checkout moved away from recorded path", map[string]any{upstream: entry(origin, formerPath, "manual")}, "auto", "repository", ""},
		{"recorded path now holds another repository", map[string]any{upstream: entry(origin, otherRepo, "manual")}, "auto", "repository", ""},
		{"different origin at this path", map[string]any{otherOrigin: entry(otherOrigin, repo, "manual")}, "auto", "repository", ""},
		{"another checkout still owns origin", map[string]any{origin: entry(origin, sameOrigin, "manual")}, "auto", "repository", ""},
		{"invalid mode", map[string]any{upstream: entry(origin, repo, "invalid")}, "", "", "manual or auto"},
		{"empty mode", map[string]any{upstream: entry(origin, repo, "")}, "", "", "manual or auto"},
		{"null mode", map[string]any{upstream: entry(origin, repo, nil)}, "", "", "manual or auto"},
		{"duplicate", map[string]any{upstream: entry(origin, repo, "manual"), origin: entry(origin, repo, "auto")}, "", "", "ambiguous gear override"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.entries)
			if err != nil {
				t.Fatal(err)
			}
			writeProfileTestFile(t, file, string(data))
			if tc.wantErr != "" {
				if _, err := LoadRepository(repo); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("load error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			checkMode(t, tc.mode, tc.source)
		})
	}
	noOrigin := checkout("")
	writeProfileTestFile(t, filepath.Join(noOrigin, "scripts/agent-gates/agent-graph.env"), "AGENT_MODE=auto\n")
	writeProfileTestFile(t, file, fmt.Sprintf(`{"local/no-origin":{"origin":"","path":%q,"mode":"manual"}}`, noOrigin))
	if got, err := LoadRepository(noOrigin); err != nil || got.Env["AGENT_MODE"] != "manual" || got.Sources["AGENT_MODE"] != "override" {
		t.Fatalf("checkout without origin: %+v, err=%v", got.Env, err)
	}
	writeProfileTestFile(t, file, fmt.Sprintf(`{%q:{"origin":%q,"path":%q,"mode":"manual"}}`, upstream, origin, repo))
	unreadable := filepath.Join(repo, ".git", "config")
	gitConfig, err := os.ReadFile(unreadable)
	if err != nil {
		t.Fatal(err)
	}
	writeProfileTestFile(t, unreadable, "[[[\n")
	if _, err := LoadRepository(repo); err == nil {
		t.Fatal("unreadable git config dropped the manual override")
	}
	writeProfileTestFile(t, unreadable, string(gitConfig))
	t.Setenv("PATH", t.TempDir())
	if _, err := LoadRepository(repo); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("override for this path with missing git: got %v, want executable not found", err)
	}
	writeProfileTestFile(t, file, "{}")
	checkMode(t, "auto", "repository")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	checkMode(t, "auto", "repository")
}

func TestGearChangeWithoutOrigin(t *testing.T) {
	for _, cause := range []string{"no repository", "git unavailable"} {
		t.Run(cause, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("AGENT_DRIVER_STATE_ROOT", state)
			if cause == "git unavailable" {
				t.Setenv("PATH", t.TempDir())
			}
			path := t.TempDir()
			file := filepath.Join(state, "gear.json")
			const original = `{"DJ":{"mode":"bogus"},"other":[1, 2]}`
			writeProfileTestFile(t, file, original)
			if err := SetGearOverride("DJ", path, "manual", "t", "r"); err == nil {
				t.Fatal("gear set accepted a checkout without an origin")
			}
			if got, err := os.ReadFile(file); err != nil || string(got) != original {
				t.Fatalf("failed set changed gear.json: %s, err=%v", got, err)
			}
			if err := SetGearOverride("DJ", path, "", "t", "r"); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(file)
			if err != nil || strings.TrimSpace(string(got)) != `{"other":[1, 2]}` {
				t.Fatalf("clear without origin: %s, err=%v", got, err)
			}
		})
	}
	t.Run("repository without origin remote", func(t *testing.T) {
		state := t.TempDir()
		t.Setenv("AGENT_DRIVER_STATE_ROOT", state)
		path := t.TempDir()
		if out, err := exec.Command("git", "-C", path, "init", "--quiet").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		if err := SetGearOverride("local/repo", path, "manual", "t", "r"); err != nil {
			t.Fatal(err)
		}
		var gears map[string]gearOverride
		data, err := os.ReadFile(filepath.Join(state, "gear.json"))
		if err != nil || json.Unmarshal(data, &gears) != nil {
			t.Fatalf("gear.json: %s, err=%v", data, err)
		}
		if got := gears["local/repo"]; got.Path != filepath.Clean(path) || got.Origin != "" || got.Mode != "manual" {
			t.Fatalf("set without origin remote: %+v", got)
		}
	})
}

func TestDefaultMachineLimit(t *testing.T) {
	home := t.TempDir()
	c, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxInFlight != 4 {
		t.Fatalf("limit=%d, want 4", c.MaxInFlight)
	}
}

func TestEnvIsData(t *testing.T) {
	env, err := ParseEnv(strings.NewReader("# profile\nAGENT_MAX_IN_FLIGHT=3 # cap\nAGENT_INSTALL_CMD='echo # literal' # comment\nEMPTY=''\nEVIL='$(touch forbidden)'\nsource /etc/profile\nexport NO=ignored\n"))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"AGENT_MAX_IN_FLIGHT": "3", "AGENT_INSTALL_CMD": "echo # literal", "EMPTY": "", "EVIL": "$(touch forbidden)"} {
		if got, ok := env[key]; !ok || got != want {
			t.Fatalf("%s=%q, want %q", key, got, want)
		}
	}
	if _, ok := env["NO"]; ok {
		t.Fatal("parsed shell statement")
	}
}

func TestMachineAndRepositoryConfig(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "scripts", "agent-gates"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scripts", "agent-gates", "agent-graph.env"), []byte("AGENT_MAX_IN_FLIGHT=2\nAGENT_STALE_HOURS=8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// A deployed driver.toml may still carry the retired repositories key; it
	// must load and must not read the listed path (OF-43).
	body := "max_in_flight=6\nrepositories=[\"/missing\"]\n[token_bucket]\ncapacity=3\nrefill_per_second=0.5\n"
	if err := os.WriteFile(filepath.Join(dir, "driver.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxInFlight != 6 || c.TokenBucket.Capacity != 3 || c.TokenBucket.RefillPerSecond != 0.5 {
		t.Fatalf("configuration: %+v", c)
	}
	r, err := LoadRepository(repo)
	if err != nil || r.MaxInFlight != 2 || r.StaleHours != 8 {
		t.Fatalf("repository: %+v %v", r, err)
	}
}

func TestRepositoryAgentMode(t *testing.T) {
	for _, tc := range []struct {
		name, assignment, want string
		invalid                bool
	}{
		{"default", "", "", false},
		{"manual", "AGENT_MODE=manual\n", "manual", false},
		{"auto", "AGENT_MODE=auto\n", "auto", false},
		{"empty", "AGENT_MODE=\n", "", true},
		{"unknown", "AGENT_MODE=automatic\n", "", true},
		{"case sensitive", "AGENT_MODE=AUTO\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			dir := filepath.Join(repo, "scripts", "agent-gates")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "agent-graph.env"), []byte(tc.assignment), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadRepository(repo)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "AGENT_MODE") {
					t.Fatalf("invalid mode must fail with its key: %v", err)
				}
				return
			}
			if err != nil || got.Env["AGENT_MODE"] != tc.want {
				t.Fatalf("mode=%q err=%v, want %q", got.Env["AGENT_MODE"], err, tc.want)
			}
		})
	}
}

func TestRejectNonFiniteTokenRefill(t *testing.T) {
	for _, value := range []string{"nan", "inf", "-inf"} {
		t.Run(value, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, ".config", "orca-factory")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "driver.toml"), []byte("[token_bucket]\nrefill_per_second="+value+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(home); err == nil {
				t.Fatal("accepted nonfinite refill")
			}
		})
	}
}

// Tuning sections override only named fields; the rest keep defaults, and an absent
// section keeps the whole default.
func TestTuningOverridesNamedFields(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := "max_in_flight=4\nrepositories=[]\n[scheduler]\nunregistered_grace=\"2m\"\n[orca]\nworker_start_wait=\"3m\"\n"
	if err := os.WriteFile(filepath.Join(dir, "driver.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tuning.Scheduler.UnregisteredGrace != 2*time.Minute {
		t.Fatalf("scheduler tuning: %+v", c.Tuning.Scheduler)
	}
	if c.Tuning.Orca.WorkerStartWait != 3*time.Minute || c.Tuning.Orca.CallTimeout != 30*time.Second {
		t.Fatalf("orca tuning: %+v", c.Tuning.Orca)
	}
	if c.Tuning.GitHub.HTTPTimeout != 30*time.Second || c.Tuning.Ledger.FlushInterval != 10*time.Minute {
		t.Fatalf("untouched sections must keep defaults: %+v", c.Tuning)
	}
}

// Without any [tuning] section, Load returns the built-in defaults.
func TestTuningDefaultsWhenAbsent(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "driver.toml"), []byte("max_in_flight=4\nrepositories=[]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tuning.Scheduler.UnregisteredGrace != 3*time.Minute {
		t.Fatalf("scheduler defaults: %+v", c.Tuning.Scheduler)
	}
}
