package runx

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"
)

type loggerKey struct{}

var discardLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

// WithLogger attaches the logger used by external calls. Nil disables logging.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// LoggerFrom returns a silent logger when none was supplied.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return discardLogger
}

var credentialText = regexp.MustCompile(`(?i)\b(?:ghp_|github_pat_|lin_api_)[^\s"'<>]+|Bearer\s+[^\s"'<>]+`)
var credentialFlag = regexp.MustCompile(`(?i)(--[a-z0-9_-]*(?:token|key|secret|password|authorization)[a-z0-9_-]*)(?:=|\s+)(?:"[^"]*"|'[^']*'|[^\s]+)`)
var urlCredentials = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]+@`)

func sensitiveFlag(flag string) bool {
	if !strings.HasPrefix(flag, "-") {
		return false
	}
	flag = strings.ToLower(flag)
	for _, word := range []string{"token", "key", "secret", "password", "authorization"} {
		if strings.Contains(flag, word) {
			return true
		}
	}
	return false
}

func redactText(text string) string {
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
			redacted[i] = redactText(arg)
		}
	}
	return redacted
}

func redactStderr(stderr string, args []string) string {
	for i, arg := range args {
		flag, value, assigned := strings.Cut(arg, "=")
		if !sensitiveFlag(flag) {
			continue
		}
		if !assigned && i+1 < len(args) {
			value = args[i+1]
		}
		if value != "" {
			stderr = strings.ReplaceAll(stderr, value, "[REDACTED]")
		}
	}
	stderr = strings.ToValidUTF8(redactText(stderr), "")
	if len(stderr) > 2048 {
		end := 2048
		for !utf8.RuneStart(stderr[end]) {
			end--
		}
		stderr = stderr[:end]
	}
	return stderr
}
