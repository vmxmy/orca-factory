package usage

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollect(t *testing.T) {
	claude, codex := t.TempDir(), t.TempDir()
	for _, name := range []string{"claude", "review", "coordinator", "unrelated", "broken", "codex"} {
		b, err := os.ReadFile(filepath.Join("testdata", name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		dir := claude
		if name == "codex" {
			dir = codex
		}
		if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), b, 0400); err != nil {
			t.Fatal(err)
		}
	}
	options := Options{Ticket: "ZII-894", ClaudeDir: claude, CodexDir: codex, Worktrees: []Worktree{{Path: "/unit"}, {Path: "/review", Role: "reviewer"}}, RunWorktree: "/run", ClaimedTickets: 2}
	got, err := Collect(options)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ticket != "ZII-894" || got.PriceVersion == "" {
		t.Fatalf("missing provenance: %+v", got)
	}
	if len(got.Errors) != 1 || !strings.HasSuffix(got.Errors[0].Path, "broken.jsonl") {
		t.Fatalf("file errors = %+v", got.Errors)
	}
	if len(got.Items) != 5 {
		t.Fatalf("items = %+v", got.Items)
	}
	expected := map[string]struct {
		input, output, read, write, cost float64
		unknown, apportioned             bool
	}{
		"session/claude-sonnet-4-6":     {300, 50, 10, 8, 0.001683, false, false},
		"session/mystery-model":         {17, 3, 0, 0, 0, true, false},
		"reviewer/claude-sonnet-4-6":    {80, 10, 0, 0, 0.00039, false, false},
		"coordinator/claude-sonnet-4-6": {50.5, 10.5, 0, 0, 0.000309, false, true},
		"worker/gpt-5":                  {300, 80, 100, 0, 0.0011875, false, false},
	}
	for _, item := range got.Items {
		key := item.Role + "/" + item.Model
		want, ok := expected[key]
		if !ok {
			t.Fatalf("unexpected item %+v", item)
		}
		delete(expected, key)
		if item.InputTokens != want.input || item.OutputTokens != want.output || item.CacheReadTokens != want.read || item.CacheWriteTokens != want.write || math.Abs(item.CostUSD-want.cost) > 1e-12 || item.UnknownModel != want.unknown || item.Apportioned != want.apportioned {
			t.Errorf("%s = %+v; want %+v", key, item, want)
		}
	}
	if len(expected) != 0 {
		t.Fatalf("missing %+v", expected)
	}
	t.Run("invalid allocation", func(t *testing.T) {
		options.ClaimedTickets = 0
		if _, err := Collect(options); err == nil {
			t.Fatal("accepted zero ticket divisor")
		}
	})
	t.Run("large record", func(t *testing.T) {
		dir := t.TempDir()
		line := `{"type":"user","cwd":"/unit","message":"` + strings.Repeat("x", 2<<20) + `"}` + "\n"
		b, _ := os.ReadFile("testdata/claude.jsonl")
		if err := os.WriteFile(filepath.Join(dir, "large.jsonl"), append([]byte(line), b...), 0400); err != nil {
			t.Fatal(err)
		}
		r, err := Collect(Options{Ticket: "ZII-894", ClaudeDir: dir, CodexDir: t.TempDir(), Worktrees: []Worktree{{Path: "/unit"}}})
		if err != nil || len(r.Errors) != 0 || len(r.Items) != 2 {
			t.Fatalf("large file: %+v %v", r, err)
		}
	})
}
