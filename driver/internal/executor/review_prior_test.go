package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type priorReviewTools struct {
	*recoveryTools
	commands *Commands
}

func (p *priorReviewTools) Review(ctx context.Context, repo, base, head string, estimate *float64) (string, string, error) {
	return p.commands.Review(ctx, repo, base, head, estimate)
}

// reviewer.md narrows a re-review to the previous findings, regressions and new
// lines only when the input carries the previous red; the first round must stay
// byte-identical to the input without it.
func TestReviewIncludesPreviousRedOnReReview(t *testing.T) {
	red, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", "reviewer-output-fenced.txt"))
	if err != nil {
		t.Fatal(err)
	}
	e, _, tools, _, _, ticket := recoveryFixture(t)
	var inputs []string
	commands := &Commands{ReviewerCommand: "review", Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "git" {
			return []byte("diff --git a/x b/x\n+added\n"), nil
		}
		b, readErr := os.ReadFile(s.Args[len(s.Args)-1])
		inputs = append(inputs, string(b))
		if len(inputs) == 1 {
			return red, readErr
		}
		return []byte(`{"verdict":"green","step":"review"}`), readErr
	}}
	commands.Run = withReviewerRole(t, commands.Run)
	e.Tools = &priorReviewTools{recoveryTools: tools, commands: commands}
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || len(inputs) != 2 {
		t.Fatalf("result=%+v err=%v reviews=%d", result, err, len(inputs))
	}
	const heading = "## 上一轮 reviewer 意见"
	if strings.Contains(inputs[0], heading) {
		t.Fatalf("first round must not carry a previous review:\n%s", inputs[0])
	}
	firstRound := inputs[0]
	for _, want := range []string{heading, "简报声明 diff 为空", "driver/internal/usage/usage.go"} {
		if !strings.Contains(inputs[1], want) {
			t.Fatalf("re-review input lacks %q:\n%s", want, inputs[1])
		}
	}
	if without := strings.Replace(inputs[1], priorReviewSection(inputs[1]), "", 1); without != firstRound {
		t.Fatalf("re-review input must equal the first round plus the previous review:\n%s", inputs[1])
	}

	stale := checkpoint{PriorReview: "reason: stale", PriorReviewAttempt: 9}
	if got := priorReview(stale, 1); got != "" {
		t.Fatalf("a red from another attempt must not reach the reviewer: %q", got)
	}
}

func priorReviewSection(input string) string {
	start := strings.Index(input, "## 上一轮 reviewer 意见")
	end := strings.Index(input, "```diff")
	if start < 0 || end < start {
		return ""
	}
	return input[start:end]
}
