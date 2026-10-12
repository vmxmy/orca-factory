package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type serveMergeLinear struct {
	scheduler.LinearClient
	issue    linear.Issue
	comments []linear.Comment
	writes   int
}

func (l *serveMergeLinear) Snapshot(context.Context, []string, time.Time) ([]linear.Issue, error) {
	return []linear.Issue{l.issue}, nil
}
func (l *serveMergeLinear) ListComments(context.Context, string) ([]linear.Comment, error) {
	return l.comments, nil
}
func (l *serveMergeLinear) CreateComment(_ context.Context, _ string, body string) (linear.Comment, error) {
	l.writes++
	c := linear.Comment{ID: fmt.Sprint(l.writes), CreatedAt: time.Now(), Body: body}
	l.comments = append(l.comments, c)
	return c, nil
}
func (l *serveMergeLinear) UpdateIssue(_ context.Context, _ string, state string, _ []string, _ []string) (linear.Issue, error) {
	l.writes++
	l.issue.State.Name = state
	return l.issue, nil
}

type serveMergeOrca struct{ scheduler.OrcaClient }

func (*serveMergeOrca) AutomationsList(context.Context) (orca.Receipt, error) {
	return orca.Receipt{"automations": json.RawMessage(`[]`)}, nil
}

func (*serveMergeOrca) WorkerList(context.Context, orca.ListOptions) (orca.Receipt, error) {
	return orca.Receipt{"workers": json.RawMessage(`[]`)}, nil
}
func (*serveMergeOrca) WorktreeSet(context.Context, orca.BoardCard) (orca.Receipt, error) {
	return orca.Receipt{}, nil
}

