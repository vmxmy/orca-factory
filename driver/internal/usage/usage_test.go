package usage

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollect(t *testing.T) {
	type expectation struct {
		role, model          string
		tokens               Tokens
		cost                 float64
		unknown, apportioned bool
	}
	base := []expectation{
		{"session", "claude-sonnet-4-6", Tokens{300, 50, 10, 8, 0, 0}, 0.001683, false, false},
		{"session", "mystery-model", Tokens{17, 3, 0, 0, 0, 0}, 0, true, false},
		{"reviewer", "claude-sonnet-4-6", Tokens{80, 10, 0, 0, 0, 0}, 0.00039, false, false},
		{"coordinator", "claude-sonnet-4-6", Tokens{50.5, 10.5, 0, 0, 0, 0}, 0.000309, false, true},
		{"worker", "gpt-5", Tokens{300, 80, 100, 0, 0, 0}, 0.0011875, false, false},
	}
	cases := []struct {
		name             string
		files            []string
		want             []expectation
		errors           int
		ticket           string
		run              bool
		duplicate, large bool
		invalid          string
		noReviewer       bool
		noWorktrees      bool
	}{
		{name: "Claude accumulates usage", files: []string{"claude-session"}, want: base[:2]},
		{name: "Codex takes final cumulative usage", files: []string{"codex-session"}, want: base[4:5]},
		{name: "no automatic reviewer discovery", files: []string{"claude-session", "review", "codex-session", "unrelated"}, want: []expectation{base[0], base[1], base[4]}, noReviewer: true},
		{name: "unknown model has zero cost", files: []string{"claude-session"}, want: base[:2]},
		{name: "corrupt file is skipped", files: []string{"claude-session", "corrupt"}, want: base[:2], errors: 1},
		{name: "Claude and Codex roles prices and corrupt file", files: []string{"claude-session", "review", "coordinator", "unrelated", "corrupt", "codex-session"}, want: base, errors: 1, run: true},
		{name: "second ticket receives same half", files: []string{"coordinator"}, want: base[3:4], ticket: "ZII-895", run: true},
		{name: "duplicate session identity", files: []string{"codex-session"}, want: base[4:5], duplicate: true},
		{name: "duplicate Claude message", files: []string{"duplicate-message"}, want: base[:1]},
		{name: "Codex final cumulative snapshot and model", files: []string{"codex-model-switch"}, want: []expectation{{"worker", "future-model", Tokens{300, 80, 100, 0, 0, 0}, 0, true, false}}},
		{name: "large record", files: []string{"claude-session"}, want: base[:2], large: true},
		{name: "no matching unit worktrees", files: []string{"claude-session", "codex-session"}, noWorktrees: true, noReviewer: true},
		{name: "invalid ticket", invalid: "ticket"},
		{name: "missing coordinator session binding", invalid: "binding", run: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			unit, review, run := filepath.Join(root, "unit"), filepath.Join(root, "review"), filepath.Join(root, "run")
			claude, codex := filepath.Join(root, "claude"), filepath.Join(root, "codex")
			for _, dir := range []string{unit, review, run, claude, codex} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range tc.files {
				data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", "usage-"+name+".jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				data = []byte(strings.NewReplacer("/unit-other", filepath.Join(root, "unit-other"), "/unit", filepath.Join(unit, "nested"), "/review", review, "/run", run).Replace(string(data)))
				dir := claude
				if strings.HasPrefix(name, "codex") {
					dir = codex
				}
				if tc.large {
					data = append([]byte(`{"type":"user","cwd":"`+unit+`","message":"`+strings.Repeat("x", 2<<20)+`"}`+"\n"), data...)
				}
				if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), data, 0400); err != nil {
					t.Fatal(err)
				}
				if tc.duplicate {
					if err := os.WriteFile(filepath.Join(dir, "duplicate.jsonl"), data, 0400); err != nil {
						t.Fatal(err)
					}
				}
			}
			ticket := tc.ticket
			if ticket == "" {
				ticket = "ZII-894"
			}
			opts := Options{Ticket: ticket, ClaudeDir: claude, CodexDir: codex, Worktrees: []Worktree{{Path: unit}}, ReviewerWorktree: review}
			if tc.noReviewer {
				opts.ReviewerWorktree = ""
			}
			if tc.run {
				opts.RunWorktree = run
				opts.ClaimedTicketCount = 2
				if tc.invalid != "binding" {
					opts.CoordinatorSession = "coordinator-one"
				}
			}
			if tc.noWorktrees {
				opts.Worktrees = nil
			}
			if tc.invalid == "ticket" {
				opts.Ticket = "../ZII-894"
			}
			paths := []string{}
			for _, tree := range opts.Worktrees {
				paths = append(paths, tree.Path)
			}
			got, err := Collect(context.Background(), opts.Ticket, paths, opts)
			if tc.invalid != "" {
				if err == nil {
					t.Fatalf("accepted missing or invalid %s: %+v", tc.invalid, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Ticket != ticket || got.PriceVersion != "2026-10-06-standard-v2" {
				t.Fatalf("provenance: %+v", got)
			}
			if len(got.Skipped) != tc.errors {
				t.Fatalf("file errors: %+v", got.Skipped)
			}
			if tc.errors > 0 && (!strings.HasSuffix(got.Skipped[0].Path, "corrupt.jsonl") || got.Skipped[0].Error == "") {
				t.Fatalf("error details: %+v", got.Skipped)
			}
			if len(got.Items) != len(tc.want) {
				t.Fatalf("items: %+v, want %+v", got.Items, tc.want)
			}
			for _, want := range tc.want {
				found := false
				for _, item := range got.Items {
					if item.Role == want.role && item.Model == want.model {
						found = true
						if item.Tokens != want.tokens || math.Abs(item.CostUSD-want.cost) > 1e-12 || item.UnknownModel != want.unknown || item.Apportioned != want.apportioned {
							t.Errorf("item: %+v, want %+v", item, want)
						}
					}
				}
				if !found {
					t.Errorf("missing %+v in %+v", want, got.Items)
				}
			}
		})
	}
}

