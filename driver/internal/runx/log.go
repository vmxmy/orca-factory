package runx

import (
	"regexp"
	"strings"
)

const stderrLimit = 2 << 10

type stderrBuffer []byte

func (b *stderrBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := stderrLimit - len(*b); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		*b = append(*b, p...)
	}
	return n, nil
}

var credentials = regexp.MustCompile(`(?i)(?:lin_api_|gh[pousr]_|github_pat_|sk-)[a-z0-9_-]+|(?:bearer|basic)\s+\S+|(?:token|password|secret|api[_-]?key|authorization)[=:]\s*[^\s&]+|://[^/\s@]+@`)

// SafeText removes common credential forms from diagnostic text.
func SafeText(s string) string { return credentials.ReplaceAllString(s, "[REDACTED]") }

func secretFlag(s string) bool {
	s = strings.ToLower(s)
	for _, part := range []string{"token", "password", "secret", "api-key", "api_key", "apikey", "authorization", "credential", "capability"} {
		if strings.Contains(s, part) {
			return true
		}
	}
	return s == "-u" || s == "--user"
}

// SafeArgs returns a separate, redacted argument list for logging only.
func SafeArgs(args []string) []string {
	result, _ := safeArgs(args)
	return result
}
func safeArgs(args []string) ([]string, []string) {
	result := make([]string, len(args))
	var secrets []string
	hide := false
	for i, arg := range args {
		key, value, assigned := strings.Cut(arg, "=")
		switch {
		case hide:
			result[i] = "[REDACTED]"
			secrets = append(secrets, arg)
			hide = false
		case secretFlag(key) && assigned:
			result[i] = key + "=[REDACTED]"
			secrets = append(secrets, value)
		case strings.HasPrefix(arg, "-") && secretFlag(arg):
			result[i] = arg
			hide = true
		default:
			result[i] = SafeText(arg)
		}
	}
	// A credential can also occur inside a script or diagnostic argument.
	for i, s := range result {
		result[i] = hideSecrets(s, secrets)
	}
	return result, secrets
}
func hideSecrets(s string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	return SafeText(s)
}
