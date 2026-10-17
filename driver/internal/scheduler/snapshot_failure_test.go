package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
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

// A recorded GitHub 409 body travels through the real client and Runtime.
func TestRecordedSnapshotFailureKeepsCause(t *testing.T) {
	body := recorded(t, "github-merge-head-modified-409.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	gh, err := github.New(github.Options{BaseURL: server.URL, HTTPClient: server.Client(), Token: func(context.Context) (string, error) { return "TEST-PLACEHOLDER-CRED", nil }})
	if err != nil {
		t.Fatal(err)
	}
	client, err := issues.NewGitHub("o/r", gh)
	if err != nil {
		t.Fatal(err)
	}
	repo := config.Repository{Path: "/fixture", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "o/r", "AGENT_SCOPE_AVOID": "0"}}
	runtime := &Runtime{Clients: map[string]issues.Store{repo.Path: client}, Repositories: map[string]config.Repository{repo.Path: repo}}
	f := &failingSnapshotBackend{snapshot: func(ctx context.Context, _ []config.Repository) ([]Ticket, error) {
		return runtime.Snapshot(ctx, []config.Repository{repo})
	}}
	s := setup(&f.fakeBackend)
	s.Backend = f
	var logs bytes.Buffer
	s.Logger = logx.New("scheduler", &logs)

	err = s.Tick(context.Background())

	var classified *infra.Ticket
	if !errors.As(err, &classified) || !strings.HasPrefix(err.Error(), "scheduler snapshot failed: issue snapshot failed") {
		t.Fatalf("err=%v", err)
	}
	warn, _, _ := strings.Cut(logs.String(), "\n")
	if !strings.Contains(warn, `"level":"WARN"`) || !strings.Contains(warn, `"category":"other"`) || !strings.Contains(warn, "issue snapshot failed") {
		t.Fatalf("WARN=%s", warn)
	}
	for _, leaked := range []string{"Head branch was modified", "TEST-PLACEHOLDER-CRED"} {
		if strings.Contains(err.Error(), leaked) || strings.Contains(logs.String(), leaked) {
			t.Fatalf("leaked %q: err=%v log=%s", leaked, err, logs.String())
		}
	}
}

func TestSnapshotFailureKeepsCause(t *testing.T) {
	imported := func(envelope string) error {
		_, err := issues.DecodeImportedComments(task.Comment{Body: "<!-- orca-import-comments-v1: " + envelope + " -->\n\n"})
		if err == nil {
			t.Fatalf("imported envelope accepted: %s", envelope)
		}
		return err
	}
	for _, tc := range []struct {
		name   string
		cause  error
		hidden []string
		want   []string
	}{{
		name:   "unclassified",
		cause:  errors.New("linear request rejected: key lin_api_secret123"), // ggignore
		hidden: []string{"lin_api_secret123"},                                // ggignore
		want:   []string{`"category":"other"`, "linear request rejected"},
	}, {
		name:   "imported unknown field",
		cause:  fmt.Errorf("ownership snapshot failed: %w", imported(`{"SECRET-BODY":1}`)),
		hidden: []string{"SECRET-BODY"},
		want:   []string{`"category":"other"`, "ownership snapshot failed: invalid JSON"},
	}, {
		name:   "imported number",
		cause:  fmt.Errorf("ownership snapshot failed: %w", imported(`{"comments":[{"body":12345678901234567890e9999}],"sha256":""}`)),
		hidden: []string{"12345678901234567890e9999"},
		want:   []string{`"category":"other"`, "ownership snapshot failed: invalid JSON"},
	}, {
		name:   "imported time",
		cause:  fmt.Errorf("ownership snapshot failed: %w", imported(`{"comments":[{"createdAt":"PRIVATE-BODY"}],"sha256":""}`)),
		hidden: []string{"PRIVATE-BODY"},
		want:   []string{`"category":"other"`, "ownership snapshot failed: invalid JSON"},
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

func TestSnapshotFailureLogsCause(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inner     string
		cause     func(error) error
		wantError bool
	}{
		{name: "ticket", inner: "feishu API code 1254040", cause: func(err error) error { return &infra.Ticket{Err: err} }, wantError: true},
		{name: "transient", inner: "feishu API code 1254290", cause: func(err error) error { return &infra.Transient{Err: err} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := config.Repository{Path: "/fixture", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "o/r"}}
			cause := tc.cause(errors.New(tc.inner))
			store := &notifyStore{readErr: cause}
			store.issues = []task.Issue{{ID: "rec1", Identifier: "OF-7", Project: task.NamedID{ID: "o/r"}}}
			runtime := &Runtime{Clients: map[string]issues.Store{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}
			f := &failingSnapshotBackend{snapshot: func(ctx context.Context, _ []config.Repository) ([]Ticket, error) {
				return runtime.Snapshot(ctx, []config.Repository{repo})
			}}
			s := setup(&f.fakeBackend)
			s.Backend = f
			var logs bytes.Buffer
			s.Logger = logx.New("scheduler", &logs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var reported []error

			err := Serve(ctx, t.TempDir()+"/serve.lock", func(context.Context) (*Scheduler, error) { return s, nil }, func(context.Context) error {
				cancel()
				return nil
			}, func(err error) { reported = append(reported, err) })

			if err != nil {
				t.Fatal(err)
			}
			warn, _, _ := strings.Cut(logs.String(), "\n")
			for _, want := range []string{`"level":"WARN"`, "OF-7", "/fixture", tc.inner} {
				if !strings.Contains(warn, want) {
					t.Fatalf("WARN missing %q: %s", want, warn)
				}
			}
			if got := len(reported) > 0; got != tc.wantError {
				t.Fatalf("reported=%v, want ERROR report %v", reported, tc.wantError)
			}
			if tc.wantError && !errors.Is(reported[0], cause) {
				t.Fatalf("reported cause lost: %v", reported[0])
			}
		})
	}
}
