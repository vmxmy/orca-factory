package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
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
			t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
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
			if !strings.HasPrefix(body, "Fixes https://github.example/acme/demo/issues/42\n") || !strings.Contains(body, "Canonical identifier: "+identifier+"\n") || !strings.Contains(body, "Canonical ID: 01234567-1234-1234-1234-012345678901\n") || pr.created[0].Head != "agent/"+identifier+"/executor" {
				t.Fatalf("missing URL or changed canonical identity: %+v", pr.created[0])
			}
		})
	}
}
