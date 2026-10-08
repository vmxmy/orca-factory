package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type canonicalPR struct {
	*fakeGitHub
	head string
}

func (p *canonicalPR) FindOpenPR(_ context.Context, owner, repo, head string) (*github.PullRequest, error) {
	if owner != "acme" || repo != "demo" || head != p.head {
		return nil, fmt.Errorf("wrong PR identity %s/%s %s", owner, repo, head)
	}
	return nil, nil
}

type observedOrca struct {
	*fakeOrca
	reads int
}

func (o *observedOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	o.reads++
	return o.fakeOrca.TaskList(ctx, run)
}

func TestGitHubClosedCheckpointRemainsReadOnlyObservable(t *testing.T) {
	e, oc, _, _, _, ticket := fixture(t, "new-sha")
	ticket.ID = "01234567-1234-1234-1234-012345678901"
	observed := &observedOrca{fakeOrca: oc}
	e.Orca = observed
	writes := 0
	raw, _ := json.Marshal(issues.Metadata{ID: ticket.ID, Identifier: ticket.Identifier, Project: "acme/demo", State: "Done", Complete: true, Blockers: []string{}, Related: []string{}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes++
			http.Error(w, "forbidden", 500)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			if _, err := fmt.Fprint(w, `[]`); err != nil {
				t.Error(err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode([]any{map[string]any{"number": 42, "state": "closed", "body": "<!-- orca-task-v1: " + string(raw) + " -->"}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	gh, _ := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	e.Linear, _ = issues.NewGitHub("acme/demo", gh)
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.Start(context.Background(), ticket)
	if err == nil || observed.reads != 1 || writes != 0 || len(oc.runRequests) != 0 || len(oc.workerRequests) != 0 || oc.releases != 0 {
		t.Fatalf("closed recovery err=%v reads=%d writes=%d runs=%d workers=%d releases=%d", err, observed.reads, writes, len(oc.runRequests), len(oc.workerRequests), oc.releases)
	}
}

func TestGitHubReplacementRequiresRemoteTaskEvidence(t *testing.T) {
	e, oc, _, _, tools, ticket := fixture(t, "new-sha")
	ticket.ID = "01234567-1234-1234-1234-012345678901"
	raw, err := json.Marshal(issues.Metadata{ID: ticket.ID, Identifier: ticket.Identifier, Project: "acme/demo", State: "In Progress", Complete: true, Blockers: []string{}, Related: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected mutation %s", r.Method)
			http.Error(w, "forbidden", 500)
			return
		}
		var out any = []any{map[string]any{"number": 42, "state": "open", "body": "<!-- orca-task-v1: " + string(raw) + " -->"}}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			out = []any{map[string]any{"id": 1, "body": "归属: 执行器 term=term_old run=run_one attempt=1", "created_at": time.Now()}}
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	e.Linear, err = issues.NewGitHub("acme/demo", gh)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "missing_task", Dispatch: "ctx_one"}); err != nil {
		t.Fatal(err)
	}
	_, err = e.Start(context.Background(), ticket)
	if err == nil || !strings.Contains(err.Error(), "replacement executor recovery identity unconfirmed") {
		t.Fatalf("missing remote task did not block replacement: %v", err)
	}
	if len(oc.runRequests) != 0 || len(oc.workerRequests) != 0 || oc.releases != 0 || tools.gateCalls != 0 {
		t.Fatal("unconfirmed replacement performed execution actions")
	}
}

func TestGitHubReplacementRetainsDispatchWithRecordedTaskShape(t *testing.T) {
	// #given
	e, oc, _, _, _, ticket := fixture(t, "new-sha")
	ticket.ID = "01234567-1234-1234-1234-012345678901"
	recorded := &realShapeOrca{fakeOrca: *oc}
	e.Orca = recorded
	oc = &recorded.fakeOrca
	raw, err := json.Marshal(issues.Metadata{ID: ticket.ID, Identifier: ticket.Identifier, Project: "acme/demo", State: "In Progress", Complete: true, Blockers: []string{}, Related: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "stop after recovery guard", http.StatusForbidden)
			return
		}
		var out any = []any{map[string]any{"number": 42, "state": "open", "body": "<!-- orca-task-v1: " + string(raw) + " -->"}}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			out = []any{map[string]any{"id": 1, "body": "归属: 执行器 term=term_old run=run_one attempt=1", "created_at": time.Now()}}
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	e.Linear, err = issues.NewGitHub("acme/demo", gh)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Worktree: "/worktrees/ZII-861-executor", Baseline: "old-sha"}); err != nil {
		t.Fatal(err)
	}
	// #when
	result, err := e.Start(context.Background(), ticket)
	// #then
	cp, readErr := readCheckpoint(e.Config, ticket)
	if readErr != nil || cp == nil || cp.Dispatch != "ctx_one" || err != nil && strings.Contains(err.Error(), "replacement executor recovery identity unconfirmed") || result.RunID != "run_one" || len(oc.runRequests) != 0 || len(oc.workerRequests) != 0 {
		t.Fatalf("recorded task recovery lost dispatch or started work: result=%+v err=%v runs=%v workers=%v", result, err, oc.runRequests, oc.workerRequests)
	}
}

func TestGitHubClosedTaskDoesNotDispatch(t *testing.T) {
	e, oc, _, _, _, ticket := fixture(t, "new-sha")
	ticket.ID = "01234567-1234-1234-1234-012345678901"
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes++
			http.Error(w, "write forbidden", 500)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			if _, err := fmt.Fprint(w, `[]`); err != nil {
				t.Error(err)
			}
			return
		}
		if _, err := fmt.Fprint(w, `[{"number":42,"state":"closed","body":"<!-- orca-task-v1: {\"id\":\"01234567-1234-1234-1234-012345678901\",\"identifier\":\"ZII-1\",\"project\":\"acme/demo\",\"state\":\"Todo\",\"complete\":true,\"blockers\":[],\"related\":[]} -->"}]`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	gh, _ := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	e.Linear, _ = issues.NewGitHub("acme/demo", gh)
	if _, err := e.Execute(context.Background(), ticket); err == nil {
		t.Fatal("closed task executed")
	}
	if writes != 0 {
		t.Fatalf("closed task wrote %d mutations", writes)
	}
	if len(oc.workerRequests) != 0 {
		t.Fatalf("closed task dispatched %d workers", len(oc.workerRequests))
	}
}

func TestGitHubInvalidRecoveryEvidenceDoesNotDispatch(t *testing.T) {
	for _, receipt := range []string{"merge-progress-v1: {}", "resolver-state {}"} {
		t.Run(receipt, func(t *testing.T) {
			// #given
			e, oc, _, _, _, ticket := fixture(t, "new-sha")
			ticket.ID = "01234567-1234-1234-1234-012345678901"
			raw, err := json.Marshal(issues.Metadata{ID: ticket.ID, Identifier: ticket.Identifier, Project: "acme/demo", State: "In Progress", Complete: true, Blockers: []string{}, Related: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				var out any = []any{map[string]any{"number": 42, "state": "open", "body": "<!-- orca-task-v1: " + string(raw) + " -->"}}
				if strings.HasSuffix(r.URL.Path, "/comments") {
					out = []any{map[string]any{"id": 1, "body": "归属: 执行器 term=- run=- attempt=1\n" + receipt}}
				}
				if err := json.NewEncoder(w).Encode(out); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
			if err != nil {
				t.Fatal(err)
			}
			e.Linear, err = issues.NewGitHub("acme/demo", gh)
			if err != nil {
				t.Fatal(err)
			}
			// #when
			_, err = e.Start(context.Background(), ticket)
			// #then
			if err == nil || writes != 0 || len(oc.runRequests) != 0 || len(oc.workerRequests) != 0 {
				t.Fatalf("invalid recovery evidence allowed execution: err=%v writes=%d runs=%d workers=%d", err, writes, len(oc.runRequests), len(oc.workerRequests))
			}
		})
	}
}

func TestGitHubPRReferencesIssueURLAndCanonicalIdentity(t *testing.T) {
	for _, identifier := range []string{"ZII-861", "GH-42"} {
		t.Run(identifier, func(t *testing.T) {
			e, _, _, pr, _, ticket := fixture(t, "new-sha")
			e.GitHub = &canonicalPR{fakeGitHub: pr, head: "agent/" + identifier + "/executor"}
			ticket.ID = "01234567-1234-1234-1234-012345678901"
			ticket.Identifier = identifier
			meta := issues.Metadata{ID: ticket.ID, Identifier: identifier, Project: "acme/demo", State: "In Progress", Complete: true, Blockers: []string{}, Related: []string{}}
			raw, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			row := map[string]any{"number": 42, "html_url": "https://github.example/acme/demo/issues/42", "body": "<!-- orca-task-v1: " + string(raw) + " -->", "state": "open", "labels": []any{}, "updated_at": time.Now()}
			comments := []map[string]any{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var out any
				switch {
				case strings.HasSuffix(r.URL.Path, "/comments"):
					if r.Method == "POST" {
						var input map[string]string
						if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
							t.Error(err)
							return
						}
						c := map[string]any{"id": len(comments) + 1, "body": input["body"], "created_at": time.Now(), "updated_at": time.Now()}
						comments = append(comments, c)
						out = c
					} else {
						out = comments
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
							row[k] = labels
						} else {
							row[k] = v
						}
					}
					out = row
				case r.Method == "GET" && r.URL.Path == "/repos/acme/demo/issues":
					out = []any{row}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if err := json.NewEncoder(w).Encode(out); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			var mu sync.Mutex
			tick := int64(0)
			gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }, Now: func() time.Time { mu.Lock(); defer mu.Unlock(); tick++; return time.Unix(tick, 0) }})
			if err != nil {
				t.Fatal(err)
			}
			e.Linear, err = issues.NewGitHub("acme/demo", gh)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = e.Execute(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			if len(pr.created) != 1 {
				t.Fatalf("PR creation: %+v", pr.created)
			}
			body := pr.created[0].Body
			if !strings.HasPrefix(body, "关联票: https://github.example/acme/demo/issues/42\n") || !strings.Contains(body, "Canonical identifier: "+identifier+"\n") || !strings.Contains(body, "Canonical ID: 01234567-1234-1234-1234-012345678901\n") || pr.created[0].Head != "agent/"+identifier+"/executor" {
				t.Fatalf("missing URL or changed canonical identity: %+v", pr.created[0])
			}
		})
	}
}
