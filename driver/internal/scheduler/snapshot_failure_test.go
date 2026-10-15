package scheduler

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/logx"
)

type failingSnapshotBackend struct {
	fakeBackend
	err error
}

func (f *failingSnapshotBackend) Snapshot(context.Context, []config.Repository) ([]Ticket, error) {
	return nil, f.err
}

func TestSnapshotFailureKeepsCause(t *testing.T) {
	cause := errors.New("linear request rejected: lin_api_secret123 not allowed")
	f := &failingSnapshotBackend{err: cause}
	s := setup(&f.fakeBackend)
	s.Backend = f
	var logs bytes.Buffer
	s.Logger = logx.New("scheduler", &logs)

	err := s.Tick(context.Background())

	if !errors.Is(err, cause) || !strings.HasPrefix(err.Error(), "scheduler snapshot failed: ") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), "linear request rejected") || strings.Contains(err.Error(), "lin_api_secret123") {
		t.Fatalf("returned error must keep the reason and hide credentials: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, `"category":"other"`) || !strings.Contains(out, "linear request rejected") {
		t.Fatalf("log must carry category and reason: %s", out)
	}
	if strings.Contains(out, "lin_api_secret123") {
		t.Fatalf("log leaked credential: %s", out)
	}
}
