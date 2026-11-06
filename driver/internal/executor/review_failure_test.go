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
	escaped := reviewCommandFailure(nil, `{"password":"prefix\"sensitive-tail","ok":true}`)
	if strings.Contains(escaped.stderr, "sensitive-tail") {
		t.Errorf("quoted value with escaped quote leaked: %q", escaped.stderr)
	}
	if !strings.Contains(escaped.stderr, `[REDACTED]`) {
		t.Errorf("redaction marker missing: %q", escaped.stderr)
	}
	bearer := reviewCommandFailure(nil, `Authorization: Bearer "synthetic-sensitive-value"`)
	if strings.Contains(bearer.stderr, "synthetic-sensitive-value") {
		t.Errorf("quoted bearer value leaked after layered redaction: %q", bearer.stderr)
	}
	jsonBearer := reviewCommandFailure(nil, `{"Authorization":"Bearer \"synthetic-nested-value\""}`)
	if strings.Contains(jsonBearer.stderr, "synthetic-nested-value") {
		t.Errorf("json-embedded escaped bearer leaked: %q", jsonBearer.stderr)
	}
	ansiKey := reviewCommandFailure(nil, "\x1b[31mAPI_KEY\x1b[0m=synthetic-ansi-value")
	if strings.Contains(ansiKey.stderr, "synthetic-ansi-value") {
		t.Errorf("ansi-wrapped credential key leaked: %q", ansiKey.stderr)
	}
	flagQuoted := reviewCommandFailure(nil, `error invoking reviewer: --password "prefix\"synthetic-flag-tail"`)
	if strings.Contains(flagQuoted.stderr, "synthetic-flag-tail") {
		t.Errorf("escaped-quoted flag value leaked: %q", flagQuoted.stderr)
	}
	timeout := reviewCommandFailure(context.DeadlineExceeded, "reviewer timed out mid-run")
	if timeout.class != "timeout" || !strings.Contains(timeout.Error(), "reviewer timed out mid-run") {
		t.Errorf("runtime timeout misclassified: %+v", timeout)
	}
	empty := recordedReviewFailure(t, "empty")
	if !errors.As(empty, &classified) || classified.class != "empty_output" || classified.exitCode != 0 {
		t.Fatalf("recorded empty sample misclassified: %v", empty)
	}
	if !strings.Contains(empty.Error(), "session ended without final message") {
		t.Errorf("recorded empty evidence lost diagnostics: %v", empty)
	}
}

func TestReviewStderrDropsTruncatedLeadingLine(t *testing.T) {
	dir := t.TempDir()
	oversized := filepath.Join(dir, "single.stderr")
	if err := os.WriteFile(oversized, []byte("API_KEY="+strings.Repeat("k", 70<<10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tail := reviewStderr(oversized); tail != "" {
		t.Fatalf("a capture that starts mid-credential must be dropped, got %q", tail[:min(40, len(tail))])
	}
	mixed := filepath.Join(dir, "mixed.stderr")
	long := "Authorization: Bearer " + strings.Repeat("c", 70<<10)
	body := long + "\nprovider unavailable\nretry budget exhausted\n"
	if err := os.WriteFile(mixed, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := reviewStderr(mixed)
	for _, want := range []string{"provider unavailable", "retry budget exhausted"} {
		if !strings.Contains(tail, want) {
			t.Errorf("complete tail lines were lost with the truncated head: %q", tail)
		}
	}
	if strings.Contains(tail, "cccc") {
		t.Errorf("truncated credential fragment leaked into diagnostics: %q", tail[:40])
	}
	allContinuation := filepath.Join(dir, "continuation.stderr")
	raw := make([]byte, 70<<10)
	for i := range raw {
		raw[i] = 0x80
	}
	if err := os.WriteFile(allContinuation, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if tail := reviewStderr(allContinuation); tail != "" {
		t.Errorf("capture of only continuation bytes must be dropped, got %d bytes", len(tail))
	}
	// A dropped leading line can leave a sole continuation byte: alignment
	// must trim to empty instead of indexing past the slice.
	danglingContinuation := filepath.Join(dir, "dangling.stderr")
	body = strings.Repeat("a", 70000) + "\n\x80"
	if err := os.WriteFile(danglingContinuation, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if tail := reviewStderr(danglingContinuation); tail != "" {
		t.Errorf("sole continuation byte must be dropped, got %q", tail)
	}
	// An oversized final line may itself be truncated at EOF mid-credential;
	// reviewCommandFailure only keeps a bounded tail, so the published window
	// holds the start of that last line, which can cut a credential open.
	noTrailingNewline := filepath.Join(dir, "open.stderr")
	if err := os.WriteFile(noTrailingNewline, []byte(strings.Repeat("y", 70<<10)+"\nlast diagnostic line without newline"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalTail := reviewStderr(noTrailingNewline)
	if !strings.Contains(finalTail, "last diagnostic line without newline") {
		t.Errorf("complete trailing line was lost: %q", tail)
	}
	// A quoted credential opened before the capture window straddles into it:
	// the closing line holds the value's tail without its key, and parity
	// cannot prove where the value ends, so the capture is dropped wholesale.
	straddled := filepath.Join(dir, "straddle.stderr")
	straddleBody := `password="` + strings.Repeat("p", 70<<10) + "\nsynthetic-multiline-tail\"\nprovider unavailable\n"
	if err := os.WriteFile(straddled, []byte(straddleBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if straddleTail := reviewStderr(straddled); straddleTail != "" {
		t.Errorf("straddling multiline credential must drop the whole capture, got %q", straddleTail)
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
