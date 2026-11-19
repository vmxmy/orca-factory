package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
)

func TestGearCommands(t *testing.T) {
	f := newOnboardFixture(t)
	ledger, server := newWizardFeishuServer(t)
	ledger.rows = []feishuFakeRow{{id: "rec1", cells: map[string]any{
		"名称": "orca-factory", "仓库远端": "github.com/vmxmy/orca-factory", "编号前缀": "OF", "启用": true,
	}}}
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "test-app")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "test-secret")
	t.Setenv("AGENT_ORCA_BIN", fakeOrcaRepoList(t, f.factory, "github.com/vmxmy/orca-factory"))
	writeExecutable(t, filepath.Join(f.factory, "bin", "orca-factory"), "#!/bin/sh\nexit 0\n")
	for _, args := range [][]string{{"init", f.factory}, {"-C", f.factory, "remote", "add", "origin", "https://github.com/vmxmy/orca-factory.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	profile := filepath.Join(f.factory, "scripts/agent-gates/agent-graph.env")
	original, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	invoke := func(args ...string) []byte {
		t.Helper()
		var code int
		out := captureStdout(t, func() { code = run(append([]string{"gear"}, args...)) })
		if code != 0 {
			t.Fatalf("gear %v: exit %d, output %s", args, code, out)
		}
		return out
	}
	status := func(mode, source string) {
		t.Helper()
		var rows []struct{ Remote, Prefix, Path, Mode, Source string }
		out := invoke("status", "--json")
		if err := json.Unmarshal(out, &rows); err != nil {
			t.Fatalf("status: %s %v", out, err)
		}
		if len(rows) != 1 || rows[0].Mode != mode || rows[0].Source != source || rows[0].Prefix != "OF" || rows[0].Remote != "github.com/vmxmy/orca-factory" || rows[0].Path != f.factory {
			t.Fatalf("status=%+v, want %s %s", rows, mode, source)
		}
	}
	status("manual", "default")
	invoke("set", "OF", "auto", "--operator", "tester", "--reason", "resume queue")
	status("auto", "override")
	data, err := os.ReadFile(filepath.Join(f.state, "gear.json"))
	if err != nil {
		t.Fatal(err)
	}
	var gears map[string]struct {
		Mode, Operator, Reason string
		UpdatedAt              time.Time
	}
	if err := json.Unmarshal(data, &gears); err != nil {
		t.Fatal(err)
	}
	gear := gears["github.com/vmxmy/orca-factory"]
	if gear.Mode != "auto" || gear.Operator != "tester" || gear.Reason != "resume queue" || gear.UpdatedAt.IsZero() {
		t.Fatalf("persisted gear=%+v", gear)
	}
	invoke("set", f.factory, "manual", "--operator", "tester", "--reason", "pause queue")
	status("manual", "override")
	if got, err := config.LoadRepository(f.factory); err != nil || got.Env["AGENT_MODE"] != "manual" || got.Sources["AGENT_MODE"] != "override" {
		t.Fatalf("loaded=%+v error=%v", got, err)
	}
	invoke("clear", "OF")
	status("manual", "default")
	for _, args := range [][]string{
		{"set", "OF", "auto"},
		{"set", "OF", "invalid", "--operator", "tester", "--reason", "bad"},
		{"set", "OF", "auto", "--operator", " ", "--reason", "bad"},
		{"status", "--bad"},
		{"clear"},
	} {
		if code := run(append([]string{"gear"}, args...)); code != 2 {
			t.Fatalf("gear %v exit=%d, want 2", args, code)
		}
	}
	after, err := os.ReadFile(profile)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("repository profile changed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("logs=%q, want three changes", logs.String())
	}
	for _, line := range lines {
		if !strings.Contains(line, "operator=") || !strings.Contains(line, "reason=") {
			t.Fatalf("audit fields missing: %s", line)
		}
	}
	if !strings.Contains(lines[0], `operator="tester"`) || !strings.Contains(lines[0], `reason="resume queue"`) {
		t.Fatalf("set audit=%s", lines[0])
	}
}
