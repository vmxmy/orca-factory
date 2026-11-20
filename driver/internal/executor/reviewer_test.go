package executor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type reviewerTools struct {
	*fakeTools
	commands *Commands
}

func (r *reviewerTools) Review(ctx context.Context, repo, base, head string, estimate *float64) (string, string, error) {
	return r.commands.Review(ctx, repo, base, head, estimate)
}

func TestReviewerByEstimate(t *testing.T) {
	two, three, zero := 2.0, 3.0, 0.0
	for _, tc := range []struct {
		name                 string
		estimate             *float64
		maximum              *float64
		small, command, tier string
	}{
		{"at default threshold", &two, nil, "small-review", `small-review`, "small"},
		{"above threshold", &three, nil, "small-review", `main-review`, "default"},
		{"empty small low", &zero, nil, "", `main-review`, "default"},
		{"empty small high", &three, nil, "", `main-review`, "default"},
		{"missing estimate", nil, nil, "small-review", `main-review`, "default"},
		{"custom threshold", &three, &three, "small-review", `small-review`, "small"},
		{"zero threshold", &two, &zero, "small-review", `main-review`, "default"},
		{"zero estimate", &zero, &zero, "small-review", `small-review`, "small"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, comments, _, baseTools, ticket := fixture(t, "new-sha")
			ticket.Estimate = tc.estimate
			calls := 0
			commands := &Commands{ReviewerCommand: "main-review", ReviewerSmallCommand: tc.small, ReviewerSmallMaxEstimate: tc.maximum}
			commands.Run = func(_ context.Context, spec runx.Spec) ([]byte, error) {
				if spec.Path == "git" {
					return []byte("diff"), nil
				}
				calls++
				if spec.Path != "/bin/sh" || len(spec.Args) != 5 || spec.Args[3] != tc.command {
					t.Fatalf("review command = %+v; want %q", spec, tc.command)
				}
				return []byte(`{"verdict":"green","step":"review"}`), nil
			}
			commands.Run = withReviewerRole(t, commands.Run)
			e.Tools = &reviewerTools{fakeTools: baseTools, commands: commands}
			if _, err := e.Execute(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("review calls = %d; want 1", calls)
			}
			if len(comments.comments) != 6 {
				t.Fatalf("comment count = %d; want 6", len(comments.comments))
			}
			if got := comments.comments[4]; got != "阶段: 开 PR\n审查命令档位: "+tc.tier {
				t.Fatalf("review completion comment = %q", got)
			}
			for i, body := range comments.comments {
				if i != 4 && strings.Contains(body, "审查命令档位:") {
					t.Fatalf("tier on unrelated transition: %q", body)
				}
			}
		})
	}
	t.Run("empty review exhausts budget", func(t *testing.T) {
		e, _, comments, _, baseTools, ticket := fixture(t, "new-sha")
		e.Sleep = func(context.Context, time.Duration) error { return nil }
		ticket.Estimate = &two
		commands := &Commands{ReviewerCommand: "main-review", ReviewerSmallCommand: "small-review"}
		commands.Run = withReviewerRole(t, func(context.Context, runx.Spec) ([]byte, error) { return nil, nil })
		e.Tools = &reviewerTools{fakeTools: baseTools, commands: commands}
		if _, err := e.Execute(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		commands.Run = withReviewerRole(t, commands.Run)
		last := comments.comments[len(comments.comments)-1]
		if !strings.Contains(last, "审查命令档位: small") {
			t.Fatalf("review handoff lost actual tier: %q", last)
		}
	})

}