func TestCollectFillsTicketUsage(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "unit")
	claudeProjects := filepath.Join(root, "claude", "projects")
	codexSessions := filepath.Join(root, "codex", "sessions")
	for _, dir := range []string{unit, claudeProjects, codexSessions} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, dir := range map[string]string{"claude": claudeProjects, "codex": codexSessions} {
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", "usage-"+map[string]string{"claude": "claude-session", "codex": "codex-session"}[name]+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.NewReplacer("/unit", unit).Replace(string(data)))
		if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), data, 0400); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	got, err := Collect(context.Background(), "ZII-894", []string{unit})
	if err != nil {
		t.Fatal(err)
	}
	if got.Ticket != "ZII-894" || got.PriceVersion == "" || got.Apportioned {
		t.Fatalf("header=%+v", got)
	}
	want := Totals{Input: 617, Output: 133, CacheRead: 110, CacheWrite: 8}
	if g := got.Totals; g.Input != want.Input || g.Output != want.Output || g.CacheRead != want.CacheRead || g.CacheWrite != want.CacheWrite || math.Abs(g.CostUSD-0.0028705) > 1e-9 {
		t.Fatalf("totals=%+v want %+v cost 0.0028705", g, want)
	}
	if got.ByRole.Worker.Input != 300 || got.ByRole.Worker.CacheRead != 100 || got.ByRole.Session.Input != 317 || got.ByRole.Reviewer.Input != 0 {
		t.Fatalf("by_role=%+v", got.ByRole)
	}
	if got.ByModel["gpt-5"].Output != 80 || got.ByModel["claude-sonnet-4-6"].Input != 300 || got.ByModel["mystery-model"].CostUSD != 0 {
		t.Fatalf("by_model=%+v", got.ByModel)
	}
	if empty, err := Collect(context.Background(), "ZII-894", nil); err != nil || empty.Ticket != "ZII-894" || empty.PriceVersion != "2026-10-06-standard-v2" || len(empty.Items) != 0 || len(empty.Skipped) != 0 || empty.Totals != (Totals{}) {
		t.Fatalf("no worktrees: usage = %+v, error = %v; want empty usage", empty, err)
	}
}

// Real Claude layout: <project>/<sessionId>.jsonl plus <project>/<sessionId>/subagents/agent-*.jsonl.
// Subagent records keep the parent's sessionId (isSidechain true) but carry their own message
// IDs, so the usage of both files counts, while a message replayed in both counts once.
func TestCollectCountsSubagentTranscripts(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "unit")
	project := filepath.Join(root, "claude", "projects", "proj")
	subagents := filepath.Join(project, "sess-1", "subagents")
	for _, dir := range []string{unit, subagents} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	line := func(sidechain bool, msgID string, in, out int) string {
		return fmt.Sprintf(`{"type":"assistant","sessionId":"sess-1","isSidechain":%t,"cwd":%q,"message":{"id":%q,"model":"claude-sonnet-4-6","role":"assistant","usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n", sidechain, unit, msgID, in, out)
	}
	parent := line(false, "msg_a", 100, 10) + line(false, "msg_shared", 50, 5)
	sub := line(true, "msg_b", 200, 20) + line(true, "msg_shared", 50, 5)
	if err := os.WriteFile(filepath.Join(project, "sess-1.jsonl"), []byte(parent), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subagents, "agent-x.jsonl"), []byte(sub), 0400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	got, err := Collect(context.Background(), "ZII-894", []string{unit})
	if err != nil {
		t.Fatal(err)
	}
	if got.Input != 350 || got.Output != 35 {
		t.Fatalf("input=%d output=%d; want 350/35 (parent 100+50, subagent 200, replayed message once)", got.Input, got.Output)
	}
}
