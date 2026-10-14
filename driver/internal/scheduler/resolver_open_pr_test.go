package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

func TestResolverOpenPR(t *testing.T) {
	prRaw := recorded(t, "github-pr-open-clean.json")
	var pr github.PullRequest
	if err := json.Unmarshal(prRaw, &pr); err != nil {
		t.Fatal(err)
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(recorded(t, "github-commit-main.json"), &commit); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"create", "existing", "mismatch", "missing", "create-failed", "lookup-failed", "pr-head-changed"} {
		t.Run(mode, func(t *testing.T) {
			head := pr.Head.SHA
			creates, lookups := 0, 0
			prResponse := prRaw
			if mode == "pr-head-changed" {
				var changed map[string]any
				if err := json.Unmarshal(prRaw, &changed); err != nil {
					t.Fatal(err)
				}
				changed["head"].(map[string]any)["sha"] = commit.SHA
				var err error
				prResponse, err = json.Marshal(changed)
				if err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/commits/"):
					if mode == "missing" {
						http.NotFound(w, r)
						return
					}
					remote := commit
					if mode != "mismatch" {
						remote.SHA = head
					}
					_ = json.NewEncoder(w).Encode(remote)
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
					lookups++
					if mode == "lookup-failed" {
						http.Error(w, "denied", 403)
						return
					}
					if mode == "existing" {
						_, _ = w.Write(append(append([]byte("["), prRaw...), ']'))
					} else {
						_ = json.NewEncoder(w).Encode([]github.PullRequest{})
					}
				case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
					creates++
					var input github.CreatePRRequest
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.Head != "agent/OF-5/executor" || input.Base != "main" || input.Title != "Recover PR" || !strings.HasPrefix(input.Body, "关联票: https://ledger.example/OF-5\n") || !strings.Contains(input.Body, "Verified fix") {
						t.Errorf("wrong PR: %+v", input)
					}
					if mode == "create-failed" {
						http.Error(w, "invalid", 422)
						return
					}
					_, _ = w.Write(prResponse)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			gh, err := github.New(github.Options{BaseURL: server.URL, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			l := &fakeLinear{}
			runtime := &Runtime{GitHub: gh, Clients: map[string]LinearClient{"/repo": l}, Repositories: map[string]config.Repository{"/repo": {Env: map[string]string{"AGENT_GH_REPO": "o/r"}}}}
			raw, _ := json.Marshal(map[string]string{"action": "open-pr", "branch": "agent/OF-5/executor", "head": head, "title": "Recover PR", "body": "Verified fix", "reason": "gate passed"})
			ticket := Ticket{ID: "id", Identifier: "OF-5", URL: "https://ledger.example/OF-5", Repo: "/repo", Owner: machine.OwnerResolver, Resolution: &resolver.Record{Version: 1, Attempt: 2, Phase: "result", Result: string(raw)}}
			if err := runtime.applyResolution(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			comments := make([]linear.Comment, len(l.comments))
			for i, body := range l.comments {
				comments[i] = linear.Comment{Body: body}
			}
			record, err := readResolution(comments)
			if err != nil || record == nil {
				t.Fatalf("missing receipt: %+v %v", l.comments, err)
			}
			success := mode == "create" || mode == "existing"
			if success {
				if record.Phase != "waiting" || !record.RetryAt.IsZero() || record.Attempt != 2 || record.Outcome != "succeeded" {
					t.Fatalf("next attempt not ready: %+v", record)
				}
				if !strings.Contains(strings.Join(l.comments, "\n"), fmt.Sprintf("pr=%d head=%s", pr.Number, head)) {
					t.Fatalf("missing PR evidence: %+v", l.comments)
				}
				f := &fakeBackend{tickets: []Ticket{{ID: "id", Identifier: "OF-5", Repo: "a", Owner: machine.OwnerResolver, Resolution: record}}}
				if err := setup(f).Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(f.launched) != 1 {
					t.Fatalf("next resolver was not launched: %+v", f.launched)
				}

			} else {
				if record.Phase != "human" || record.Outcome != "failed" || !strings.Contains(strings.Join(l.comments, "\n"), "归属: 人") {
					t.Fatalf("failure not stopped: %+v", l.comments)
				}
				f := &fakeBackend{tickets: []Ticket{{ID: "id", Identifier: "OF-5", Repo: "a", Owner: machine.OwnerHuman, Resolution: record}}}
				if err := setup(f).Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(f.launched) != 0 || len(f.actions) != 0 {
					t.Fatalf("failure retried: %+v %+v", f.launched, f.actions)
				}
			}
			wantCreates := 0
			if mode == "create" || mode == "create-failed" || mode == "pr-head-changed" {
				wantCreates = 1
			}
			if creates != wantCreates {
				t.Fatalf("POST count %d want %d", creates, wantCreates)
			}
			if (mode == "missing" || mode == "mismatch") && lookups != 0 {
				t.Fatal("looked up PR before confirming head")
			}
		})
	}
}
