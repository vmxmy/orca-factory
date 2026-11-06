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

// credentialOpener finds a credential key or flag followed by an opening quote.
var credentialOpener = regexp.MustCompile(`(?i)(--[a-z0-9_-]*(?:token|key|secret|password|authorization)[a-z0-9_-]*(?:=|\s+)|(?:"|')?[a-z0-9_-]*(?:authorization|api[_-]?key|token|password|secret)[a-z0-9_-]*(?:"|')?\s*[:=]\s*)(?:(?:basic|bearer|token)\s+)?["']`)

// redactUnclosedCredentials replaces the first credential value whose opening
// quote has no closer, together with everything after it, with [REDACTED]:
// no later text is provably outside the value body. Closed values are left
// for the ordinary credential passes.
func redactUnclosedCredentials(text string) string {
	from := 0
	for {
		loc := credentialOpener.FindStringIndex(text[from:])
		if loc == nil {
			return text
		}
		start := from + loc[0]
		valueStart := from + loc[1]
		quote := text[valueStart-1]
		if closer := closingQuoteIndex(text, valueStart, quote); closer >= 0 {
			from = closer + 1
			continue
		}
		return text[:start] + "[REDACTED]"
	}
}

func closingQuoteIndex(text string, from int, quote byte) int {
	for i := from; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case quote:
			return i
		}
	}
	return -1
}

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
	stderr = runx.StripANSI(stderr)
	stderr = redactUnclosedCredentials(stderr)
	stderr = reviewCredentials.ReplaceAllString(stderr, "${1}[REDACTED]")
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
	if offset > 0 && oddQuoteParity(file, offset) {
		// The window begins inside a quoted value that opened earlier and may
		// never close in-window: nothing here can be published.
		return ""
	}
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
		// redaction boundaries: the fragment may begin mid-credential, a
		// quoted value opened on the window's first line can straddle on, and
		// an unclosed value can run to EOF. Only a window with even quote
		// parity at every line boundary and at EOF is publishable.
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

// oddQuoteParity reports whether the prefix before the capture window holds an
// odd number of unescaped quotes, meaning the window starts inside a value.
func oddQuoteParity(file *os.File, offset int64) bool {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return true
	}
	var dq, sq int
	buf := make([]byte, 32<<10)
	var read int64
	for read < offset {
		want := offset - read
		if want > int64(len(buf)) {
			want = int64(len(buf))
		}
		n, err := file.Read(buf[:want])
		if n == 0 || err != nil {
			return true
		}
		for i := 0; i < n; i++ {
			switch buf[i] {
			case '\\':
				i++
			case '"':
				dq ^= 1
			case '\'':
				sq ^= 1
			}
		}
		read += int64(n)
	}
	return dq != 0 || sq != 0
}

// dropUnbalancedPrefix publishes only when quote parity is even at every
// line boundary and at EOF: a value opened before the window is
// indistinguishable from one opened on the first line, and an unclosed value
// runs past EOF, so any odd state forfeits the whole capture.
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
