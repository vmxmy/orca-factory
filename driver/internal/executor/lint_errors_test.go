package executor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"testing"
)

type lintFailWriter struct{}

func (lintFailWriter) Write([]byte) (int, error) { return 0, errors.New("secret-warning-sink") }
func TestExecutorLogWriteFailureDoesNotAffectExecution(t *testing.T) {
	var fallback bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&fallback, nil)))
	defer slog.SetDefault(old)
	e, o, _, _, _, ticket := fixture(t, "new-sha")
	e.Stderr = lintFailWriter{}
	o.releaseErr = errors.New("secret-original-error")
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Phase != machine.PhaseMergeQueue || o.releases != 1 {
		t.Fatalf("result=%+v err=%v releases=%d", result, err, o.releases)
	}
	if fallback.Len() != 0 {
		t.Fatalf("logging bypassed logx: %q", fallback.String())
	}
}
