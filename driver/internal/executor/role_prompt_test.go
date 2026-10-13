package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func writeRole(t *testing.T, repo, name, text string) {
	t.Helper()
	dir := filepath.Join(repo, "scripts", "agent-gates", "roles")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// withReviewerRole makes a faked `git worktree add` produce a checkout that
// carries reviewer.md, as the real head checkout does.
func withReviewerRole(t *testing.T, run func(context.Context, runx.Spec) ([]byte, error)) func(context.Context, runx.Spec) ([]byte, error) {
	return func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "git" && len(s.Args) >= 4 && s.Args[0] == "worktree" && s.Args[1] == "add" {
			writeRole(t, s.Args[3], "reviewer.md", "REVIEWER ROLE\n")
		}
		return run(ctx, s)
	}
}

// Role rules live in roles/*.md so editing them takes effect without a driver
// release; Go only appends ticket data.
func TestRolePromptWorkerSpecPrefixesRolesBeforeBrief(t *testing.T) {
	repo := t.TempDir()
	if _, err := workerTaskSpec(repo, "BRIEF"); err == nil {
		t.Fatal("missing driver-worker.md must fail instead of dispatching an unbriefed worker")
	}
	writeRole(t, repo, "driver-worker.md", "WORKER ROLE\n")
	spec, err := workerTaskSpec(repo, "BRIEF")
	if err != nil || spec != "WORKER ROLE\n\nBRIEF" {
		t.Fatalf("spec=%q err=%v", spec, err)
	}
	writeRole(t, repo, "local-worker.md", "LOCAL RULES\n")
	spec, err = workerTaskSpec(repo, "BRIEF")
	if err != nil || spec != "WORKER ROLE\n\nLOCAL RULES\n\nBRIEF" {
		t.Fatalf("spec=%q err=%v", spec, err)
	}
	if strings.Contains(briefTemplate, "Constraints:") || strings.Contains(briefTemplate, "Delivery:") {
		t.Fatal("brief template must carry ticket data only; rules belong in driver-worker.md")
	}
}

func TestRolePromptCorrectionStartsWithRole(t *testing.T) {
	repo := t.TempDir()
	var prompt string
	c := &Commands{Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		prompt = s.Args[1]
		return nil, nil
	}}
	req := CorrectionRequest{Repo: repo, Path: "/state/correction.md", Cause: "gate red", Brief: "BRIEF", Number: 1}
	if err := c.Correction(context.Background(), req); err == nil || prompt != "" {
		t.Fatalf("missing correction.md must fail before starting a session: err=%v prompt=%q", err, prompt)
	}
	writeRole(t, repo, "correction.md", "CORRECTION ROLE\n")
	if err := c.Correction(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prompt, "CORRECTION ROLE\n") {
		t.Fatalf("prompt must start with correction.md: %q", prompt)
	}
	for _, want := range []string{`"/state/correction.md"`, "1/3", "gate red", "BRIEF"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q: %q", want, prompt)
		}
	}
}

func TestRolePromptReviewerRequired(t *testing.T) {
	c := &Commands{ReviewerCommand: "review", Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path != "git" {
			t.Fatal("reviewer must not start without reviewer.md")
		}
		return []byte("diff --git a/x b/x\n+added\n"), nil
	}}
	if _, _, err := c.Review(context.Background(), t.TempDir(), "base", "head", nil); err == nil {
		t.Fatal("missing reviewer.md must fail the review")
	}
}
