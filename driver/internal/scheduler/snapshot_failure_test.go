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
	for _, tc := range []struct {
		name   string
		cause  error
		hidden []string
		want   []string
	}{{
		name:   "unclassified",
		cause:  errors.New(`linear request rejected: {"errors":[{"message":"票正文 SECRET-BODY lin_api_secret123"}]}`),
		hidden: []string{"SECRET-BODY", "lin_api_secret123", "linear request rejected"},
		want:   []string{`"category":"other"`, "unclassified error", "*errors.errorString"},
	}, {
		name: "evidence",
		cause: &SnapshotEvidenceError{
			Tickets: []Ticket{{Identifier: "OF-9", Repo: "/repo"}},
			Err:     errors.New("comment body SECRET-BODY"),
		},
		hidden: []string{"SECRET-BODY", "comment body"},
		want:   []string{`"category":"evidence"`, "OF-9"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &failingSnapshotBackend{err: tc.cause}
			s := setup(&f.fakeBackend)
			s.Backend = f
			var logs bytes.Buffer
			s.Logger = logx.New("scheduler", &logs)

			err := s.Tick(context.Background())

			if !errors.Is(err, tc.cause) || !strings.HasPrefix(err.Error(), "scheduler snapshot failed: ") {
				t.Fatalf("err=%v", err)
			}
			out := logs.String()
			if !strings.Contains(out, `"level":"WARN"`) {
				t.Fatalf("log must be WARN: %s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("log missing %q: %s", want, out)
				}
			}
			for _, secret := range tc.hidden {
				if strings.Contains(err.Error(), secret) || strings.Contains(out, secret) {
					t.Fatalf("leaked %q: err=%v log=%s", secret, err, out)
				}
			}
		})
	}
}
