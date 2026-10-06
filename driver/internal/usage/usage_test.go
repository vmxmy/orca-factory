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
		{"session", "claude-sonnet-4-6", Tokens{0, 300, 50, 10, 8, 0}, 0.001683, false, false},
		{"session", "mystery-model", Tokens{0, 17, 3, 0, 0, 0}, 0, true, false},
		{"reviewer", "claude-sonnet-4-6", Tokens{0, 80, 10, 0, 0, 0}, 0.00039, false, false},
		{"coordinator", "claude-sonnet-4-6", Tokens{0, 50.5, 10.5, 0, 0, 0}, 0.000309, false, true},
		{"worker", "gpt-5", Tokens{0, 300, 80, 100, 0, 0}, 0.0011875, false, false},
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
		{name: "Codex cumulative intervals by model", files: []string{"codex-model-switch"}, want: []expectation{
			{"worker", "gpt-5", Tokens{0, 80, 10, 20, 0, 0}, 0.0002025, false, false},
			{"worker", "future-model", Tokens{0, 220, 70, 80, 0, 0}, 0, true, false},
		}},
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

// A rollout belongs to its session_meta cwd even when later turns run elsewhere.
func TestCollectCodexSessionAttribution(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(fmt.Sprint(moved), func(t *testing.T) {
			root := t.TempDir()
			unit := filepath.Join(root, "unit")
			cwd := unit
			if moved {
				cwd = filepath.Join(root, "other")
			}
			data := fmt.Sprintf(
				"{\"type\":\"session_meta\",\"payload\":{\"id\":\"session\",\"cwd\":%q}}\n"+
					"{\"type\":\"turn_context\",\"payload\":{\"model\":\"gpt-6.1-sol\",\"cwd\":%q}}\n"+
					"{\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":{\"input_tokens\":100,\"cached_input_tokens\":20,\"cache_write_input_tokens\":10,\"output_tokens\":30,\"reasoning_output_tokens\":12}}}}\n", unit, cwd)
			if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(data), 0400); err != nil {
				t.Fatal(err)
			}
			claude := filepath.Join(root, "claude")
			if err := os.Mkdir(claude, 0700); err != nil {
				t.Fatal(err)
			}
			got, err := Collect(context.Background(), "ZII-894", []string{unit}, Options{ClaudeDir: claude, CodexDir: root})
			if err != nil {
				t.Fatal(err)
			}
			if got.Input != 70 || got.CacheRead != 20 || got.CacheWrite != 10 || got.Output != 30 || got.Reasoning != 12 || got.ByRole.Worker.Reasoning != 12 || got.ByModel["gpt-6.1-sol"].Reasoning != 12 || math.Abs(got.CostUSD-0.000467) > 1e-12 {
				t.Fatalf("usage = %+v; want input=70 read=20 write=10 output=30 reasoning=12 cost=0.000467", got)
			}
		})
	}
}

func TestCollectCodexModelIntervals(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			root := t.TempDir()
			claude, codex := filepath.Join(root, "claude"), filepath.Join(root, "codex")
			for _, dir := range []string{claude, codex} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			meta := `{"type":"session_meta","payload":{"id":"interval-session","cwd":"/unit"}}` + "\n"
			turn := func(model string) string {
				return fmt.Sprintf(`{"type":"turn_context","payload":{"model":%q,"cwd":"/elsewhere"}}`+"\n", model)
			}
			count := func(input, cached, write, output, reasoning int) string {
				return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d}}}}`+"\n", input, cached, write, output, reasoning)
			}
			first, second, third := count(100, 20, 10, 30, 12), count(200, 40, 15, 50, 20), count(300, 60, 20, 80, 30)
			data := meta + turn("gpt-5") + first + first + turn("future-model") + first + second + second + turn("gpt-5") + second + third + third
			if invalid {
				data += first
			}
			if err := os.WriteFile(filepath.Join(codex, "rollout.jsonl"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := Collect(context.Background(), "ZII-894", []string{"/unit"}, Options{ClaudeDir: claude, CodexDir: codex})
			if err != nil {
				t.Fatal(err)
			}
			if invalid {
				if len(got.Skipped) != 1 || len(got.Items) != 0 {
					t.Fatalf("regressing counters must not produce a negative or duplicate charge: %+v", got)
				}
				return
			}
			if len(got.Skipped) != 0 || len(got.Items) != 2 || got.Input != 220 || got.CacheRead != 60 || got.CacheWrite != 20 || got.Output != 80 || got.Reasoning != 30 {
				t.Fatalf("session totals: %+v", got)
			}
			known, unknown := got.ByModel["gpt-5"], got.ByModel["future-model"]
			if known.Input != 145 || known.CacheRead != 40 || known.CacheWrite != 15 || known.Output != 60 || known.Reasoning != 22 || math.Abs(known.CostUSD-0.00078625) > 1e-12 {
				t.Fatalf("known model: %+v", known)
			}
			if unknown.Input != 75 || unknown.CacheRead != 20 || unknown.CacheWrite != 5 || unknown.Output != 20 || unknown.Reasoning != 8 || unknown.CostUSD != 0 {
				t.Fatalf("unknown model: %+v", unknown)
			}
			if got.ByRole.Worker != got.Totals {
				t.Fatalf("worker attribution: %+v", got)
			}
		})
	}
}
