package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

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

func TestSnapshotCommentsRefreshEditsWithoutIssueTimestampChange(t *testing.T) {
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
	var history []linear.Comment
	for round := range 3 {
		if round == 2 {
			commentETag = `"comments-v2"`
		}
		if _, err := c.List(context.Background()); err != nil {
			t.Fatal(err)
		}
		var err error
		history, err = c.SnapshotComments(context.Background(), "ZII-42")
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