func TestReviewerRetriesNextCandidateOnModelError(t *testing.T) {
	limit, err := os.ReadFile("../../testdata/recorded/claude-model-limit.txt")
	if err != nil {
		t.Fatal(err)
	}
	primary := modelhealth.Ref{Agent: "claude", Model: "claude-opus-5-5", Effort: "high"}
	fallback := modelhealth.Ref{Agent: "codex", Model: "gpt-6-astra", Effort: "high"}
	want, err := config.GenerateReviewerCommand(fallback.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		output []byte
		runs   int
	}{
		{"model limit", limit, 2},
		{"other failure", []byte("reviewer crashed"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			var commands []string
			c := &Commands{ReviewerCommand: "claude -p --model claude-opus-5-5", ReviewerCandidates: []modelhealth.Ref{primary, fallback}, StateRoot: state, Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
				if s.Path != "/bin/sh" {
					return nil, nil
				}
				commands = append(commands, s.Args[3])
				if len(commands) == 1 {
					return tc.output, errors.New("exit status 1")
				}
				return []byte(`{"verdict":"green","step":"review"}`), nil
			}}
			c.Run = withReviewerRole(t, c.Run)
			_, _, err := c.Review(context.Background(), t.TempDir(), "origin/main", "HEAD", nil)
			if len(commands) != tc.runs || (tc.runs == 1) != (err != nil) {
				t.Fatalf("runs=%d err=%v commands=%q", len(commands), err, commands)
			}
			if tc.runs == 2 && commands[1] != want {
				t.Fatalf("retry command=%q want %q", commands[1], want)
			}
		})
	}
}

// Every candidate whose run shows a model error is recorded: a lone candidate
// and a fallback that fails the retry too.
func TestReviewerRecordsEveryModelErrorRun(t *testing.T) {
	limit, err := os.ReadFile("../../testdata/recorded/claude-model-limit.txt")
	if err != nil {
		t.Fatal(err)
	}
	primary := modelhealth.Ref{Agent: "claude", Model: "claude-opus-5-5", Effort: "high"}
	fallback := modelhealth.Ref{Agent: "claude", Model: "claude-sonnet-5-5", Effort: "high"}
	for _, tc := range []struct {
		name       string
		candidates []modelhealth.Ref
		runs       int
	}{
		{"lone candidate", []modelhealth.Ref{primary}, 1},
		{"fallback limited too", []modelhealth.Ref{primary, fallback}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			runs := 0
			c := &Commands{ReviewerCommand: "claude -p --model claude-opus-5-5", ReviewerCandidates: tc.candidates, StateRoot: state, Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
				if s.Path != "/bin/sh" {
					return nil, nil
				}
				runs++
				return limit, errors.New("exit status 1")
			}}
			c.Run = withReviewerRole(t, c.Run)
			if _, _, err := c.Review(context.Background(), t.TempDir(), "origin/main", "HEAD", nil); err == nil || runs != tc.runs {
				t.Fatalf("runs=%d err=%v", runs, err)
			}
			registry, err := modelhealth.Open(state)
			if err != nil {
				t.Fatal(err)
			}
			for _, ref := range tc.candidates {
				if s := registry.Status(ref, time.Now()); s.State != modelhealth.Unavailable {
					t.Fatalf("%s not recorded: %+v", ref, s)
				}
			}
		})
	}
}

// A structured reviewer role without fallback records its model error; a
// hand-written command names no model to record.
func TestReviewerWithoutFallbackRecordsModelError(t *testing.T) {
	limit, err := os.ReadFile("../../testdata/recorded/claude-model-limit.txt")
	if err != nil {
		t.Fatal(err)
	}
	primary := modelhealth.Ref{Agent: "claude", Model: "claude-opus-5-5", Effort: "high"}
	structured, err := config.GenerateReviewerCommand(primary.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		command  string
		recorded bool
	}{{structured, true}, {"claude -p --model claude-opus-5-5 --effort high", false}} {
		state := t.TempDir()
		c := &Commands{ReviewerCommand: tc.command, StateRoot: state, Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
			if s.Path != "/bin/sh" {
				return nil, nil
			}
			return limit, errors.New("exit status 1")
		}}
		c.Run = withReviewerRole(t, c.Run)
		if _, _, err := c.Review(context.Background(), t.TempDir(), "origin/main", "HEAD", nil); err == nil {
			t.Fatal("failed review reported success")
		}
		registry, err := modelhealth.Open(state)
		if err != nil {
			t.Fatal(err)
		}
		if got := registry.Status(primary, time.Now()).State == modelhealth.Unavailable; got != tc.recorded {
			t.Fatalf("command %q recorded=%v", tc.command, got)
		}
	}
}
