package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestBackendRequiresExplicitStore(t *testing.T) {
	for _, value := range []string{"", "linear", "jira"} {
		if backend, err := Backend(map[string]string{"AGENT_ISSUE_BACKEND": value}); err == nil {
			t.Fatalf("AGENT_ISSUE_BACKEND=%q selected %q", value, backend)
		}
	}
}

const taskBody = `spec\n<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Todo","complete":true,"blockers":["missing"],"related":[]} -->`

func TestGitHubSnapshotIdentityAndUnknownBlocker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues" {
			t.Errorf("unexpected request: %s", r.URL)
			http.Error(w, "bad", 400)
			return
		}
		_, _ = fmt.Fprintf(w, `[{"number":42,"title":"task","body":%q,"state":"open","labels":[{"name":"ready-for-agent"}],"created_at":"2026-10-01T00:00:00Z"},{"number":43,"pull_request":{},"body":""}]`, taskBody)
	}))
	defer server.Close()
	gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewGitHub("o/r", gh)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.Snapshot(context.Background(), []string{"o/r"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "01234567-1234-1234-1234-012345678901" || rows[0].Identifier != "ZII-42" || !rows[0].Blocked() {
		t.Fatalf("identity or fail-closed blockers lost: %+v", rows)
	}
}
func TestGitHubRejectsIncompleteMetadata(t *testing.T) {
	_, err := ParseMetadata(`<!-- orca-task-v1: {"id":"x","identifier":"ZII-1","complete":false} -->`)
	if err == nil {
		t.Fatal("incomplete task accepted")
	}
}

func TestImportHoldVisibleButBlockedInRuntimeSnapshot(t *testing.T) {
	body := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"In Progress","complete":true,"importHold":true,"blockers":[],"related":[]} -->`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":"open"}]`, body)
	}))
	defer server.Close()
	gh, _ := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	c, _ := NewGitHub("o/r", gh)
	rows, err := c.Snapshot(context.Background(), []string{"o/r"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Blocked() {
		t.Fatalf("held imported task barrier became invisible or runnable: %+v", rows)
	}
}

// Comment edits need not change the issue timestamp; each page revalidates its ETag.
func TestListCommentsRefreshEditsWithoutIssueTimestampChange(t *testing.T) {
	issueReads, commentReads := 0, 0
	commentETag := `"comments-v1"`
	body := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Todo","complete":true,"blockers":[],"related":[]} -->`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/issues" {
			issueReads++
			_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":"open","updated_at":"2026-10-06T00:00:00Z"}]`, body)
			return
		}
		commentReads++
		if r.Header.Get("If-None-Match") == commentETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", commentETag)
		body := "ownership"
		if commentETag == `"comments-v2"` {
			body = "ownership edited"
		}
		_, _ = fmt.Fprintf(w, `[{"id":3,"body":%q,"created_at":"2026-10-06T00:00:00Z","updated_at":"2026-10-06T00:00:00Z"}]`, body)
	}))
	defer s.Close()
	gh, _ := github.New(github.Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	c, _ := NewGitHub("o/r", gh)
	var history []task.Comment
	for round := range 3 {
		if round == 2 {
			commentETag = `"comments-v2"`
		}
		if _, err := c.List(context.Background()); err != nil {
			t.Fatal(err)
		}
		var err error
		history, err = c.ListComments(context.Background(), "ZII-42")
		if err != nil {
			t.Fatal(err)
		}
	}
	if issueReads != 3 || commentReads != 3 || len(history) != 1 || history[0].Body != "ownership edited" {
		t.Fatalf("edited comments stayed cached: issues=%d comments=%d history=%+v", issueReads, commentReads, history)
	}
}

