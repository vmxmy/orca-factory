package executor

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type lintFailWriter struct{}

func (lintFailWriter) Write([]byte) (int, error) { return 0, errors.New("secret-warning-sink") }
func TestLintWarningFallback(t *testing.T) {
	var fallback bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&fallback, nil)))
	defer slog.SetDefault(old)
	e := &Executor{Stderr: lintFailWriter{}}
	e.warn("committed operation cleanup failed: %s", "secret-original-error")
	if !strings.Contains(fallback.String(), "warning output failed") || strings.Contains(fallback.String(), "secret-") {
		t.Fatalf("fallback=%q", fallback.String())
	}
}
