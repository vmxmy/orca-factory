package runx

import (
	"fmt"
	"regexp"
	"strings"
)

const stderrLimit = 2 << 10

// Match cmd.Output's diagnostic retention independently of the log limit.
const stderrDiagnosticLimit = 32 << 10

type stderrCapture struct {
	prefix, suffix []byte
	omitted        int64
}

func (b *stderrCapture) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := stderrDiagnosticLimit - len(b.prefix); remaining > 0 {
		added := min(len(p), remaining)
		b.prefix = append(b.prefix, p[:added]...)
		p = p[added:]
	}
	if len(p) >= stderrDiagnosticLimit {
		b.omitted += int64(len(b.suffix) + len(p) - stderrDiagnosticLimit)
		b.suffix = append(b.suffix[:0], p[len(p)-stderrDiagnosticLimit:]...)
	} else {
		if excess := len(b.suffix) + len(p) - stderrDiagnosticLimit; excess > 0 {
			b.omitted += int64(excess)
			copy(b.suffix, b.suffix[excess:])
			b.suffix = b.suffix[:len(b.suffix)-excess]
		}
		b.suffix = append(b.suffix, p...)
	}
	return n, nil
}

func (b *stderrCapture) diagnostic() []byte {
	result := append([]byte(nil), b.prefix...)
	if b.omitted > 0 {
		result = fmt.Appendf(result, "\n... omitting %d bytes ...\n", b.omitted)
	}
	return append(result, b.suffix...)
}

func (b *stderrCapture) logText(secrets []string) string {
	var text string
	if b.omitted == 0 {
		text = string(b.diagnostic())
	} else {
		// Never join across omitted bytes for logging. A credential beginning
		// in the retained prefix may continue into the discarded middle.
		text = string(b.prefix)
		for _, secret := range secrets {
			for n := min(len(text), len(secret)-1); n > 0; n-- {
				if strings.HasSuffix(text, secret[:n]) {
					text = text[:len(text)-n] + "[REDACTED]"
					break
				}
			}
		}
	}
	text = hideSecrets(text, secrets)
	if len(text) > stderrLimit {
		text = text[:stderrLimit]
	}
	return text
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
