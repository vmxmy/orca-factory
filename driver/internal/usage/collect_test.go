package usage

import (
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
		{"session", "claude-sonnet-4-6", Tokens{300, 50, 10, 8, 0}, 0.001683, false, false},
		{"session", "mystery-model", Tokens{17, 3, 0, 0, 0}, 0, true, false},
		{"reviewer", "claude-sonnet-4-6", Tokens{80, 10, 0, 0, 0}, 0.00039, false, false},
		{"coordinator", "claude-sonnet-4-6", Tokens{50.5, 10.5, 0, 0, 0}, 0.000309, false, true},
		{"worker", "gpt-5", Tokens{300, 80, 100, 0, 0}, 0.0011875, false, false},
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
	}{
		{name: "providers roles prices and corrupt file", files: []string{"claude", "review", "coordinator", "unrelated", "broken", "codex"}, want: base, errors: 1, run: true},
		{name: "second ticket receives same half", files: []string{"coordinator"}, want: base[3:4], ticket: "ZII-895", run: true},
		{name: "duplicate session identity", files: []string{"codex"}, want: base[4:5], duplicate: true},
		{name: "duplicate Claude message", files: []string{"duplicate-message"}, want: base[:1]},
		{name: "Codex final cumulative snapshot and model", files: []string{"codex-model-switch"}, want: []expectation{{"worker", "future-model", Tokens{300, 80, 100, 0, 0}, 0, true, false}}},
		{name: "large record", files: []string{"claude"}, want: base[:2], large: true},
		{name: "missing unit worktrees", invalid: "worktrees"},
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
				data, err := os.ReadFile(filepath.Join("testdata", name+".jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				data = []byte(strings.NewReplacer("/unit-other", filepath.Join(root, "unrelated"), "/unit", unit, "/review", review, "/run", run).Replace(string(data)))
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
			opts := Options{Ticket: ticket, ClaudeDir: claude, CodexDir: codex, Worktrees: []Worktree{{Path: unit}, {Path: review, Role: "reviewer"}}}
			if tc.run {
				opts.RunWorktree = run
				opts.ClaimedTickets = []string{"ZII-894", "ZII-895", "ZII-894"}
				opts.RunID = "run-test"
				if tc.invalid != "binding" {
					opts.CoordinatorSessions = []string{"coordinator-one"}
				}
			}
			switch tc.invalid {
			case "worktrees":
				opts.Worktrees = nil
			case "ticket":
				opts.Ticket = "../ZII-894"
			}
			got, err := Collect(opts)
			if tc.invalid != "" {
				if err == nil {
					t.Fatalf("accepted missing or invalid %s: %+v", tc.invalid, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Ticket != ticket || got.PriceVersion != "2026-10-05-standard-v1" {
				t.Fatalf("provenance: %+v", got)
			}
			if len(got.Errors) != tc.errors {
				t.Fatalf("file errors: %+v", got.Errors)
			}
			if tc.errors > 0 && (!strings.HasSuffix(got.Errors[0].Path, "broken.jsonl") || got.Errors[0].Error == "") {
				t.Fatalf("error details: %+v", got.Errors)
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
