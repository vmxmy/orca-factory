package runx

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"strings"
)

const stderrLimit = 2 << 10

type loggerContextKey struct{}

// WithLogger attaches the optional external-call logger. A nil logger disables
// an inherited logger without changing command or request behavior.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

// LoggerFrom returns nil when the context has no external-call logger.
func LoggerFrom(ctx context.Context) *slog.Logger {
	logger, _ := ctx.Value(loggerContextKey{}).(*slog.Logger)
	return logger
}

var credential = regexp.MustCompile(`(?i)(?:ghp_|gho_|github_pat_|lin_api_)[a-z0-9_-]*|Bearer\s+[^\s"',}\]]+`)
var userinfo = regexp.MustCompile(`://[^/\s@]+@`)
var credentialField = regexp.MustCompile(`(?i)(\b(?:token|key|password|secret|authorization)\b["']?\s*(?:[=:]|\s)\s*)("(?:\\.|[^"\\])*"|'[^']*'|(?:Bearer\s+)?[^\s,}\]]+)`)

func sensitiveFlag(flag string) bool {
	switch strings.ToLower(flag) {
	case "--token", "--key", "--password", "--secret", "--authorization",
		"--objective", "--comment", "--body", "--text", "--spec", "--command":
		// Orca may send private prose as command arguments. Hide it from runx too.
		return true
	}
	return false
}

func sensitiveValues(args []string) []string {
	var values []string
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if !sensitiveFlag(flag) {
			continue
		}
		if !assigned && i+1 < len(args) {
			i++
			value = args[i]
		}
		if value != "" {
			values = append(values, value)
		}
	}
	// Replace longer overlapping values before shorter ones.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

func redactArgs(args []string) []string {
	result := make([]string, len(args))
	secrets := sensitiveValues(args)
	for i := 0; i < len(args); i++ {
		flag, _, assigned := strings.Cut(args[i], "=")
		switch {
		case sensitiveFlag(flag) && assigned:
			result[i] = flag + "=[REDACTED]"
		case sensitiveFlag(flag):
			result[i] = flag
			if i+1 < len(args) {
				i++
				result[i] = "[REDACTED]"
			}
		default:
			result[i] = redactText(args[i], secrets)
		}
	}
	return result
}

// SafeText redacts credential syntax in HTTP paths and diagnostic identifiers.
func SafeText(text string) string { return redactText(text, nil) }

func redactText(text string, secrets []string) string {
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
		// Output retains only a diagnostic head/tail for very large stderr. A
		// credential longer than the log can be split by that diagnostic boundary.
		if len(secret) > stderrLimit {
			text = strings.ReplaceAll(text, secret[:stderrLimit], "[REDACTED]")
		}
	}
	text = credentialField.ReplaceAllString(text, "${1}[REDACTED]")
	text = credential.ReplaceAllString(text, "[REDACTED]")
	return userinfo.ReplaceAllString(text, "://")
}
