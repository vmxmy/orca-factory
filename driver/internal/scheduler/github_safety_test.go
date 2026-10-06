package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

const safetyID = "01234567-1234-1234-1234-012345678901"

// The issue API is a system boundary; scheduler, merge channel and Git are real.
type safetyIssues struct {
	mu          sync.Mutex
	row         map[string]any
	comments    []map[string]any
	afterIntent func()
}

func (g *safetyIssues) comment(body string) {
	g.row["updated_at"] = time.Now().Add(time.Duration(len(g.comments)) * time.Second)
	g.comments = append(g.comments, map[string]any{"id": len(g.comments) + 1, "body": body, "created_at": time.Now().Add(time.Duration(len(g.comments)) * time.Second), "updated_at": time.Now()})
}
func (g *safetyIssues) stop(human bool) {
	if human {
		g.comment("转人: stop\n归属: 人")
	} else {
		g.row["state"] = "closed"
		g.row["state_reason"] = "not_planned"
	}
}
func githubSafetyFixture(t *testing.T, f *mergeFixture) *safetyIssues {
	t.Helper()
	rewriteHandoff(t, f, func(h map[string]any) { h["ticket_id"] = safetyID })
	m := issues.Metadata{ID: safetyID, Identifier: "ENG-1", Project: "o/r", State: "In Progress", Complete: true, Blockers: []string{}, Related: []string{}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	g := &safetyIssues{row: map[string]any{"number": 42, "title": "execution", "body": "<!-- orca-task-v1: " + string(raw) + " -->", "state": "open", "labels": []any{}, "updated_at": time.Now()}}
	for _, c := range f.linear.rows {
		g.comment(c.Body)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		var out any
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments"):
			if r.Method == "POST" {
				var input map[string]string
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					return
				}
				g.comment(input["body"])
				out = g.comments[len(g.comments)-1]
				if strings.Contains(input["body"], `"phase":"merge-intent"`) && g.afterIntent != nil {
					g.afterIntent()
				}
			} else {
				out = g.comments
			}
		case r.Method == "PATCH":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			for k, v := range input {
				if k == "labels" {
					labels := []any{}
					for _, n := range v.([]any) {
						labels = append(labels, map[string]any{"name": n})
					}
					g.row[k] = labels
				} else {
					g.row[k] = v
				}
			}
			out = g.row
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			out = []any{g.row}
		default:
			t.Errorf("unexpected issue request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	var clockMu sync.Mutex
	tick := int64(0)
	gh, err := github.New(github.Options{BaseURL: server.URL, HTTPClient: &http.Client{Transport: &http.Transport{}}, Token: func(context.Context) (string, error) { return "test", nil }, Now: func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); tick++; return time.Unix(tick, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	client, err := issues.NewGitHub("o/r", gh)
	if err != nil {
		t.Fatal(err)
	}
	repo := f.config.Repositories[0]
	repo.Env["AGENT_ISSUE_BACKEND"] = "github"
	delete(repo.Env, "AGENT_LINEAR_PROJECT")
	f.config.Repositories[0] = repo
	f.runtime.Clients = map[string]LinearClient{repo.Path: client}
	f.runtime.Reader = client
	f.runtime.Repositories[repo.Path] = repo
	return g
}

func TestGitHubCancellationBeforeSquash(t *testing.T) {
	for _, when := range []string{"gate-cancel", "gate-human", "intent-cancel"} {
		t.Run(when, func(t *testing.T) {
			f := overlapFixture(t, false)
			g := githubSafetyFixture(t, f)
			gates := 0
			f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if s.Path != "bash" {
					return runx.Run(ctx, s)
				}
				gates++
				g.mu.Lock()
				if when != "intent-cancel" {
					g.stop(when == "gate-human")
				}
				g.mu.Unlock()
				return []byte("UNIT executor\nVERDICT green\nSTEP all\nREASON checked\nEVIDENCE test\nSCOPE feature shared\nLANE 1"), nil
			}
			if when == "intent-cancel" {
				g.afterIntent = func() { g.stop(false) }
			}
			err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
			if gates != 1 || f.puts != 0 || f.merged || err == nil {
				t.Fatalf("stop ignored: gates=%d squash=%d merged=%v error=%v", gates, f.puts, f.merged, err)
			}
		})
	}
}

func TestGitHubStoppedIntentRetainsDeploymentBarrier(t *testing.T) {
	for _, tc := range []struct {
		name           string
		merged, failed bool
		sameVersion    bool
	}{
		{"canceled-unmerged", false, false, false}, {"canceled-merged", true, false, false},
		{"failed-unmerged", false, true, false}, {"failed-merged", true, true, false},
		{"same-version-canceled-merged", true, false, true},
		{"same-version-failed-unmerged", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := tc.merged
			f := newMergeFixture(t)
			g := githubSafetyFixture(t, f)
			rows, err := f.runtime.Snapshot(context.Background(), f.config.Repositories)
			if err != nil || len(rows) != 1 {
				t.Fatalf("snapshot: %v %v", rows, err)
			}
			h := rows[0].Handoff
			p := merge.Progress{Version: 1, QueueID: h.QueueID, TicketID: h.TicketID, Repository: h.Repository, PR: h.PR, Head: h.Head, Main: f.main, Phase: "merge-intent", Attempt: 1, Deployment: &merge.DeploymentProgress{State: "pending", Smoke: "true"}}
			b, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			g.mu.Lock()
			before := g.row["updated_at"]
			g.comment(merge.ProgressPrefix + string(b))
			if tc.sameVersion {
				g.row["updated_at"] = before
			}
			if tc.failed {
				g.row["labels"] = []any{map[string]any{"name": "auto-failed"}}
			} else {
				g.stop(false)
			}
			g.mu.Unlock()
			f.merged = merged
			smoke := 0
			f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if s.Path == "bash" {
					smoke++
					return nil, nil
				}
				return runx.Run(ctx, s)
			}
			err = (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
			if f.puts != 0 {
				t.Fatalf("canceled intent authorized squash: %d", f.puts)
			}
			rows, readErr := f.runtime.Snapshot(context.Background(), f.config.Repositories)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !merged && (err == nil || smoke != 0 || len(rows) != 1 || !DeploymentBarrier(rows[0])) {
				t.Fatalf("lost canceled barrier: %v smoke=%d rows=%+v", err, smoke, rows)
			}
			if merged && (err != nil || smoke != 1) {
				t.Fatalf("merged recovery blocked: %v smoke=%d", err, smoke)
			}
		})
	}
}

func TestGitHubFailedRecoveryNeverStartsExecutor(t *testing.T) {
	for _, when := range []string{"before-resume", "before-launch", "allowed-launch"} {
		t.Run(when, func(t *testing.T) {
			f := newMergeFixture(t)
			g := githubSafetyFixture(t, f)
			f.runtime.DriverPath = "/test/driver"
			g.mu.Lock()
			g.comments = nil
			g.comment("阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=1")
			if when == "before-resume" {
				g.row["labels"] = []any{map[string]any{"name": "auto-failed"}}
			}
			g.mu.Unlock()
			rows, err := f.runtime.Snapshot(context.Background(), f.config.Repositories)
			if err != nil || len(rows) != 1 {
				t.Fatalf("snapshot: %+v %v", rows, err)
			}
			if when == "before-resume" {
				err = f.runtime.Apply(context.Background(), rows[0], Action{Kind: Resume, Attempt: 2, Phase: rows[0].Phase})
				g.mu.Lock()
				comments := len(g.comments)
				g.mu.Unlock()
				if err == nil || comments != 1 {
					t.Fatalf("failed issue resumed: err=%v comments=%d", err, comments)
				}
			} else {
				g.mu.Lock()
				if when == "before-launch" {
					g.row["labels"] = []any{map[string]any{"name": "auto-failed"}}
				}
				g.mu.Unlock()
				err = f.runtime.Launch(context.Background(), rows[0])
				if when == "allowed-launch" {
					if err != nil || len(f.runtime.Orca.(*fakeOrca).launches) != 1 {
						t.Fatalf("allowed launch blocked: %v", err)
					}
					return
				}
				if err == nil || len(f.runtime.Orca.(*fakeOrca).launches) != 0 {
					t.Fatalf("failed issue launched: err=%v", err)
				}
			}
		})
	}
}