// Exercise the same status wrapper and Runtime that serve assembles. Only HTTP
// and subprocess boundaries are replaced; no Channel or Backend is mocked.
func TestServeRuntimeActuallyMergesAndDryRunIsReadOnly(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprint(dry), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := t.TempDir()
			t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
			head, main := strings.Repeat("b", 40), strings.Repeat("a", 40)
			merged := false
			puts, fetches := 0, 0
			pr := func() map[string]any {
				return map[string]any{"number": 7, "merged": merged, "state": "open", "draft": false, "mergeable": true, "mergeable_state": "clean", "merge_commit_sha": strings.Repeat("c", 40), "head": map[string]any{"sha": head, "ref": "agent/ENG-1/executor", "repo": map[string]any{"full_name": "o/r"}}, "base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "o/r"}}}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/7/merge"):
					var b map[string]string
					if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
						t.Error(err)
						return
					}
					if r.Method != "PUT" || b["sha"] != head || b["merge_method"] != "squash" {
						t.Errorf("wrong squash request %v", b)
					}
					puts++
					merged = true
					if _, err := fmt.Fprint(w, `{}`); err != nil {
						t.Error(err)
						return
					}
				case strings.HasSuffix(r.URL.Path, "/pulls/7"):
					if err := json.NewEncoder(w).Encode(pr()); err != nil {
						t.Error(err)
						return
					}
				case strings.HasSuffix(r.URL.Path, "/pulls"):
					if merged {
						if err := json.NewEncoder(w).Encode([]any{}); err != nil {
							t.Error(err)
							return
						}
					} else {
						if err := json.NewEncoder(w).Encode([]any{pr()}); err != nil {
							t.Error(err)
							return
						}
					}
				case strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
					if _, err := fmt.Fprintf(w, `{"object":{"sha":%q}}`, main); err != nil {
						t.Error(err)
						return
					}
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					if _, err := fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":1,"status":"completed","conclusion":"success","head_sha":%q}]}`, head); err != nil {
						t.Error(err)
						return
					}
				case strings.HasSuffix(r.URL.Path, "/status"):
					if _, err := fmt.Fprint(w, `{"total_count":0,"statuses":[]}`); err != nil {
						t.Error(err)
						return
					}
				default:
					t.Errorf("unexpected endpoint %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
			if err != nil {
				t.Fatal(err)
			}
			evidence := map[string]any{"version": 1, "ticket_id": "id", "identifier": "ENG-1", "repository": "o/r", "unit": "executor", "pr": 7, "head": head, "baseline": main, "gate_spec": "UNIT: executor\nCHECK: true\nSCOPE: feature\nLANE: 1", "gate_output": "UNIT executor\nVERDICT green\nSTEP all\nREASON passed\nEVIDENCE\nSCOPE feature\nLANE 1", "review_output": `{"verdict":"green","step":"review"}`, "lane": 1, "run": "run1", "task": "task1", "dispatch": "dispatch1", "worktree": dir, "attempt": 1, "queue_id": "q1", "enqueued_at": "2026-10-06T00:00:00Z"}
			raw, rawMarshalErr := json.Marshal(evidence)
			if rawMarshalErr != nil {
				t.Fatal(rawMarshalErr)
			}
			l := &serveMergeLinear{issue: linear.Issue{ID: "id", Identifier: "ENG-1", State: linear.NamedID{Name: "In Progress"}, Project: linear.NamedID{ID: "p"}}, comments: []linear.Comment{{ID: "handoff", Body: "归属: 合并通道\nmerge-handoff-v1: " + string(raw)}}}
			repo := config.Repository{Path: dir, MaxInFlight: 2, StaleHours: 8, Env: map[string]string{"AGENT_LINEAR_PROJECT": "p", "AGENT_GH_REPO": "o/r", "AGENT_SCOPE_AVOID": "0"}}
			cfg := config.Config{MaxInFlight: 2, TokenBucket: config.TokenBucket{Capacity: 2, RefillPerSecond: 1}, Repositories: []config.Repository{repo}}
			runtime := &scheduler.Runtime{GitHub: gh, Clients: map[string]scheduler.LinearClient{dir: l}, Repositories: map[string]config.Repository{dir: repo}, Orca: &serveMergeOrca{}, Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
				if s.Path != "git" || s.Dir != dir || s.Timeout <= 0 {
					t.Fatalf("unsafe command: %+v", s)
				}
				switch s.Args[0] {
				case "ls-remote":
					return nil, nil
				case "fetch":
					fetches++
					if strings.Join(s.Args, " ") != "fetch --no-tags origin "+head+" "+main {
						t.Fatalf("unfixed fetch %v", s.Args)
					}
					return nil, nil
				case "merge-base":
					return []byte(main + "\n"), nil
				case "diff":
					if strings.Join(s.Args, " ") == "diff --name-only --no-renames -z "+main+" "+head+" --" {
						return []byte("feature\x00"), nil
					}
					if strings.Join(s.Args, " ") == "diff --name-only --no-renames -z "+main+" "+main+" --" {
						return nil, nil
					}
				}
				t.Fatalf("unexpected command %v", s.Args)
				return nil, nil
			}}
			if dry {
				var out bytes.Buffer
				err = dryRunTick(ctx, cfg, runtime, &out, time.Now)
			} else {
				err = serveWithStatus(ctx, filepath.Join(dir, "serve.lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
					return &scheduler.Scheduler{Config: cfg, Backend: runtime}, runtime, nil
				}, func(context.Context) error { cancel(); return ctx.Err() }, func(e error) { t.Error(e) })
			}
			if err != nil {
				t.Fatal(err)
			}
			if dry {
				if puts != 0 || fetches != 0 || l.writes != 0 {
					t.Fatalf("dry-run wrote: merges=%d fetches=%d linear=%d", puts, fetches, l.writes)
				}
			} else {
				if puts != 1 || l.issue.State.Name != "Done" || !strings.Contains(l.comments[len(l.comments)-1].Body, "归属: 无") {
					t.Fatalf("serve not wired: merges=%d state=%s comments=%v", puts, l.issue.State.Name, l.comments)
				}
			}
		})
	}
}
