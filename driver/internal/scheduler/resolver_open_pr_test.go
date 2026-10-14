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
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
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
	for _, mode := range []string{"create", "existing", "existing-no-reference", "existing-wrong-ticket", "existing-wrong-canonical", "mismatch", "missing", "create-failed", "lookup-failed", "pr-head-changed", "ticket-identity-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			base := newFeishuClaimBase(t, true)
			feishuServer := httptest.NewServer(base)
			t.Cleanup(feishuServer.Close)
			client, err := feishu.New(feishu.Options{Endpoint: feishuServer.URL, AppID: "cli_test", AppSecret: "secret"})
			if err != nil {
				t.Fatal(err)
			}
			store, err := issues.NewFeishu(ctx, client, "BASE_TOKEN", "https://ledger.example/base/BASE_TOKEN", feishuClaimRepo, issues.FeishuOptions{})
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := store.List(ctx)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("ticket list: %+v %v", tasks, err)
			}
			task := tasks[0]
			if task.URL != "https://ledger.example/base/BASE_TOKEN?table=tblT&record=recA" {
				t.Fatalf("ticket link: %q", task.URL)
			}
			head := pr.Head.SHA
			creates, lookups := 0, 0
			prResponse := prRaw
			if strings.HasPrefix(mode, "existing") {
				existing := pr
				existing.Body = fmt.Sprintf("关联票: %s\n\nCanonical identifier: %s\nCanonical ID: %s\n\nEarlier verified fix", task.URL, task.Identifier, task.ID)
				switch mode {
				case "existing-no-reference":
					existing.Body = "Earlier verified fix"
				case "existing-wrong-ticket":
					existing.Body = strings.Replace(existing.Body, task.URL, task.URL+"-other", 1)
				case "existing-wrong-canonical":
					existing.Body = strings.Replace(existing.Body, "Canonical ID: "+task.ID, "Canonical ID: other", 1)
				}
				prResponse, err = json.Marshal(existing)
				if err != nil {
					t.Fatal(err)
				}
			}
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
						http.Error(w, "denied", http.StatusForbidden)
						return
					}
					if strings.HasPrefix(mode, "existing") {
						_, _ = w.Write(append(append([]byte("["), prResponse...), ']'))
					} else {
						_ = json.NewEncoder(w).Encode([]github.PullRequest{})
					}
				case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
					creates++
					var input github.CreatePRRequest
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.Head != "agent/OF-5/executor" || input.Base != "main" || input.Title != "Recover PR" ||
						!strings.HasPrefix(input.Body, "关联票: "+task.URL+"\n") ||
						!strings.Contains(input.Body, "Canonical identifier: "+task.Identifier+"\n") ||
						!strings.Contains(input.Body, "Canonical ID: "+task.ID+"\n") ||
						!strings.Contains(input.Body, "Verified fix") {
						t.Errorf("wrong PR: %+v", input)
					}
					if mode == "create-failed" {
						http.Error(w, "invalid", http.StatusUnprocessableEntity)
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
			runtime := &Runtime{GitHub: gh, Clients: map[string]LinearClient{"/repo": store}, Repositories: map[string]config.Repository{"/repo": {Env: map[string]string{"AGENT_GH_REPO": "o/r"}}}}
			raw, _ := json.Marshal(map[string]string{"action": "open-pr", "branch": "agent/OF-5/executor", "head": head, "title": "Recover PR", "body": "Verified fix", "reason": "gate passed"})
			ticket := Ticket{ID: task.ID, Identifier: task.Identifier, Repo: "/repo", Owner: machine.OwnerResolver, Phase: machine.PhaseResolver, Resolution: &resolver.Record{Version: 1, Attempt: 2, Phase: "result", Result: string(raw)}}
			if mode == "ticket-identity-mismatch" {
				ticket.Identifier += "-changed"
			}
			initial, err := json.Marshal(ticket.Resolution)
			if err != nil {
				t.Fatal(err)
			}
			base.mu.Lock()
			base.rows["tblE"] = []map[string]any{{
				"_id": "recE0", "票据": []feishu.LinkRef{{ID: "recA"}},
				"创建时间": "2026-10-15T00:09:00.000+08:00", "修改时间": "2026-10-15T00:09:00.000+08:00",
				"正文": "阶段: Agent处置\n归属: 处置Agent\n" + resolver.Prefix + string(initial),
			}}
			base.mu.Unlock()
			if err := runtime.applyResolution(ctx, ticket); err != nil {
				t.Fatal(err)
			}
			base.mu.Lock()
			events := base.events
			body := ""
			for _, row := range base.rows["tblE"] {
				if row["_id"] == "recE1" {
					body = row["正文"].(string)
				}
			}
			base.mu.Unlock()
			if events != 1 || body == "" {
				t.Fatalf("want one resolution event, got %d: %q", events, body)
			}
			record, err := readResolution([]linear.Comment{{Body: body}})
			if err != nil || record == nil {
				t.Fatalf("missing receipt: %q %v", body, err)
			}
			success := mode == "create" || mode == "existing"
			if success {
				if record.Phase != "waiting" || !record.RetryAt.IsZero() || record.Attempt != 2 || record.Outcome != "succeeded" {
					t.Fatalf("next attempt not ready: %+v", record)
				}
				if !strings.Contains(body, fmt.Sprintf("pr=%d head=%s", pr.Number, head)) {
					t.Fatalf("missing PR evidence: %q", body)
				}
				f := &fakeBackend{tickets: []Ticket{{ID: task.ID, Identifier: task.Identifier, Repo: "a", Owner: machine.OwnerResolver, Resolution: record}}}
				if err := setup(f).Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if len(f.launched) != 1 {
					t.Fatalf("next resolver was not launched: %+v", f.launched)
				}

			} else {
				if record.Phase != "human" || record.Outcome != "failed" || !strings.Contains(body, "归属: 人") {
					t.Fatalf("failure not stopped: %q", body)
				}
				if mode == "ticket-identity-mismatch" && !strings.Contains(body, "ticket reference identity differs") {
					t.Fatalf("missing ticket identity evidence: %q", body)
				}
				if strings.HasPrefix(mode, "existing-") && !strings.Contains(body, "PR ticket reference differs") {
					t.Fatalf("missing PR reference evidence: %q", body)
				}
				f := &fakeBackend{tickets: []Ticket{{ID: ticket.ID, Identifier: ticket.Identifier, Repo: "a", Owner: machine.OwnerHuman, Resolution: record}}}
				if err := setup(f).Tick(ctx); err != nil {
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
