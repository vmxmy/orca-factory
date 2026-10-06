// Package logx provides structured, credential-redacted driver logs.
package logx

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

// New writes JSON records with a component attached to every record.
func New(component string, w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: redactAttr})).With("component", component)
}

func redactAttr(groups []string, a slog.Attr) slog.Attr {
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
	case slog.KindGroup:
		attrs := a.Value.Group()
		clean := make([]slog.Attr, len(attrs))
		for i, attr := range attrs {
			clean[i] = redactAttr(groups, attr)
		}
		a.Value = slog.GroupValue(clean...)
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(redactString(err.Error()))
			break
		}
		b, err := json.Marshal(a.Value.Any())
		if err != nil {
			return slog.String(a.Key, "[REDACTED]")
		}
		var value any
		if json.Unmarshal(b, &value) != nil {
			return slog.String(a.Key, "[REDACTED]")
		}
		a.Value = slog.AnyValue(redactJSON(value))
	}
	return a
}

// Redact protects records sent to a caller-provided logger using the same
// credential policy as New. Sink failures remain best effort, as in slog.
func Redact(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return nil
	}
	return slog.New(redactingHandler{Handler: logger.Handler()})
}

type redactingHandler struct {
	slog.Handler
	groups []string
}

func (h redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, redactString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool { clean.AddAttrs(redactAttr(h.groups, a)); return true })
	return h.Handler.Handle(ctx, clean)
}

func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = redactAttr(h.groups, a)
	}
	h.Handler = h.Handler.WithAttrs(clean)
	return h
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	h.Handler = h.Handler.WithGroup(name)
	h.groups = append(append([]string(nil), h.groups...), name)
	return h
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

// RedactText applies the handler credential policy before callers truncate diagnostics.
func RedactText(value string) string {
	return redactString(value)
}

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
