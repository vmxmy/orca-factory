package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

type clockedBackend struct {
	fakeBackend
	now func() time.Time
	at  []time.Time
}

func (b *clockedBackend) Snapshot(ctx context.Context, repos []config.Repository) ([]Ticket, error) {
	b.at = append(b.at, b.now())
	return b.fakeBackend.Snapshot(ctx, repos)
}

// A resolver result landing after an inspection starts brings the next one
// forward, never sooner than wakeMinInterval after the last one ended.
func TestResolverResultWakesNextInspection(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		poll, landed time.Duration
		request      bool
		mode         string
		want         time.Duration
		reason       string
	}{
		{name: "result wakes", poll: 30 * time.Second, landed: 5 * time.Second, request: true, mode: "running", want: wakeMinInterval, reason: "resolver-result"},
		{name: "minimum interval", poll: 10 * time.Second, landed: time.Second, request: true, mode: "running", want: wakeMinInterval, reason: "resolver-result"},
		{name: "no result", poll: 30 * time.Second, mode: "running", want: time.Minute, reason: "interval"},
		{name: "drain", poll: 30 * time.Second, landed: 5 * time.Second, request: true, mode: "draining", want: time.Minute, reason: "interval"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("AGENT_DRIVER_STATE_ROOT", root)
			if err := maintenance.Set(c.mode); err != nil {
				t.Fatal(err)
			}
			now := start
			clock := func() time.Time { return now }
			backend := &clockedBackend{now: clock}
			s := setup(nil)
			s.Backend, s.Now = backend, clock
			var logs bytes.Buffer
			s.Logger = logx.New("scheduler", &logs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waits := 0
			err := Serve(ctx, filepath.Join(root, "serve.lock"), func(context.Context) (*Scheduler, error) { return s, nil }, func(context.Context) error {
				waits++
				if c.request && waits == 1 {
					if err := requestWake(root, wakeRequest{Reason: "resolver-result", Ticket: "ENG-1", At: start.Add(c.landed)}); err != nil {
						t.Fatal(err)
					}
				}
				now = now.Add(c.poll)
				if len(backend.at) == 2 || waits > 20 {
					cancel()
				}
				return nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(backend.at) != 2 || backend.at[1].Sub(start) != c.want {
				t.Fatalf("inspections at %v, want second at +%v", backend.at, c.want)
			}
			if got := summaryWakeReasons(t, logs.String()); len(got) != 2 || got[1] != c.reason {
				t.Fatalf("wake_reason = %v, want second %q", got, c.reason)
			}
		})
	}
}

func summaryWakeReasons(t *testing.T, logs string) []any {
	t.Helper()
	var reasons []any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["event"] == "summary" {
			reasons = append(reasons, row["wake_reason"])
		}
	}
	return reasons
}

// A resolver round asks for a wake only when it ends on a record the
// scheduler must act on now.
func TestResolverRoundRequestsWakeOnlyWithResult(t *testing.T) {
	cases := []struct {
		name   string
		record *resolver.Record
		want   string
	}{
		{name: "result", record: &resolver.Record{Version: 1, Attempt: 1, Phase: "result", Result: "{}"}, want: "resolver-result"},
		{name: "waiting not due", record: &resolver.Record{Version: 1, Attempt: 1, Phase: "waiting", RetryAt: time.Now().Add(time.Hour)}},
		{name: "interrupted start"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("AGENT_DRIVER_STATE_ROOT", root)
			repo := t.TempDir()
			roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
			if err := os.MkdirAll(roles, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve the ticket"), 0600); err != nil {
				t.Fatal(err)
			}
			bodies := []string{"阶段: Agent处置\n归属: 处置Agent"}
			if c.record != nil {
				raw, err := json.Marshal(c.record)
				if err != nil {
					t.Fatal(err)
				}
				bodies = append(bodies, resolver.Prefix+string(raw))
			}
			comments := evidenceComments(bodies...)
			r := &Runtime{StateRoot: root, Clients: map[string]issues.Store{repo: &fakeIssues{issueComments: comments}}, Repositories: map[string]config.Repository{repo: {Path: repo}}, Orca: &resolverStartupProbe{}}
			ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "ENG-1", Repo: repo}, comments)
			err := r.ResolveTicket(context.Background(), ticket, nil, "controller", nil)
			if (c.record == nil) != (err != nil) {
				t.Fatalf("ResolveTicket = %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(root, wakeFile))
			if c.want == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("wake requested without a result: %s %v", raw, err)
				}
				return
			}
			var request wakeRequest
			if err != nil || json.Unmarshal(raw, &request) != nil || request.Reason != c.want || request.Ticket != "ENG-1" || request.At.IsZero() {
				t.Fatalf("wake request = %s %v, want %s for ENG-1", raw, err, c.want)
			}
		})
	}
}
