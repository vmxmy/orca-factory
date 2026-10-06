package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeGitHubOnlyInitialization(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	r := &Runtime{Orca: &fakeOrca{automations: receipt(`{"automations":[]}`)}}
	cfg := config.Config{Repositories: []config.Repository{{Path: "/github", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "o/r"}}}}
	err := r.initialize(context.Background(), cfg, func(context.Context, string) (LinearClient, error) {
		t.Fatal("GitHub selected but Linear initialized")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("GitHub-only startup: %v", err)
	}
	if r.Clients["/github"] == nil {
		t.Fatal("GitHub backend not bound")
	}
}

func TestGitHubClaimRefreshesOwnershipBeforeMutation(t *testing.T) {
	ctx := context.Background()
	body := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Todo","complete":true,"blockers":[],"related":[]} -->`
	changed := false
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes++
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			if changed {
				_, _ = fmt.Fprint(w, `[{"id":4,"body":"转人: manual stop\n归属: 人","created_at":"2026-10-06T00:00:01Z","updated_at":"2026-10-06T00:00:01Z"}]`)
			} else {
				_, _ = fmt.Fprint(w, `[]`)
			}
			return
		}
		row := fmt.Sprintf(`{"number":42,"body":%q,"state":"open","labels":[{"name":"ready-for-agent"},{"name":"auto-impl"}],"updated_at":"2026-10-06T00:00:00Z"}`, body)
		if r.Method == "GET" {
			_, _ = fmt.Fprintf(w, "[%s]", row)
		} else {
			_, _ = fmt.Fprint(w, row)
		}
	}))
	defer server.Close()
	gh, _ := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	client, _ := issues.NewGitHub("o/r", gh)
	if _, err := client.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SnapshotComments(ctx, "ZII-42"); err != nil {
		t.Fatal(err)
	}
	changed = true
	r := &Runtime{Clients: map[string]LinearClient{"/repo": client}}
	err := r.Apply(ctx, Ticket{ID: "01234567-1234-1234-1234-012345678901", Identifier: "ZII-42", Repo: "/repo", Candidate: true}, Action{Kind: Claim})
	if err == nil || !strings.Contains(err.Error(), "ownership") || writes != 0 {
		t.Fatalf("claim used cached ownership: error=%v writes=%d", err, writes)
	}
}

func TestGitHubRuntimeFrozenMergeRecoveryEndToEnd(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	f := newMergeFixture(t)
	canonical := "01234567-1234-1234-1234-012345678901"
	rewriteHandoff(t, f, func(h map[string]any) { h["ticket_id"] = canonical })
	body := fmt.Sprintf(`<!-- orca-task-v1: {"id":%q,"identifier":"ENG-1","project":"o/r","state":"In Progress","complete":true,"blockers":[],"related":[]} -->`, canonical)
	row := map[string]any{"number": 42, "title": "frozen execution", "body": body, "state": "open", "labels": []any{}, "updated_at": "2026-10-06T01:00:00Z"}
	comments := []map[string]any{{"id": 1, "body": f.linear.rows[0].Body, "created_at": f.linear.rows[0].CreatedAt, "updated_at": f.linear.rows[0].CreatedAt}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out any
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments"):
			if r.Method == "POST" {
				var input map[string]string
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				c := map[string]any{"id": len(comments) + 1, "body": input["body"], "created_at": time.Now(), "updated_at": time.Now()}
				comments = append(comments, c)
				out = c
			} else {
				out = comments
			}
		case r.Method == "PATCH":
			var patch map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Error(err)
			}
			for k, v := range patch {
				if k != "labels" {
					row[k] = v
				}
			}
			out = row
		case r.URL.Path == "/repos/o/r/issues":
			out = []any{row}
		default:
			t.Errorf("unexpected issue path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	ticks := int64(0)
	gh, err := github.New(github.Options{BaseURL: server.URL, HTTPClient: &http.Client{Transport: &http.Transport{}}, Token: func(context.Context) (string, error) { return "test", nil }, Now: func() time.Time { ticks++; return time.Unix(ticks, 0) }})
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
	f.runtime.Reader = client
	f.runtime.Clients = map[string]LinearClient{repo.Path: client}
	f.runtime.Repositories[repo.Path] = repo
	s := &Scheduler{Config: f.config, Backend: f.runtime}
	if err = s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	final, err := client.Read(context.Background(), canonical)
	if err != nil {
		t.Fatal(err)
	}
	if f.puts != 1 || !f.merged || final.State.Name != "Done" || final.ID != canonical || final.Identifier != "ENG-1" {
		t.Fatalf("GitHub merge changed identity or failed: puts=%d final=%+v", f.puts, final)
	}
	history, err := client.ListComments(context.Background(), canonical)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := InspectOwnership(history)
	if err != nil || ownership.Owner != "无" {
		t.Fatalf("GitHub ownership did not release: %+v %v", ownership, err)
	}
	if err = s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.puts != 1 {
		t.Fatal("recovered completed GitHub merge was replayed")
	}
}

func TestGitHubDoneMergeOwnerMayFinishDurableReceipt(t *testing.T) {
	body := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Done","complete":true,"blockers":[],"related":[]} -->`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_, _ = fmt.Fprint(w, `[{"id":1,"body":"归属: 合并通道","created_at":"2026-10-06T00:00:00Z","updated_at":"2026-10-06T00:00:00Z"}]`)
			return
		}
		_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":"closed","state_reason":"completed"}]`, body)
	}))
	defer server.Close()
	gh, _ := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	client, _ := issues.NewGitHub("o/r", gh)
	r := &Runtime{Clients: map[string]LinearClient{"/repo": client}}
	err := r.verifyGitHubDecision(context.Background(), Ticket{ID: "01234567-1234-1234-1234-012345678901", Identifier: "ZII-42", Repo: "/repo", Owner: "合并通道"}, "")
	if err != nil {
		t.Fatalf("Done before final ownership receipt stranded merge: %v", err)
	}
}
