package ledger

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type lintFailWriter struct{ err error }

func (w lintFailWriter) Write([]byte) (int, error) { return 0, w.err }
func TestLintStatsWarningError(t *testing.T) {
	failure := errors.New("warning sink failed")
	f := newFake()
	f.comments["old"] = []Comment{{Body: "ledger invalid"}}
	l := openTest(f)
	defer func() {
		if err := l.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if err := l.Stats(context.Background(), true, io.Discard, lintFailWriter{failure}); !errors.Is(err, failure) {
		t.Fatalf("warning error lost: %v", err)
	}
}
func TestLintFlushWarningFallbackRetainsRows(t *testing.T) {
	var fallback bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&fallback, nil)))
	defer slog.SetDefault(old)
	ticks := make(chan time.Time)
	f := newFake()
	f.fail = true
	l := New(f, Options{Ticks: ticks, Warnings: lintFailWriter{errors.New("secret-warning-sink")}})
	if err := l.Append(`{"ticket":"A"}`); err != nil {
		t.Fatal(err)
	}
	ticks <- time.Now()
	close(ticks)
	<-l.done
	if !strings.Contains(fallback.String(), "warning output failed") || strings.Contains(fallback.String(), "secret-warning-sink") {
		t.Errorf("fallback=%q", fallback.String())
	}
	if err := l.Close(context.Background()); err == nil {
		t.Error("flush failure lost")
	}
	f.fail = false
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 1 {
		t.Fatalf("buffer lost: %v", f.writes)
	}
}
