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
	if _, _, err := c.Review(context.Background(), repo, "origin/main", "HEAD", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdin, "End stdout with JSON") || !strings.Contains(stdin, "+added") {
		t.Fatalf("stdin must carry prompt and diff: %q", stdin)
	}
	if !strings.HasSuffix(script, `reviewer claude -p --allowedTools "Read Grep" `+repo+"/"+fileBase(stdinPath)) {
		t.Fatalf("reviewer command must be passed unchanged, without a positional prompt: %s", script)
	}
}

func fileBase(path string) string { return path[strings.LastIndex(path, "/")+1:] }
