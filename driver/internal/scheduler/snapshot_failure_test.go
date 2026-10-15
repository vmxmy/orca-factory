package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/logx"
)

type failingSnapshotBackend struct {
	fakeBackend
	err      error
	snapshot func(context.Context, []config.Repository) ([]Ticket, error)
}

func (f *failingSnapshotBackend) Snapshot(ctx context.Context, repos []config.Repository) ([]Ticket, error) {
	if f.snapshot != nil {
		return f.snapshot(ctx, repos)
	}
	return nil, f.err
}

// The recorded Linear 400 body travels through the real client and Runtime.
func TestRecordedSnapshotFailureKeepsCause(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "lin_api_recorded_fixture")
	body := recorded(t, "linear-export-parse-error.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Query string }
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		if !strings.Contains(q.Query, "query Metadata(") {
			http.Error(w, string(body), http.StatusBadRequest)
			return
		}
		nodes := []any{}
		if strings.Contains(q.Query, "projects(") {
			nodes = append(nodes, map[string]any{"id": "p1", "name": "Alpha"})
		}
		connection := map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}
		field := map[bool]string{true: "projects", false: "states"}[strings.Contains(q.Query, "projects(")]
		if strings.Contains(q.Query, "labels(") {
			field = "labels"
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"team": map[string]any{field: connection}}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := linear.New(context.Background(), linear.Options{TeamID: "team", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	repo := config.Repository{Path: "/fixture", Env: map[string]string{"AGENT_LINEAR_PROJECT": "Alpha", "AGENT_SCOPE_AVOID": "0"}}
	runtime := &Runtime{Clients: map[string]LinearClient{repo.Path: client}, Repositories: map[string]config.Repository{repo.Path: repo}}
	f := &failingSnapshotBackend{snapshot: func(ctx context.Context, _ []config.Repository) ([]Ticket, error) {
		return runtime.Snapshot(ctx, []config.Repository{repo})
	}}
	s := setup(&f.fakeBackend)
	s.Backend = f
	var logs bytes.Buffer
	s.Logger = logx.New("scheduler", &logs)

	err = s.Tick(context.Background())

	var classified *infra.Ticket
	if !errors.As(err, &classified) || !strings.HasPrefix(err.Error(), "scheduler snapshot failed: linear snapshot failed") {
		t.Fatalf("err=%v", err)
	}
	warn, _, _ := strings.Cut(logs.String(), "\n")
	if !strings.Contains(warn, `"level":"WARN"`) || !strings.Contains(warn, `"category":"other"`) || !strings.Contains(warn, "linear snapshot failed") {
		t.Fatalf("WARN=%s", warn)
	}
	for _, leaked := range []string{"Syntax Error", "lin_api_recorded_fixture"} {
		if strings.Contains(err.Error(), leaked) || strings.Contains(logs.String(), leaked) {
			t.Fatalf("leaked %q: err=%v log=%s", leaked, err, logs.String())
		}
	}
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
			Err:     errors.New("invalid resolver receipt"),
		},
		want: []string{`"category":"evidence"`, "invalid resolver receipt"},
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