func TestGitHubCreateReconcilesLostResponse(t *testing.T) {
	var stored map[string]any
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			writes++
			if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
				t.Error(err)
			}
			stored["node_id"] = "I_native_7"
			stored["number"] = 7
			stored["state"] = "open"
			stored["labels"] = []any{}
			http.Error(w, "lost response", http.StatusServiceUnavailable)
			return
		}
		rows := []map[string]any{}
		if stored != nil {
			rows = append(rows, stored)
		}
		if err := json.NewEncoder(w).Encode(rows); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	gh, _ := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	c, _ := NewGitHub("o/r", gh)
	task, err := c.Create(context.Background(), CreateInput{Title: "native task"})
	if err != nil || task.Identifier != "GH-7" || task.ID == "" || writes != 1 {
		t.Fatalf("ambiguous create not reconciled: task=%+v error=%v writes=%d", task, err, writes)
	}
}

// node ID recorded through gh api repos/vmxmy/orca-factory/issues/401 on
// 2026-10-15; the field this test exercises, not a hand-written value.
func recordedGitHubNodeID(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/recorded/github/issues-401-node-id.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		NodeID string `json:"node_id"`
	}
	if err = json.Unmarshal(raw, &rows); err != nil || len(rows) != 1 || rows[0].NodeID == "" {
		t.Fatalf("recorded node id fixture malformed: %v", err)
	}
	return rows[0].NodeID
}

func TestGitHubCreateAdoptsBeforeListCatchesUp(t *testing.T) {
	nodeID := recordedGitHubNodeID(t)
	var mu sync.Mutex
	var created map[string]any
	lag := 2
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			created = map[string]any{"node_id": nodeID, "number": 1, "title": in["title"], "body": in["body"], "state": "open", "labels": []any{}}
			if err := json.NewEncoder(w).Encode(created); err != nil {
				t.Error(err)
			}
		case http.MethodPatch:
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			for k, v := range in {
				created[k] = v
			}
			if err := json.NewEncoder(w).Encode(created); err != nil {
				t.Error(err)
			}
		default:
			rows := []map[string]any{}
			if created != nil {
				if lag > 0 {
					lag--
				} else {
					rows = append(rows, created)
				}
			}
			if err := json.NewEncoder(w).Encode(rows); err != nil {
				t.Error(err)
			}
		}
	}))
	defer server.Close()
	gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewGitHub("o/r", gh)
	if err != nil {
		t.Fatal(err)
	}
	task, err := c.Create(context.Background(), CreateInput{Title: "native", Description: "## 要做\n施工\nCHECK: make test\nSCOPE: driver/\nDEPENDENCIES: {\"blockers\":[],\"related\":[]}", Labels: []string{}, Blockers: []string{}, Related: []string{}})
	if err != nil || !task.Metadata.Complete || task.Identifier != "GH-1" {
		t.Fatalf("create did not survive list lag before adoption: task=%+v err=%v", task, err)
	}
}

func TestMetadataRejectsAmbiguousAndMissingDependencies(t *testing.T) {
	for _, raw := range []string{
		`{"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Todo","complete":true}`,
		`{"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Todo","complete":true,"blockers":["unknown"],"blockers":[],"related":[]}`,
	} {
		if _, err := ParseMetadata("<!-- orca-task-v1: " + raw + " -->"); err == nil {
			t.Errorf("unsafe metadata accepted: %s", raw)
		}
	}
}

func TestGitHubReopenedBlockerStillBlocks(t *testing.T) {
	blocked := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Todo","complete":true,"blockers":["01234567-1234-1234-1234-012345678902"],"related":[]} -->`
	for name, blocker := range map[string]string{
		"reopened": `{"number":43,"body":%q,"state":"open"}`,
		"held":     `{"number":43,"body":%q,"state":"open"}`,
	} {
		meta := `"state":"Done","complete":true`
		if name == "held" {
			meta = `"state":"Done","complete":true,"importHold":true`
		}
		body := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678902","identifier":"ZII-43","project":"o/r",` + meta + `,"blockers":[],"related":[]} -->`
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":"open"},`+blocker+`]`, blocked, body)
			}))
			defer server.Close()
			gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
			if err != nil {
				t.Fatal(err)
			}
			c, err := NewGitHub("o/r", gh)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := c.List(context.Background())
			if err != nil || len(rows) != 2 || !rows[0].Blocked() {
				t.Fatalf("open blocker treated as completed: %+v err=%v", rows, err)
			}
		})
	}
}
