package usage

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real records, trimmed to the fields the parser reads (conversation content and account
// data removed, paths redacted to /work/unit): see driver/CONTRACTS.md. The two Claude lines
// are a `claude -p` session and a subagent line; the Codex lines are one rollout.
func TestRecordedSessionFormats(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "unit")
	project := filepath.Join(root, "claude", "projects", "proj")
	sessions := filepath.Join(root, "codex", "sessions")
	for _, dir := range []string{unit, project, sessions} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	claude, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", "claude-session-assistant.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	codex, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", "codex-session-token-count.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Line 1 is the main session; line 2 is a subagent line, which Claude stores under
	// <session>/subagents/ with the parent's sessionId.
	lines := strings.SplitAfter(strings.TrimSpace(string(claude))+"\n", "\n")
	subagents := filepath.Join(project, "recorded", "subagents")
	if err := os.MkdirAll(subagents, 0700); err != nil {
		t.Fatal(err)
	}
	// Point the recorded cwds at the temp unit.
	redirect := strings.NewReplacer(
		"/work/unit", unit,
		"/home/user/orca/workspaces/orca-factory/ZII-887-executor", unit,
	)
	for dst, text := range map[string]string{
		filepath.Join(project, "recorded.jsonl"):          lines[0],
		filepath.Join(subagents, "agent-recorded.jsonl"):  lines[1],
		filepath.Join(sessions, "rollout-recorded.jsonl"): string(codex),
	} {
		if err := os.WriteFile(dst, []byte(redirect.Replace(text)), 0400); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	got, err := CollectItems(Options{Ticket: "ZII-1", Worktrees: []Worktree{{Path: unit}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Errors) != 0 {
		t.Fatalf("recorded files failed to parse: %v", got.Errors)
	}
	type want struct {
		role, model string
		tokens      Tokens
		cost        float64
	}
	// Opus 5.5 (1h cache write), Sonnet 5.5 (5m cache write) and Codex gpt-6.1-sol, all priced.
	wants := []want{
		{"session", "claude-opus-5-5", Tokens{InputTokens: 2, OutputTokens: 320, CacheReadTokens: 29090, CacheWriteTokens: 0, CacheWriteHourTokens: 36610}, 0.305106},
		{"session", "claude-sonnet-5-5", Tokens{InputTokens: 2, OutputTokens: 24, CacheReadTokens: 16952, CacheWriteTokens: 16163, CacheWriteHourTokens: 0}, 0.0440419},
		{"worker", "gpt-6.1-sol", Tokens{InputTokens: 58412, OutputTokens: 7756, CacheReadTokens: 671488}, 0.2615328},
	}
	if len(got.Items) != len(wants) {
		t.Fatalf("items=%+v", got.Items)
	}
	for _, w := range wants {
		found := false
		for _, item := range got.Items {
			if item.Role == w.role && item.Model == w.model {
				found = true
				if item.Tokens != w.tokens || math.Abs(item.CostUSD-w.cost) > 1e-9 || item.UnknownModel {
					t.Errorf("%s %s: tokens=%+v cost=%v unknown=%v; want %+v cost %v", w.role, w.model, item.Tokens, item.CostUSD, item.UnknownModel, w.tokens, w.cost)
				}
			}
		}
		if !found {
			t.Errorf("no item for %s %s in %+v", w.role, w.model, got.Items)
		}
	}
	ticket, err := Collect(context.Background(), "ZII-1", []string{unit})
	if err != nil || ticket.Output != 320+24+7756 || ticket.ByRole.Worker.Output != 7756 || ticket.Totals.CacheWrite != 36610+16163 {
		t.Fatalf("ticket usage=%+v err=%v", ticket, err)
	}
}
