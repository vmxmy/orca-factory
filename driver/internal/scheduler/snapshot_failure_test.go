package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	decoder := json.NewDecoder(strings.NewReader(`{"SECRET-BODY":1}`))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&struct{}{})
	if decodeErr == nil {
		t.Fatal("decoder accepted an unknown field")
	}
	for _, tc := range []struct {
		name   string
		cause  error
		hidden []string
		want   []string
	}{{
		name:   "unclassified",
		cause:  errors.New("linear request rejected: key lin_api_secret123"),
		hidden: []string{"lin_api_secret123"},
		want:   []string{`"category":"other"`, "linear request rejected"},
	}, {
		name:   "decoded comment",
		cause:  fmt.Errorf("ownership snapshot failed: %w", decodeErr),
		hidden: []string{"SECRET-BODY"},
		want:   []string{`"category":"other"`, "ownership snapshot failed", "unknown field"},
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
			warn, _, _ := strings.Cut(out, "\n")
			if !strings.Contains(warn, `"level":"WARN"`) || !strings.Contains(warn, `"msg":"scheduler snapshot failed"`) {
				t.Fatalf("first record must be the snapshot WARN: %s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(warn, want) {
					t.Fatalf("WARN missing %q: %s", want, warn)
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
