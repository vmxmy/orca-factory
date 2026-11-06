package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type reviewFailure struct {
	class, stderr string
	exitCode      int
	cause         error
}

var reviewCredentials = regexp.MustCompile(`(?i)((?:"|')?[a-z0-9_-]*(?:authorization|api[_-]?key|token|password|secret)[a-z0-9_-]*(?:"|')?\s*[:=]\s*)(?:(?:basic|bearer|token)\s+)?("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s,;]+)`)

// A credential value already orphaned from its key (Bearer "…" becomes
// [REDACTED] "…" after the first pass) is redacted by its quoted shape alone.
var quotedCredentialValue = regexp.MustCompile(`(?i)(authorization|token|password|secret|api[_-]?key)[a-z0-9_-]*\s*[:=]\s*\[REDACTED\]\s*("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*')`)

// Command-line credential flags with quoted values also carry escapes;
// the runx pass stops at the first escaped quote, so match the full quoted
// value here before it runs.
var credentialFlagQuoted = regexp.MustCompile(`(?i)(--[a-z0-9_-]*(?:token|key|secret|password|authorization)[a-z0-9_-]*)(?:=|\s+)("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*')`)

func (failure *reviewFailure) Error() string {
	return fmt.Sprintf("reviewer %s exit_code=%d stderr=%s", failure.class, failure.exitCode, strconv.Quote(failure.stderr))
}

func (failure *reviewFailure) Unwrap() error { return failure.cause }

func reviewCommandFailure(err error, stderr string) *reviewFailure {
	failure := &reviewFailure{class: "empty_output", exitCode: 0}
	if err != nil {
		failure.class, failure.exitCode = "start_failure", -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			failure.class, failure.exitCode = "nonzero_exit", exit.ExitCode()
			if stderr == "" {
				stderr = string(exit.Stderr)
			}
		}
		if errors.Is(err, context.Canceled) {
			failure.cause = context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			failure.cause = context.DeadlineExceeded
			// A started reviewer killed by its own 10-minute timeout is a
			// runtime failure, not a start failure.
			failure.class = "timeout"
		}
	}
	stderr = reviewCredentials.ReplaceAllString(runx.StripANSI(stderr), "${1}[REDACTED]")
	stderr = credentialFlagQuoted.ReplaceAllString(stderr, "${1}=[REDACTED]")
	stderr = runx.RedactDiagnostic(stderr)
	stderr = quotedCredentialValue.ReplaceAllString(stderr, "[REDACTED]")
	const limit = 4096
	if len(stderr) > limit {
		stderr = strings.ToValidUTF8(stderr[len(stderr)-limit:], "")
	}
	failure.stderr = strings.TrimSpace(stderr)
	if failure.stderr == "" {
		failure.stderr = "(unavailable)"
	}
	return failure
}

func reviewStderr(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	const capture = 64 << 10
	offset := max(0, info.Size()-capture)
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, capture))
	if err != nil {
		return ""
	}
	text := string(data)
	if offset > 0 {
		// A captured window that starts inside a line cannot prove its own
		// redaction boundaries: the fragment may begin mid-credential, and a
		// quoted value opened before the window can straddle into it. Drop
		// the partial head line, then drop leading lines until quote parity
		// settles, plus the line where it settles: that line may hold the
		// value's closing tail.
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			return ""
		}
		text = dropUnbalancedPrefix(text[idx+1:])
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
		if text == "" {
			return ""
		}
	}
	return text
}

// dropUnbalancedPrefix removes leading lines while a quoted value opened
// before the capture may still be open, plus the line that closes it: that
// line can hold the value's tail without its key.
// dropUnbalancedPrefix publishes only when quote parity is even at every line
// boundary from the window start: a value opened before the window is
// indistinguishable from one opened on the first line, so any odd boundary
// forfeits the whole capture. An all-even window keeps its first line.
func dropUnbalancedPrefix(text string) string {
	var dq, sq int
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '"':
			dq ^= 1
		case '\'':
			sq ^= 1
		case '\n':
			if dq != 0 || sq != 0 {
				return ""
			}
		}
	}
	if dq != 0 || sq != 0 {
		return ""
	}
	return text
}
