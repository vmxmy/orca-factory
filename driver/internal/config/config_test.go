package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	if err := os.WriteFile(filepath.Join(repo, "scripts", "agent-gates", "agent-graph.env"), []byte("AGENT_MAX_IN_FLIGHT=2\nAGENT_STALE_HOURS=8\nAGENT_LINEAR_PROJECT=project-id\n"), 0600); err != nil {
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
	body := "max_in_flight=4\nrepositories=[]\n[scheduler]\npause_fraction=0.02\nresume_fraction=0.01\nquota_probe_backoff=\"2m\"\n[orca]\nworker_start_wait=\"3m\"\n"
	if err := os.WriteFile(filepath.Join(dir, "driver.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tuning.Scheduler.PauseFraction != 0.02 || c.Tuning.Scheduler.ResumeFraction != 0.01 {
		t.Fatalf("scheduler tuning: %+v", c.Tuning.Scheduler)
	}
	if c.Tuning.Scheduler.QuotaProbeBackoff != 2*time.Minute {
		t.Fatalf("quota backoff: %v", c.Tuning.Scheduler.QuotaProbeBackoff)
	}
	if c.Tuning.Scheduler.UnregisteredGrace != 3*time.Minute {
		t.Fatalf("unregistered grace must keep default: %v", c.Tuning.Scheduler.UnregisteredGrace)
	}
	if c.Tuning.Orca.WorkerStartWait != 3*time.Minute || c.Tuning.Orca.CallTimeout != 30*time.Second {
		t.Fatalf("orca tuning: %+v", c.Tuning.Orca)
	}
	if c.Tuning.Linear.HTTPTimeout != 30*time.Second || c.Tuning.Ledger.FlushInterval != 10*time.Minute {
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
	if c.Tuning.Scheduler.PauseFraction != .05 || c.Tuning.Scheduler.ResumeFraction != .10 {
		t.Fatalf("scheduler defaults: %+v", c.Tuning.Scheduler)
	}
	if c.Tuning.Status.StaleAfter != 5*time.Minute || c.Tuning.Runx.WaitDelay != 100*time.Millisecond {
		t.Fatalf("defaults: %+v", c.Tuning)
	}
}
