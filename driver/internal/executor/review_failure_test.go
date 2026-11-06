package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func failureReviewer(t *testing.T, command string, startFailure bool) *Commands {
	t.Helper()
	commands := &Commands{ReviewerCommand: command}
	commands.Run = withReviewerRole(t, func(ctx context.Context, spec runx.Spec) ([]byte, error) {
		if spec.Path == "git" {
			return []byte("diff"), nil
		}
		if startFailure {
			spec.Path = filepath.Join(t.TempDir(), "missing-reviewer")
		}
		return runx.Run(ctx, spec)
	})
	return commands
}

func TestReviewFailureEvidence(t *testing.T) {
	for _, test := range []struct {
		name, command, class, code, diagnostic string
		startFailure                           bool
	}{
		{"start", "unused", "start_failure", "exit_code=-1", "(unavailable)", true},
		{"missing executable", "exec nonexistent-reviewer-of419", "nonzero_exit", "exit_code=127", "nonexistent-reviewer-of419", false},
		{"exit", "printf 'private stdout ghp_stdoutsecret'; printf 'provider unavailable\\nAuthorization: Bearer hidden-token\\nAPI_KEY=hidden-key\\n' >&2; exit 17", "nonzero_exit", "exit_code=17", "provider unavailable", false},
		{"empty", "printf ' \\n'; printf 'provider returned no verdict\\n' >&2", "empty_output", "exit_code=0", "provider returned no verdict", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands := failureReviewer(t, test.command, test.startFailure)
			output, _, err := commands.Review(context.Background(), t.TempDir(), "base", "head", nil)
			if err == nil {
				t.Fatal("reviewer failure was accepted")
			}
			evidence := output + err.Error()
			for _, want := range []string{test.class, test.code, test.diagnostic} {
				if !strings.Contains(evidence, want) {
					t.Errorf("failure evidence %q lacks %q", evidence, want)
				}
			}
			for _, secret := range []string{"ghp_stdoutsecret", "hidden-token", "hidden-key", "private stdout"} {
				if strings.Contains(evidence, secret) {
					t.Errorf("failure evidence exposed %q", secret)
				}
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				t.Fatal("review error exposes the original subprocess stderr")
			}
			e, _, comments, _, tools, ticket := fixture(t, "new-sha")
			e.Sleep = func(context.Context, time.Duration) error { return nil }
			e.Tools = &reviewerTools{fakeTools: tools, commands: commands}
			result, err := e.Execute(context.Background(), ticket)
			if err != nil || result.State.Owner != machine.OwnerResolver || result.State.Infra != 3 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if !strings.Contains(result.State.HumanReason, test.class) {
				t.Errorf("handoff reason %q misdescribes %s", result.State.HumanReason, test.class)
			}
			last := comments.comments[len(comments.comments)-1]
			if len(strings.Split(last, "\n")) != 5 {
				t.Fatalf("handoff changed its five-line contract: %q", last)
			}
			for _, want := range []string{test.class, test.code, test.diagnostic, "shared infra budget=3/3", test.class + "=3"} {
				if !strings.Contains(last, want) {
					t.Errorf("handoff %q lacks %q", last, want)
				}
			}
			for _, body := range comments.comments {
				if strings.Contains(body, "hidden-token") || strings.Contains(body, "hidden-key") || strings.Contains(body, "ghp_stdoutsecret") {
					t.Fatal("ticket comment exposed subprocess credentials")
				}
			}
		})
	}
	t.Run("bounded tail", func(t *testing.T) {
		commands := failureReviewer(t, "printf 'discarded-prefix\\n' >&2; head -c 6000 /dev/zero | tr '\\000' x >&2; printf '\\nlast diagnostic\\n' >&2; exit 23", false)
		_, _, err := commands.Review(context.Background(), t.TempDir(), "base", "head", nil)
		if err == nil || !strings.Contains(err.Error(), "last diagnostic") || strings.Contains(err.Error(), "discarded-prefix") || len(err.Error()) > 4300 {
			t.Fatalf("stderr tail was lost or unbounded: %v", err)
		}
	})
}

