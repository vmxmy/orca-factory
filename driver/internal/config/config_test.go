package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGearOverrideWinsOverRepository(t *testing.T) {
	state := t.TempDir()
	t.Setenv("AGENT_DRIVER_STATE_ROOT", state)
	const upstream = "github.com/dujiao-next/dujiao-next"
	const fork = "github.com/vmxmy/dujiao-next"
	repo := t.TempDir()
	writeProfileTestFile(t, filepath.Join(repo, "scripts/agent-gates/agent-graph.env"), "AGENT_MODE=auto\n")
	file := filepath.Join(state, "gear.json")
	for _, tc := range []struct {
		name, gears, remote   string
		mode, source, wantErr string
	}{
		{"manual beside malformed entries", `{"github.com/dujiao-next/dujiao-next":{"mode":"manual","path":"/old/checkout"},"github.com/test/scalar":7,"github.com/test/mode":{"mode":null}}`, upstream, "manual", "override", ""},
		{"auto", `{"github.com/dujiao-next/dujiao-next":{"mode":"auto"}}`, upstream, "auto", "override", ""},
		{"another remote at this path", `{"github.com/dujiao-next/dujiao-next":{"mode":"manual"}}`, fork, "auto", "repository", ""},
		{"invalid mode", `{"github.com/dujiao-next/dujiao-next":{"mode":"invalid"}}`, upstream, "", "", "manual or auto"},
		{"null mode", `{"github.com/dujiao-next/dujiao-next":{"mode":null}}`, upstream, "", "", "manual or auto"},
		{"mode key differs in case", `{"github.com/dujiao-next/dujiao-next":{"mode":"invalid","MODE":"auto"}}`, upstream, "", "", "manual or auto"},
		{"entry not an object", `{"github.com/dujiao-next/dujiao-next":"manual"}`, upstream, "", "", "manual or auto"},
		{"registration without remote identity", `{"":{"mode":"manual"}}`, "", "auto", "repository", ""},
		{"empty file object", `{}`, upstream, "auto", "repository", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeProfileTestFile(t, file, tc.gears)
			got, err := LoadRegisteredRepository(tc.remote, repo)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("load error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got.Env["AGENT_MODE"] != tc.mode || got.Sources["AGENT_MODE"] != tc.source {
				t.Fatalf("mode=%q source=%q err=%v, want %q %q", got.Env["AGENT_MODE"], got.Sources["AGENT_MODE"], err, tc.mode, tc.source)
			}
		})
	}
	if err := SetGearOverride(upstream, repo, "manual", "t", "r"); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadRegisteredRepository(upstream, repo); err != nil || got.Env["AGENT_MODE"] != "manual" || got.Sources["AGENT_MODE"] != "override" {
		t.Fatalf("after set: %v %v err=%v", got.Env["AGENT_MODE"], got.Sources["AGENT_MODE"], err)
	}
	if err := SetGearOverride(upstream, repo, "", "t", "r"); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadRegisteredRepository(upstream, repo); err != nil || got.Env["AGENT_MODE"] != "auto" || got.Sources["AGENT_MODE"] != "repository" {
		t.Fatalf("after clear: %v %v err=%v", got.Env["AGENT_MODE"], got.Sources["AGENT_MODE"], err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadRegisteredRepository(upstream, repo); err != nil || got.Sources["AGENT_MODE"] != "repository" {
		t.Fatalf("without gear.json: %v err=%v", got.Sources["AGENT_MODE"], err)
	}
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
