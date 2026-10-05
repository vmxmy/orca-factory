package runx

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const stderrLimit = 2 << 10

// Match cmd.Output's diagnostic retention independently of the log limit.
const stderrDiagnosticLimit = 32 << 10

// Diagnostic windows match cmd.Output. The separate log prefix has enough
// lookahead for any credential supplied in Args and common credential syntax;
// neither copy grows with the amount of child output.
type stderrCapture struct {
	prefix, suffix, logPrefix []byte
	omitted, consumed         int64
	logOverflow               bool
}

func newStderrCapture(secrets []string) *stderrCapture {
	longest := 0
	for _, secret := range secrets {
		longest = max(longest, len(secret))
	}
	return &stderrCapture{
		prefix:    make([]byte, 0, stderrDiagnosticLimit),
		suffix:    make([]byte, 0, stderrDiagnosticLimit),
		logPrefix: make([]byte, 0, stderrLimit+128+longest),
	}
}

func (b *stderrCapture) Write(p []byte) (int, error) {
	n := len(p)
	b.consumed += int64(n)
	added := min(n, cap(b.logPrefix)-len(b.logPrefix))
	b.logPrefix = append(b.logPrefix, p[:added]...)
	b.logOverflow = b.logOverflow || added < n
	added = min(n, stderrDiagnosticLimit-len(b.prefix))
	b.prefix = append(b.prefix, p[:added]...)
	p = p[added:]
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

func (b *stderrCapture) Bytes() []byte {
	result := append([]byte(nil), b.prefix...)
	if b.omitted > 0 {
		result = fmt.Appendf(result, "\n... omitting %d bytes ...\n", b.omitted)
	}
	return append(result, b.suffix...)
}

func (b *stderrCapture) logText(secrets []string) string {
	s := string(b.logPrefix)
	if b.logOverflow {
		// A raw capture boundary may split a known credential. Hide that
		// overlapping suffix before replacements can bring it into the log.
		cut := len(s)
		for _, secret := range secrets {
			for n := min(len(s), len(secret)-1); n > 0; n-- {
				if strings.HasSuffix(s, secret[:n]) {
					cut = min(cut, len(s)-n)
					break
				}
			}
		}
		if cut < len(s) {
			s = s[:cut] + "***"
		}
	}
	s = hideSecrets(s, secrets)
	if b.logOverflow && len(s) < len(b.logPrefix) {
		// Drop the final incomplete text token after redaction. Substitutions
		// earlier in the prefix may otherwise expose a split credential marker.
		end := strings.LastIndexAny(s, " \t\r\n")
		if end >= 0 {
			s = s[:end+1]
		} else {
			s = ""
		}
	}
	if len(s) > stderrLimit {
		s = s[:stderrLimit]
	}
	return s
}

var credentials = regexp.MustCompile(`(?i)(?:lin_api_|gh[pousr]_|github_pat_|sk-)[a-z0-9_-]+|(?:bearer|basic)\s+\S+|://[^/\s@]+@`)
var jsonCredential = regexp.MustCompile(`(?i)("(?:token|password|secret|api[_-]?key|authorization|credential|capability)"\s*:\s*)("(?:\\.|[^"\\])*"?|[^,}\]\s]+)`)
var authorization = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)((?:bearer|basic)\s+)?([^\s&]+)`)
var credentialValue = regexp.MustCompile(`(?i)((?:token|password|secret|api[_-]?key|credential|capability)[=:]\s*)([^\s&]+)`)

// SafeText removes common credential forms from diagnostic text.
func SafeText(s string) string {
	text, _ := redactText(s)
	return text
}

// Recognize key/value shapes before generic credential patterns, so an
// Authorization header's scheme cannot consume the match ahead of its value.
func redactText(s string) (string, []string) {
	var secrets []string
	s = jsonCredential.ReplaceAllStringFunc(s, func(match string) string {
		parts := jsonCredential.FindStringSubmatch(match)
		value := parts[2]
		var decoded string
		if json.Unmarshal([]byte(value), &decoded) == nil {
			secrets = append(secrets, decoded)
			fields := strings.Fields(decoded)
			if len(fields) == 2 && (strings.EqualFold(fields[0], "Bearer") || strings.EqualFold(fields[0], "Basic")) {
				secrets = append(secrets, fields[1])
			}
		} else {
			secrets = append(secrets, strings.Trim(value, `"`))
		}
		return parts[1] + `"***"`
	})
	s = authorization.ReplaceAllStringFunc(s, func(match string) string {
		parts := authorization.FindStringSubmatch(match)
		secrets = append(secrets, parts[3])
		return parts[1] + "***"
	})
	s = credentialValue.ReplaceAllStringFunc(s, func(match string) string {
		parts := credentialValue.FindStringSubmatch(match)
		secrets = append(secrets, parts[2])
		return parts[1] + "***"
	})
	return credentials.ReplaceAllString(s, "***"), secrets
}

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
		case secretFlag(key) && assigned && !strings.ContainsAny(key, " \t\r\n{}[]\"'"):
			result[i] = key + "=***"
			secrets = append(secrets, value)
		case strings.HasPrefix(arg, "-") && secretFlag(arg):
			result[i] = arg
			hide = true
		default:
			var found []string
			result[i], found = redactText(arg)
			secrets = append(secrets, found...)
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
