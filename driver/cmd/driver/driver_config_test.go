package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDriverConfigShowsEffectiveValuesWithoutSecrets runs `config --json` on
// a machine whose only eligible repository is the factory itself, with a
// driver.env holding credentials and a service the test cannot reach.
func TestDriverConfigShowsEffectiveValuesWithoutSecrets(t *testing.T) {
	const appSecret = "fake-app-secret-7f3a"
	const envSecret = "fake-driverenv-token-91c2"
	const proxyPassword = "fake-proxy-pass-55d0"
	f := newOnboardFixture(t)
	ledger, server := newWizardFeishuServer(t)
	ledger.rows = []feishuFakeRow{{id: "rec1", cells: map[string]any{
		"名称": "orca-factory", "仓库远端": "github.com/vmxmy/orca-factory", "编号前缀": "OF", "启用": true,
	}}}
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "test-app")
	t.Setenv("AGENT_FEISHU_APP_SECRET", appSecret)
	t.Setenv("AGENT_ORCA_BIN", fakeOrcaRepoList(t, f.factory, "github.com/vmxmy/orca-factory"))
	writeExecutable(t, filepath.Join(f.factory, "bin", "orca-factory"), "#!/bin/sh\nexit 0\n")
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "systemctl"), "#!/bin/sh\nexit 1\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	writeTestFile(t, filepath.Join(f.home, ".config/orca-factory/driver.toml"),
		"factory_root = \""+f.factory+"\"\nmax_in_flight = 3\n[scheduler]\npause_fraction = 0.1\nresume_fraction = 0.2\n")
	writeTestFile(t, filepath.Join(f.home, ".config/orca-factory/driver.env"),
		"AGENT_FEISHU_APP_SECRET="+envSecret+"\nAGENT_MODEL_PROXY_URL=http://operator:"+proxyPassword+"@127.0.0.1:7890\nAGENT_EMPTY=\n")
	writeTestFile(t, filepath.Join(f.factory, "scripts/agent-gates/agent-graph.env"), strings.Join([]string{
		"AGENT_ISSUE_BACKEND=feishu",
		"AGENT_FEISHU_BASE=BASE_TOKEN",
		"AGENT_FEISHU_PROJECT=github.com/vmxmy/orca-factory",
		"AGENT_TIER_DEFAULT='codex::low'",
		"AGENT_TIER_STRONG='claude:claude-opus-5-5:high'",
		"AGENT_ROLE_INTAKE='codex:gpt-6.1-sol:high'",
		"AGENT_ROLE_REVIEWER_CMD='env -u ORCA_PANE_KEY AGENT_X=1 /usr/local/bin/codex exec -m gpt-6-astra -c model_reasoning_effort=xhigh --sandbox read-only -'",
		"",
	}, "\n"))

	var code int
	out := captureStdout(t, func() { code = run([]string{"config", "--json"}) })
	if code != 0 {
		t.Fatalf("exit=%d, want 0\noutput=%s", code, out)
	}
	for _, secret := range []string{appSecret, envSecret, proxyPassword} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("output leaks %q:\n%s", secret, out)
		}
	}
	type entry struct {
		Key    string  `json:"key"`
		Value  *string `json:"value"`
		Source string  `json:"source"`
		Set    bool    `json:"set"`
		Secret bool    `json:"secret"`
	}
	type role struct{ Role, Agent, Model, Effort, Key, Raw string }
	var snapshot struct {
		Schema string `json:"schema"`
		Repos  []struct {
			Name  string `json:"name"`
			Roles []role `json:"roles"`
		} `json:"repos"`
		Scheduler   []entry  `json:"scheduler"`
		Environment []entry  `json:"environment"`
		Problems    []string `json:"problems"`
	}
	if err := json.Unmarshal(out, &snapshot); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if snapshot.Schema != "driver-config-v1" {
		t.Fatalf("schema=%q", snapshot.Schema)
	}

	secretEntry := entry{Key: "AGENT_FEISHU_APP_SECRET", Source: "driver.env", Set: true, Secret: true}
	found := false
	for _, e := range snapshot.Environment {
		if e.Key == secretEntry.Key && e.Source == "driver.env" {
			found = e == secretEntry
		}
	}
	if !found {
		t.Fatalf("environment lacks %+v without a value: %+v", secretEntry, snapshot.Environment)
	}

	if len(snapshot.Scheduler) != 8 {
		t.Fatalf("scheduler has %d keys, want 8: %+v", len(snapshot.Scheduler), snapshot.Scheduler)
	}
	scheduler := map[string]entry{}
	for _, e := range snapshot.Scheduler {
		scheduler[e.Key] = e
	}
	for key, want := range map[string][2]string{
		"max_in_flight":                {"3", "driver.toml"},
		"token_bucket.capacity":        {"4", "default"},
		"scheduler.unregistered_grace": {"3m", "default"},
	} {
		got := scheduler[key]
		if got.Value == nil || *got.Value != want[0] || got.Source != want[1] {
			t.Fatalf("scheduler %s=%+v, want value %q source %q", key, got, want[0], want[1])
		}
	}

	if len(snapshot.Repos) != 1 || snapshot.Repos[0].Name != "orca-factory" {
		t.Fatalf("repos=%+v, want only orca-factory", snapshot.Repos)
	}
	wantRoles := []role{
		{"worker_default", "codex", "", "", "AGENT_TIER_DEFAULT", "codex::low"},
		{"worker_strong", "claude", "claude-opus-5-5", "high", "AGENT_TIER_STRONG", "claude:claude-opus-5-5:high"},
		{"reviewer", "codex", "gpt-6-astra", "xhigh", "AGENT_ROLE_REVIEWER_CMD", "env -u ORCA_PANE_KEY AGENT_X=1 /usr/local/bin/codex exec -m gpt-6-astra -c model_reasoning_effort=xhigh --sandbox read-only -"},
		{"resolver", "claude", "claude-opus-5-5", "high", "AGENT_TIER_STRONG", "claude:claude-opus-5-5:high"},
		{"intake", "codex", "gpt-6.1-sol", "high", "AGENT_ROLE_INTAKE", "codex:gpt-6.1-sol:high"},
	}
	if got := snapshot.Repos[0].Roles; len(got) != len(wantRoles) {
		t.Fatalf("roles=%+v, want %+v", got, wantRoles)
	}
	for i, want := range wantRoles {
		if got := snapshot.Repos[0].Roles[i]; got != want {
			t.Fatalf("roles[%d]=%+v, want %+v", i, got, want)
		}
	}

	problems := strings.Join(snapshot.Problems, "\n")
	for _, want := range []string{"scheduler.resume_fraction", "scheduler.pause_fraction", "服务", "OF 的需求助手档位不是 claude"} {
		if !strings.Contains(problems, want) {
			t.Fatalf("problems=%q, want %q", snapshot.Problems, want)
		}
	}
}
