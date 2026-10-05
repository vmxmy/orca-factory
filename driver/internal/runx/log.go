package runx

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const stderrLimit = 2 << 10

// Match cmd.Output's diagnostic retention independently of the log limit.
const stderrDiagnosticLimit = 32 << 10

// Preserve cmd.Output's prefix/suffix diagnostics independently of the log.
func diagnostic(raw []byte) []byte {
	if len(raw) <= 2*stderrDiagnosticLimit {
		return append([]byte(nil), raw...)
	}
	result := append([]byte(nil), raw[:stderrDiagnosticLimit]...)
	result = fmt.Appendf(result, "\n... omitting %d bytes ...\n", len(raw)-2*stderrDiagnosticLimit)
	return append(result, raw[len(raw)-stderrDiagnosticLimit:]...)
}

var credentials = regexp.MustCompile(`(?i)(?:lin_api_|gh[pousr]_|github_pat_|sk-)[a-z0-9_-]+|(?:bearer|basic)\s+\S+|(?:token|password|secret|api[_-]?key|authorization)[=:]\s*[^\s&]+|://[^/\s@]+@`)

// SafeText removes common credential forms from diagnostic text.
func SafeText(s string) string { return credentials.ReplaceAllString(s, "***") }

func secretFlag(s string) bool {
	s = strings.ToLower(s)
	for _, part := range []string{"token", "password", "secret", "key", "authorization", "credential", "capability"} {
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
			result[i] = "***"
			secrets = append(secrets, arg)
			hide = false
		case secretFlag(key) && assigned:
			result[i] = key + "=***"
			secrets = append(secrets, value)
		case strings.HasPrefix(arg, "-") && secretFlag(arg):
			result[i] = arg
			hide = true
		default:
			result[i] = SafeText(arg)
		}
	}
	// Redact longer values first so overlapping credentials leave no suffix.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	// A credential can also occur inside a script or diagnostic argument.
	for i, s := range result {
		result[i] = hideSecrets(s, secrets)
	}
	return result, secrets
}
func hideSecrets(s string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return SafeText(s)
}
