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

var reviewCredentials = regexp.MustCompile(`(?i)((?:"|')?[a-z0-9_-]*(?:authorization|api[_-]?key|token|password|secret)[a-z0-9_-]*(?:"|')?\s*[:=]\s*)(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s,;]+)`)

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
		}
	}
	stderr = runx.RedactDiagnostic(stderr)
	stderr = reviewCredentials.ReplaceAllString(stderr, "${1}[REDACTED]")
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
		// redaction boundaries: the fragment may begin mid-credential. Drop
		// every partial line, keeping only lines that started in the file.
		// When no newline exists, the whole capture is one truncated line.
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			return ""
		}
		text = text[idx+1:]
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
		if text == "" {
			return ""
		}
	}
	return text
}
