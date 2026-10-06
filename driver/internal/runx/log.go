package runx

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/logx"
)

type loggerKey struct{}

var discardLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

// WithLogger attaches the logger used by external calls. Nil disables logging.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logx.Redact(logger))
}

// LoggerFrom returns a silent logger when none was supplied.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if logger, ok := LookupLogger(ctx); ok {
		return logger
	}
	return discardLogger
}

// LookupLogger reports whether a caller supplied a logger, allowing components
// with their own fallback sink to share the command's logger when present.
func LookupLogger(ctx context.Context) (*slog.Logger, bool) {
	logger, ok := ctx.Value(loggerKey{}).(*slog.Logger)
	return logger, ok && logger != nil
}

var authorizationText = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:(?:basic|bearer|token)\s+)?[^\s"'<>]+`)
var credentialText = regexp.MustCompile(`(?i)\b(?:ghp_|github_pat_|lin_api_)[^\s"'<>]+|Bearer\s+[^\s"'<>]+`)
var credentialFlag = regexp.MustCompile(`(?i)(--[a-z0-9_-]*(?:token|key|secret|password|authorization)[a-z0-9_-]*)(?:=|\s+)(?:"[^"]*"|'[^']*'|[^\s]+)`)
var urlCredentials = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]+@`)

func sensitiveFlag(flag string) bool {
	if !strings.HasPrefix(flag, "-") {
		return false
	}
	flag = strings.ToLower(flag)
	// Free text may contain credentials or private ticket/terminal content.
	switch flag {
	case "-p", "--print", "--objective", "--spec", "--setup", "--text", "--comment", "--command", "--body", "--title":
		return true
	}
	for _, word := range []string{"token", "key", "secret", "password", "authorization"} {
		if strings.Contains(flag, word) {
			return true
		}
	}
	return false
}

func redactText(text string) string {
	text = authorizationText.ReplaceAllString(text, "${1}[REDACTED]")
	text = credentialFlag.ReplaceAllString(text, "${1}=[REDACTED]")
	text = credentialText.ReplaceAllString(text, "[REDACTED]")
	return urlCredentials.ReplaceAllString(text, "${1}")
}

func redactArgs(args []string) []string {
	redacted := make([]string, len(args))
	hideNext := false
	for i, arg := range args {
		flag, _, assigned := strings.Cut(arg, "=")
		switch {
		case hideNext:
			redacted[i], hideNext = "[REDACTED]", false
		case sensitiveFlag(flag):
			if assigned {
				redacted[i] = flag + "=[REDACTED]"
			} else {
				redacted[i], hideNext = arg, true
			}
		case strings.HasPrefix(arg, "ghp_"), strings.HasPrefix(arg, "github_pat_"), strings.HasPrefix(arg, "lin_api_"), strings.HasPrefix(arg, "Bearer "):
			redacted[i] = "[REDACTED]"
		default:
			redacted[i] = redactText(redactValues(arg, args))
		}
	}
	return redacted
}

// Sensitive values can reappear outside the option that originally supplied them.
func redactValues(text string, args []string) string {
	for i, arg := range args {
		flag, value, assigned := strings.Cut(arg, "=")
		if !sensitiveFlag(flag) {
			continue
		}
		if !assigned && i+1 < len(args) {
			value = args[i+1]
		}
		if value != "" {
			text = strings.ReplaceAll(text, value, "[REDACTED]")
		}
	}
	return text
}

func redactStderr(stderr string, args []string) string {
	// Provider diagnostics can echo fragments after bounded capture truncation.
	for _, arg := range args {
		flag, _, _ := strings.Cut(arg, "=")
		if (flag == "-p" || flag == "--print") && stderr != "" {
			return "[REDACTED]"
		}
	}
	stderr = redactValues(stderr, args)
	stderr = strings.ToValidUTF8(logx.RedactText(redactText(stderr)), "")
	if len(stderr) > 2048 {
		end := 2048
		for !utf8.RuneStart(stderr[end]) {
			end--
		}
		stderr = stderr[:end]
	}
	return stderr
}
