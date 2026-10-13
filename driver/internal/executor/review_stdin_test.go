package executor

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// claude's --allowedTools is variadic and swallows a trailing prompt argument, so
// the reviewer must receive its instructions and the diff together on stdin.
func TestReviewSendsPromptAndDiffOnStdin(t *testing.T) {
	repo := t.TempDir()
	var stdin, script, stdinPath string
	c := &Commands{ReviewerCommand: `claude -p --allowedTools "Read Grep"`, Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "git" {
			return []byte("diff --git a/x b/x\n+added\n"), nil
		}
		script = strings.Join(s.Args, " ")
		stdinPath = s.Args[len(s.Args)-1]
		b, err := os.ReadFile(stdinPath)
		stdin = string(b)
		return []byte(`{"verdict":"green","step":"review"}`), err
	}}
	c.Run = withReviewerRole(t, c.Run)
	if _, _, err := c.Review(context.Background(), repo, "origin/main", "HEAD", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdin, "REVIEWER ROLE") || !strings.Contains(stdin, "+added") {
		t.Fatalf("stdin must carry prompt and diff: %q", stdin)
	}
	if !strings.HasSuffix(script, `reviewer claude -p --allowedTools "Read Grep" `+stdinPath) {
		t.Fatalf("reviewer command must be passed unchanged, without a positional prompt: %s", script)
	}
}

func TestMergeBaseAgainstFreshMain(t *testing.T) {
	var calls []string
	c := &Commands{Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		calls = append(calls, strings.Join(s.Args, " "))
		if s.Args[0] == "merge-base" {
			return []byte("rebased-onto\n"), nil
		}
		return nil, nil
	}}
	base, err := c.MergeBase(context.Background(), t.TempDir(), "rebased-head")
	if err != nil || base != "rebased-onto" {
		t.Fatalf("base=%q err=%v", base, err)
	}
	want := []string{"fetch --no-tags origin main", "merge-base FETCH_HEAD rebased-head"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls=%q", calls)
	}
}

// The reviewer judges the diff against the ticket brief. Without it, work that is
// out of the ticket's SCOPE (wiring, central types) is flagged as a defect.
func TestReviewStdinCarriesBriefAndLane3Globs(t *testing.T) {
	repo := t.TempDir()
	var stdin string
	c := &Commands{ReviewerCommand: "review", Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "git" {
			return []byte("diff --git a/x b/x\n+added\n"), nil
		}
		b, err := os.ReadFile(s.Args[len(s.Args)-1])
		stdin = string(b)
		return []byte(`{"verdict":"green","step":"review"}`), err
	}}
	ctx := WithReviewBrief(context.Background(), ReviewBrief{Text: "# ZII-9 / executor\nSCOPE: mods/a.tsx\nDo not edit register.tsx", Lane3Globs: ".env* *secrets*"})
	c.Run = withReviewerRole(t, c.Run)
	if _, _, err := c.Review(ctx, repo, "origin/main", "HEAD", nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SCOPE: mods/a.tsx", "Do not edit register.tsx", "AGENT_LANE3_GLOBS=.env* *secrets*", "+added"} {
		if !strings.Contains(stdin, want) {
			t.Fatalf("reviewer stdin lacks %q:\n%s", want, stdin)
		}
	}
	// The lane3 line must come immediately before the brief, as reviewer.md specifies.
	if i, j := strings.Index(stdin, "AGENT_LANE3_GLOBS="), strings.Index(stdin, "# ZII-9"); i < 0 || j < i {
		t.Fatalf("lane3 line must precede the brief (i=%d j=%d)", i, j)
	}
	// Without a brief nothing extra is added and the review still runs.
	stdin = ""
	if _, _, err := c.Review(context.Background(), repo, "origin/main", "HEAD", nil); err != nil || strings.Contains(stdin, "AGENT_LANE3_GLOBS") {
		t.Fatalf("no brief: err=%v stdin has lane3=%v", err, strings.Contains(stdin, "AGENT_LANE3_GLOBS"))
	}
}

// reviewer.md reviews "the current worktree", and the reviewer reads files to
// check the diff. Running it in the repo checkout shows origin/main, so a done
// change reads as missing; it must run in a detached checkout of head.
func TestReviewRunsInHeadCheckout(t *testing.T) {
	repo := t.TempDir()
	var added, removed, reviewDir string
	c := &Commands{ReviewerCommand: "review", Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		switch {
		case s.Path == "git" && len(s.Args) == 5 && s.Args[1] == "add":
			if s.Args[2] != "--detach" || s.Args[4] != "new-sha" {
				t.Fatalf("checkout must detach at head: %q", s.Args)
			}
			added = s.Args[3]
		case s.Path == "git" && len(s.Args) == 4 && s.Args[1] == "remove":
			removed = s.Args[3]
		case s.Path == "git":
			return []byte("diff --git a/x b/x\n+added\n"), nil
		default:
			reviewDir = s.Dir
		}
		return nil, nil
	}}
	c.Run = withReviewerRole(t, c.Run)
	if _, _, err := c.Review(context.Background(), repo, "base-sha", "new-sha", nil); err != nil {
		t.Fatal(err)
	}
	if added == "" || reviewDir != added || removed != added {
		t.Fatalf("checkout=%q reviewer dir=%q removed=%q", added, reviewDir, removed)
	}
}
