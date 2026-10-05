package executor

import (
	"context"
	"strings"
	"testing"
	"time"

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
			e.Config.Repo = t.TempDir()
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
		e.Config.Repo = t.TempDir()
		e.Sleep = func(context.Context, time.Duration) error { return nil }
		ticket.Estimate = &two
		commands := &Commands{ReviewerCommand: "main-review", ReviewerSmallCommand: "small-review"}
		commands.Run = func(context.Context, runx.Spec) ([]byte, error) { return nil, nil }
		e.Tools = &reviewerTools{fakeTools: baseTools, commands: commands}
		if _, err := e.Execute(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		last := comments.comments[len(comments.comments)-1]
		if !strings.Contains(last, "审查命令档位: small") {
			t.Fatalf("review handoff lost actual tier: %q", last)
		}
	})

}
