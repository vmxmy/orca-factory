package agentcli_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/agentcli"
	"github.com/vmxmy/orca-factory/driver/internal/config"
)

func TestAgentCliRegistryLookupAgy(t *testing.T) {
	want := agentcli.Profile{
		ModelFlag: "--model", EffortFlag: "--effort", PromptFlag: "--print",
		ReviewerArgs: "--mode plan", ProbeArgs: []string{"--mode", "plan"},
		CorrectionArgs: []string{"--mode", "accept-edits", "--add-dir", "{dir}"},
	}
	agentcli.Register(map[string]agentcli.Profile{"agy": want})
	got, ok := agentcli.Lookup("agy")
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup(agy) = %+v, %t; want %+v", got, ok, want)
	}
	if !agentcli.Registered("agy") || !slices.Contains(agentcli.List(), "agy") {
		t.Fatal("registered agy missing from registry")
	}
}

func TestAgentCliBuiltins(t *testing.T) {
	for _, tc := range []struct {
		agent, reviewer string
		probe           []string
	}{
		{"claude", "claude -p --model chosen --effort high --tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config",
			[]string{"-p", "--model", "chosen", "--tools", "", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence", "Reply with exactly: ok"}},
		{"codex", "env -u ORCA_PANE_KEY -u ORCA_AGENT_HOOK_PORT -u ORCA_AGENT_HOOK_TOKEN codex exec -m chosen -c model_reasoning_effort=high --sandbox read-only -",
			[]string{"exec", "-m", "chosen", "-c", "model_reasoning_effort=low", "--sandbox", "read-only", "--ephemeral", "Reply with exactly: ok"}},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			p, ok := agentcli.Lookup(tc.agent)
			if !ok {
				t.Fatal("builtin missing")
			}
			command, err := config.GenerateReviewerCommand(tc.agent + ":chosen:high")
			if err != nil || command != tc.reviewer {
				t.Fatalf("reviewer = %q, %v; want %q", command, err, tc.reviewer)
			}
			if args := p.ProbeArgv("chosen", "Reply with exactly: ok"); !slices.Equal(args, tc.probe) {
				t.Fatalf("probe = %q; want %q", args, tc.probe)
			}
		})
	}
}

func TestLoadAgyProfile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const machine = `[agents.agy]
model_flag = "--model"
effort_flag = "--effort"
prompt_flag = "--print"
reviewer_args = "--disable-slash-commands --mode plan"
probe_args = ["--sandbox", "--mode", "plan"]
correction_args = ["--mode", "accept-edits", "--add-dir", "{dir}"]
`
	if err := os.WriteFile(filepath.Join(dir, "driver.toml"), []byte(machine), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(home); err != nil {
		t.Fatal(err)
	}
	p, ok := agentcli.Lookup("agy")
	if !ok || p.ReviewerArgs != "--disable-slash-commands --mode plan" || !slices.Equal(p.ProbeArgs, []string{"--sandbox", "--mode", "plan"}) {
		t.Fatalf("driver.toml profile = %+v, %t", p, ok)
	}
	got, err := config.GenerateReviewerCommand("agy:chosen:high")
	if err != nil || got != `agy --model chosen --effort high --disable-slash-commands --mode plan --print "$(cat)"` {
		t.Fatalf("reviewer = %q, %v", got, err)
	}
	if got := p.ProbeArgv("chosen", "probe"); !slices.Equal(got, []string{"--model", "chosen", "--sandbox", "--mode", "plan", "--print", "probe"}) {
		t.Fatalf("probe = %q", got)
	}
	if got := p.CorrectionArgv("chosen", "high", "update file", "/dir"); !slices.Equal(got, []string{"--print", "update file", "--model", "chosen", "--effort", "high", "--mode", "accept-edits", "--add-dir", "/dir"}) {
		t.Fatalf("correction = %q", got)
	}
}
