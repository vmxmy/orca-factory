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
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, capture))
	if err != nil {
		return ""
	}
	text := string(data)
	if offset > 0 {
		if credentialStraddlesWindow(file, offset, text) {
			// The window begins inside a quoted credential value that opened
			// earlier and never closed before the window: nothing is publishable.
			return ""
		}
		// A captured window that starts inside a line cannot prove its own
		// redaction boundaries: the fragment may begin mid-credential, a
		// quoted value opened on the window's first line can straddle on, and
		// an unclosed value can run to EOF. Only a window with even quote
		// parity at every line boundary and at EOF is publishable.
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			return ""
		}
		headLine, rest := text[:idx+1], text[idx+1:]
		// The dropped head line may itself open a credential value (the key
		// sits at the window start): that value runs into the rest, so the
		// remainder is not provably outside it.
		if credentialOpener.MatchString(runx.StripANSI(headLine)) {
			return ""
		}
		text = dropUnbalancedPrefix(rest)
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
		if text == "" {
			return ""
		}
	}
	return text
}

// credentialStraddlesWindow reports whether a quoted credential value opened
// in the prefix before the capture window never closes before the window, or
// an assignment or key fragment ends at the window edge with the value's
// opening quote still to come: the window then starts inside the value body
// and nothing is publishable. windowHead is the capture's first bytes, used
// to decide whether a prefix-ending identifier fragment completes a key.
// Prose apostrophes do not arm the state machine; only a credential key or
// flag followed by an opening quote does. The scan is streaming with a
// bounded carry so stderr size does not bound memory.
func credentialStraddlesWindow(file *os.File, offset int64, windowHead string) bool {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return true
	}
	const chunk = 32 << 10
	buf := make([]byte, chunk)
	var carry string
	var read int64
	var quote byte
	escaped := false
	for read < offset {
		want := offset - read
		if want > chunk {
			want = chunk
		}
		n, err := file.Read(buf[:want])
		if n == 0 || err != nil {
			return true
		}
		data := buf[:n]
		read += int64(n)
		if quote != 0 {
			i := 0
			for ; i < len(data) && quote != 0; i++ {
				switch {
				case escaped:
					escaped = false
				case data[i] == '\\':
					escaped = true
				case data[i] == quote:
					quote = 0
				}
			}
			if quote != 0 {
				continue
			}
			data = data[i:]
		}
		// Not inside a value: drain every credential opener in the bounded
		// window, leaving carry at the byte after the last closer (or the
		// opener's value streaming into the next chunk).
		window := runx.StripANSI(carry + string(data))
		carry = ""
		for {
			loc := credentialOpener.FindStringIndex(window)
			if loc == nil {
				break
			}
			valueStart := loc[1]
			q := window[valueStart-1]
			closer := closingQuoteIndex(window, valueStart, q)
			if closer < 0 {
				// Value runs past this chunk: stream its remainder.
				quote = q
				rest := window[valueStart:]
				for i := 0; i < len(rest) && quote != 0; i++ {
					switch {
					case escaped:
						escaped = false
					case rest[i] == '\\':
						escaped = true
					case rest[i] == q:
						quote = 0
						rest = rest[i+1:]
					}
				}
				if quote != 0 {
					break
				}
				window = rest
				continue
			}
			window = window[closer+1:]
		}
		if quote != 0 {
			continue
		}
		if keep := len(window); keep > 512 {
			carry = window[keep-512:]
		} else {
			carry = window
		}
	}
	if quote != 0 {
		return true
	}
	// The prefix may end right after a credential assignment, with the value's
	// opening quote at the window start.
	if credentialAssignmentTail.MatchString(carry) {
		return true
	}
	// Or inside an identifier that only completes a credential key together
	// with the window's first line: test the joined text.
	if frag := identifierTail.FindString(carry); frag != "" {
		head := windowHead
		if idx := strings.IndexByte(head, '\n'); idx >= 0 {
			head = head[:idx]
		}
		return credentialAssignmentTail.MatchString(frag+head) || credentialOpener.MatchString(frag+head)
	}
	return false
}

// credentialAssignmentTail matches a credential key or flag whose value has
// not started yet at the scan position.
var credentialAssignmentTail = regexp.MustCompile(`(?i)(--[a-z0-9_-]*(?:token|key|secret|password|authorization)[a-z0-9_-]*(?:=|\s+)|(?:"|')?[a-z0-9_-]*(?:authorization|api[_-]?key|token|password|secret)[a-z0-9_-]*(?:"|')?\s*[:=]\s*)(?:(?:basic|bearer|token)\s+)?$`)

var identifierTail = regexp.MustCompile(`[A-Za-z0-9_-]{1,64}"?$`)

// dropUnbalancedPrefix publishes only when double-quote parity is even at
// every line boundary and at EOF: a value opened before the window is
// indistinguishable from one opened on the first line, and an unclosed value
// runs past EOF, so any odd state forfeits the whole capture. Single quotes
// are prose apostrophes; single-quoted values are handled by the credential
// scanner instead.
func dropUnbalancedPrefix(text string) string {
	var dq int
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '"':
			dq ^= 1
		case '\n':
			if dq != 0 {
				return ""
			}
		}
	}
	if dq != 0 {
		return ""
	}
	return text
}
