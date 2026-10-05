// Package logx provides structured, credential-redacted driver logs.
package logx

import (
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

// New writes JSON records with a component attached to every record.
func New(component string, w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if sensitive(a.Key) {
			return slog.String(a.Key, "[REDACTED]")
		}
		for _, group := range groups {
			if sensitive(group) {
				return slog.String(a.Key, "[REDACTED]")
			}
		}
		a.Value = a.Value.Resolve()
		switch a.Value.Kind() {
		case slog.KindString:
			a.Value = slog.StringValue(redactString(a.Value.String()))
		case slog.KindAny:
			if err, ok := a.Value.Any().(error); ok {
				a.Value = slog.StringValue(redactString(err.Error()))
				break
			}
			// JSON normalization also covers nested maps, slices, and struct fields.
			b, err := json.Marshal(a.Value.Any())
			if err != nil {
				a.Value = slog.StringValue("[REDACTED]")
				break
			}
			var value any
			if json.Unmarshal(b, &value) != nil {
				a.Value = slog.StringValue("[REDACTED]")
				break
			}
			a.Value = slog.AnyValue(redactJSON(value))
		}
		return a
	}})).With("component", component)
}

func sensitive(key string) bool {
	key = strings.ToLower(key)
	for _, part := range []string{"authorization", "api_key", "apikey", "token", "password", "secret"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}

var credential = regexp.MustCompile(`(^|[^\p{L}\p{N}_-])(?:lin_api_|ghp_|gho_|sk-)[\p{L}\p{N}_-]*`)

func redactString(value string) string {
	return credential.ReplaceAllString(value, "${1}[REDACTED]")
}
func redactJSON(value any) any {
	switch v := value.(type) {
	case string:
		return redactString(v)
	case []any:
		for i := range v {
			v[i] = redactJSON(v[i])
		}
	case map[string]any:
		for k, item := range v {
			if sensitive(k) {
				v[k] = "[REDACTED]"
			} else {
				v[k] = redactJSON(item)
			}
		}
	}
	return value
}
