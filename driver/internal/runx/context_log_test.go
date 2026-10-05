package runx

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

type logRecorder struct {
	level  string
	fields map[string]any
}

func (l *logRecorder) Debug(_ string, args ...any) { l.save("debug", args) }
func (l *logRecorder) Info(_ string, args ...any)  { l.save("info", args) }
func (l *logRecorder) Warn(_ string, args ...any)  { l.save("warn", args) }
func (l *logRecorder) Error(_ string, args ...any) { l.save("error", args) }
func (l *logRecorder) save(level string, args []any) {
	l.level, l.fields = level, map[string]any{}
	for i := 0; i+1 < len(args); i += 2 {
		l.fields[args[i].(string)] = args[i+1]
	}
}

func TestExternalCallLogContext(t *testing.T) {
	t.Run("failure and flag values", func(t *testing.T) {
		log := new(logRecorder)
		args := []string{"-c", `printf stdout; printf 'diagnostic message' >&2; exit 3`, "sh", "--token", "--private-value", "--objective=private-text", "--", "--private-positional"}
		out, err := Run(WithLogger(context.Background(), log), Spec{Path: "/bin/sh", Args: args, Timeout: time.Second})
		var exit *exec.ExitError
		if string(out) != "stdout" || !errors.As(err, &exit) || exit.ExitCode() != 3 || string(exit.Stderr) != "diagnostic message" {
			t.Fatalf("command contract: output=%q error=%v", out, err)
		}
		if log.fields["exit_code"] != 3 || log.fields["stderr"] != "diagnostic message" || log.fields["timeout"] != false || log.fields["executable"] != "sh" {
			t.Fatalf("failure fields: %v", log.fields)
		}
		if !reflect.DeepEqual(log.fields["args"], []string{"-c", "--token", "--objective"}) || log.fields["arg_count"] != len(args) {
			t.Fatalf("argument values logged: %v", log.fields)
		}
		if _, ok := log.fields["elapsed_ms"].(int64); !ok {
			t.Fatalf("missing elapsed milliseconds: %v", log.fields)
		}
	})
	t.Run("timeout warns", func(t *testing.T) {
		log := new(logRecorder)
		_, err := Run(WithLogger(context.Background(), log), Spec{Path: "/bin/sleep", Args: []string{"10"}, Timeout: 30 * time.Millisecond})
		if !errors.Is(err, context.DeadlineExceeded) || log.level != "warn" || log.fields["timeout"] != true {
			t.Fatalf("timeout error=%v level=%s fields=%v", err, log.level, log.fields)
		}
	})
	t.Run("stderr is bounded and redacted", func(t *testing.T) {
		log := new(logRecorder)
		_, err := Run(WithLogger(context.Background(), log), Spec{Path: "/bin/sh", Args: []string{"-c", `printf '%s\n' "$2" >&2; yes diagnostic | head -c 5000 >&2; exit 3`, "sh", "--token", "private-value"}, Timeout: time.Second})
		stderr, ok := log.fields["stderr"].(string)
		if err == nil || !ok || len(stderr) != 2048 || strings.Contains(stderr, "private-value") {
			t.Fatalf("stderr limit/redaction: length=%d error=%v", len(stderr), err)
		}
	})
	t.Run("nil logger", func(t *testing.T) {
		ctx := WithLogger(WithLogger(context.Background(), new(logRecorder)), nil)
		if LoggerFromContext(ctx) != nil {
			t.Fatal("nil did not disable inherited logger")
		}
	})
}
