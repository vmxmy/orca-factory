package config

import "testing"

func TestReviewerCommandMatchesExistingSamples(t *testing.T) {
	tests := []struct {
		name, value, want string
	}{
		{"codex", "codex:gpt-6-astra:high", "env -u ORCA_PANE_KEY -u ORCA_AGENT_HOOK_PORT -u ORCA_AGENT_HOOK_TOKEN codex exec -m gpt-6-astra -c model_reasoning_effort=high --sandbox read-only -"},
		{"claude", "claude:claude-fable-5-1:high", "claude -p --model claude-fable-5-1 --effort high --tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config"},
		{"codex default model keeps effort", "codex::high", "env -u ORCA_PANE_KEY -u ORCA_AGENT_HOOK_PORT -u ORCA_AGENT_HOOK_TOKEN codex exec -c model_reasoning_effort=high --sandbox read-only -"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GenerateReviewerCommand(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("command=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestReviewerCommandsPreferRepositoryStructuredThenCommandThenProfile(t *testing.T) {
	structured := "codex:structured:high"
	legacy := "claude -p --model legacy --effort low --tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config"
	profile := "claude:profile:medium"
	for _, tc := range []struct {
		name, want   string
		env, sources map[string]string
	}{
		{"repository structured", "env -u ORCA_PANE_KEY -u ORCA_AGENT_HOOK_PORT -u ORCA_AGENT_HOOK_TOKEN codex exec -m structured -c model_reasoning_effort=high --sandbox read-only -", map[string]string{"AGENT_ROLE_REVIEWER": structured, "AGENT_ROLE_REVIEWER_CMD": legacy}, map[string]string{"AGENT_ROLE_REVIEWER": "repository", "AGENT_ROLE_REVIEWER_CMD": "repository"}},
		{"repository command", legacy, map[string]string{"AGENT_ROLE_REVIEWER": profile, "AGENT_ROLE_REVIEWER_CMD": legacy}, map[string]string{"AGENT_ROLE_REVIEWER": "profile:daily", "AGENT_ROLE_REVIEWER_CMD": "repository"}},
		{"profile structured", "claude -p --model profile --effort medium --tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config", map[string]string{"AGENT_ROLE_REVIEWER": profile}, map[string]string{"AGENT_ROLE_REVIEWER": "profile:daily"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveReviewerCommand(tc.env, tc.sources, "AGENT_ROLE_REVIEWER", "AGENT_ROLE_REVIEWER_CMD")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("command=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestReviewerCommandRejectsUnsupportedAgent(t *testing.T) {
	if _, err := GenerateReviewerCommand("gemini:model:high"); err == nil {
		t.Fatal("unsupported reviewer agent accepted")
	}
}
