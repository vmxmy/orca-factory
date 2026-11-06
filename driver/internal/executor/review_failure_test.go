package executor

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// A fake reviewer command that really exits 7 with stderr, so the evidence must
// carry the true exit code and stderr tail, not just "command failed".
func realExitRun(script string) func(context.Context, runx.Spec) ([]byte, error) {
	return func(ctx context.Context, spec runx.Spec) ([]byte, error) {
		if spec.Path == "git" {
			return []byte("diff"), nil
		}
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		_, err := cmd.Output()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitErr.Stderr = stderr.Bytes()
		}
		return nil, err
	}
}

func TestReviewFailureEvidence(t *testing.T) {
	two := 2.0
	for _, tc := range []struct {
		name string
		run  func(context.Context, runx.Spec) ([]byte, error)
		kind string
		want []string
	}{
		{
			name: "nonzero exit",
			run:  realExitRun("echo 'provider rate limited, retry later' >&2; exit 7"),
			kind: "非零退出",
			want: []string{"reviewer 失败类别: 非零退出", "exit=7", "provider rate limited"},
		},
		{
			name: "start failure",
			run: func(context.Context, runx.Spec) ([]byte, error) {
				return nil, errors.New("fork/exec reviewer: no such file or directory")
			},
			kind: "命令启动失败",
			want: []string{"reviewer 失败类别: 命令启动失败"},
		},
		{
			name: "empty output",
			run: func(context.Context, runx.Spec) ([]byte, error) {
				return nil, nil
			},
			kind: "输出为空",
			want: []string{"reviewer 失败类别: 输出为空"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, comments, _, baseTools, ticket := fixture(t, "new-sha")
			e.Sleep = func(context.Context, time.Duration) error { return nil }
			ticket.Estimate = &two
			commands := &Commands{ReviewerCommand: "main-review", ReviewerSmallCommand: "small-review", Run: tc.run}
			commands.Run = withReviewerRole(t, commands.Run)
			e.Tools = &reviewerTools{fakeTools: baseTools, commands: commands}
			if _, err := e.Execute(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			evidence := 0
			for _, body := range comments.comments {
				if !strings.HasPrefix(body, "reviewer 失败类别: ") {
					continue
				}
				evidence++
				if evidence == 1 {
					for _, want := range tc.want {
						if !strings.Contains(body, want) {
							t.Fatalf("evidence comment %q lacks %q", body, want)
						}
					}
				}
			}
			if evidence != 3 {
				t.Fatalf("evidence comments = %d; want 3 (infra budget)", evidence)
			}
			last := comments.comments[len(comments.comments)-1]
			if !strings.Contains(last, "infra budget exhausted") || !strings.Contains(last, "reviewer 失败类别: "+tc.kind) {
				t.Fatalf("handoff comment lacks failure class: %q", last)
			}
		})
	}
}

func TestReviewFailureClass(t *testing.T) {
	longStderr := strings.Repeat("a", 5000) + "-tail-marker"
	for _, tc := range []struct {
		name       string
		reviewErr  error
		output     string
		kind       string
		contains   []string
		lacks      []string
		detailSize int
	}{
		{
			name:      "start failure",
			reviewErr: errors.New("fork/exec reviewer: no such file or directory"),
			kind:      "命令启动失败",
		},
		{
			name:      "nonzero exit",
			reviewErr: &exec.ExitError{},
			kind:      "非零退出",
			contains:  []string{"exit=-1"},
		},
		{
			name:      "timeout kill",
			reviewErr: context.DeadlineExceeded,
			kind:      "非零退出",
			contains:  []string{"无退出码"},
		},
		{
			name:      "empty output",
			reviewErr: nil,
			kind:      "输出为空",
			contains:  []string{"stdout 无内容"},
		},
		{
			name:      "unparseable output",
			reviewErr: nil,
			output:    "prose without verdict",
			kind:      "输出为空",
			contains:  []string{"无法解析"},
		},
		{
			name:      "redacted stderr",
			reviewErr: &exec.ExitError{Stderr: []byte("Authorization: Bearer secret-token\nboom")},
			kind:      "非零退出",
			contains:  []string{"[REDACTED]"},
			lacks:     []string{"secret-token"},
		},
		{
			name:       "stderr tail bound",
			reviewErr:  &exec.ExitError{Stderr: []byte(longStderr)},
			kind:       "非零退出",
			contains:   []string{"-tail-marker"},
			lacks:      []string{strings.Repeat("a", 4900)},
			detailSize: 4096,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := classifyReviewFailure(tc.reviewErr, tc.output)
			if failure.kind != tc.kind {
				t.Fatalf("kind = %q; want %q", failure.kind, tc.kind)
			}
			for _, want := range tc.contains {
				if !strings.Contains(failure.detail, want) {
					t.Fatalf("detail %q lacks %q", failure.detail, want)
				}
			}
			for _, banned := range tc.lacks {
				if strings.Contains(failure.detail, banned) {
					t.Fatalf("detail %q leaks %q", failure.detail, banned)
				}
			}
			if tc.detailSize > 0 && len(failure.detail) > tc.detailSize+len("stderr 末4KB: ")+len("exit=-1\n") {
				t.Fatalf("detail exceeds 4KB bound: %d bytes", len(failure.detail))
			}
		})
	}
}