// recordedReviewFailure replays a recorded reviewer failure through the real
// Review wrapper: the sample's stderr is delivered on fd 2 via the same
// `2> "$2.stderr"` redirection the wrapper installs in production.
func recordedReviewFailure(t *testing.T, sample string) error {
	t.Helper()
	base := "testdata/reviewer-fail-stderr-" + sample
	abs, err := filepath.Abs(base + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	exit, absErr := filepath.Abs(base + ".exit")
	if absErr != nil {
		t.Fatal(absErr)
	}
	commands := &Commands{ReviewerCommand: `sh -c 'cat "$1" >&2; exit "$2"' reviewer ` + strconv.Quote(abs) + " " + strconv.Quote(readTrimmed(t, exit))}
	commands.Run = withReviewerRole(t, func(ctx context.Context, spec runx.Spec) ([]byte, error) {
		if spec.Path == "git" {
			return []byte("diff"), nil
		}
		return runx.Run(ctx, spec)
	})
	_, _, reviewErr := commands.Review(context.Background(), t.TempDir(), "base", "head", nil)
	return reviewErr
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestReviewFailureEvidenceRecorded(t *testing.T) {
	code := readTrimmed(t, "testdata/reviewer-fail-stderr-nonzero.exit")
	failure := recordedReviewFailure(t, "nonzero")
	var classified *reviewFailure
	if !errors.As(failure, &classified) {
		t.Fatalf("recorded failure must stay classified, got %T", failure)
	}
	if classified.class != "nonzero_exit" || strconv.Itoa(classified.exitCode) != code {
		t.Fatalf("recorded sample classified %s exit=%d, want nonzero_exit exit=%s", classified.class, classified.exitCode, code)
	}
	for _, want := range []string{"stream error: provider upstream unavailable", "[REDACTED]"} {
		if !strings.Contains(failure.Error(), want) {
			t.Errorf("recorded evidence %q lacks %q", failure.Error(), want)
		}
	}
	for _, secret := range []string{"ghp_recordedsample0000", "recorded-sample-key"} {
		if strings.Contains(failure.Error(), secret) {
			t.Errorf("recorded evidence exposed %q", secret)
		}
	}
	empty := recordedReviewFailure(t, "empty")
	if !errors.As(empty, &classified) || classified.class != "empty_output" || classified.exitCode != 0 {
		t.Fatalf("recorded empty sample misclassified: %v", empty)
	}
	if !strings.Contains(empty.Error(), "session ended without final message") {
		t.Errorf("recorded empty evidence lost diagnostics: %v", empty)
	}
}

func TestReviewStderrKeepsSingleLongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long.stderr")
	line := strings.Repeat("x", 70<<10)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := reviewStderr(path)
	if len(tail) == 0 || len(tail) > 64<<10 {
		t.Fatalf("oversized single line was dropped instead of bounded: %d bytes", len(tail))
	}
	if strings.Count(tail, "x") != len(strings.TrimSpace(tail)) {
		t.Fatalf("tail must keep the diagnostic content, got %q", tail[:40])
	}
}

func TestReviewFailureClass(t *testing.T) {
	e, _, comments, _, tools, ticket := fixture(t, "new-sha")
	e.Sleep = func(context.Context, time.Duration) error { return nil }
	commands := failureReviewer(t, "printf 'missing verdict\\n' >&2", false)
	run, calls := commands.Run, 0
	commands.Run = func(ctx context.Context, spec runx.Spec) ([]byte, error) {
		if spec.Path != "git" {
			calls++
			switch calls {
			case 1:
				spec.Path = filepath.Join(t.TempDir(), "missing-reviewer")
			case 2:
				spec.Args[3] = "printf 'provider unavailable\\n' >&2; exit 19"
			}
		}
		return run(ctx, spec)
	}
	e.Tools = &reviewerTools{fakeTools: tools, commands: commands}
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver || calls != 3 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
	}
	all := strings.Join(comments.comments, "\n")
	for _, want := range []string{"start_failure=1", "nonzero_exit=1", "empty_output=1", "exit_code=19", "provider unavailable", "shared infra budget=3/3"} {
		if !strings.Contains(all, want) {
			t.Errorf("comments %q lack classification %q", all, want)
		}
	}
}
