package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
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
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
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

func TestImportHoldExcludedFromRuntimeSnapshot(t *testing.T) {
	body := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"In Progress","complete":true,"importHold":true,"blockers":[],"related":[]} -->`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":"open"}]`, body)
	}))
	defer server.Close()
	gh, _ := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	c, _ := NewGitHub("o/r", gh)
	rows, err := c.Snapshot(context.Background(), []string{"o/r"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("held imported task exposed to runtime: %+v", rows)
	}
}

func TestUnchangedSnapshotsReuseLogicalCommentHistory(t *testing.T) {
	issueReads, commentReads := 0, 0
	body := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Todo","complete":true,"blockers":[],"related":[]} -->`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/issues" {
			issueReads++
			_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":"open","updated_at":"2026-10-06T00:00:00Z"}]`, body)
			return
		}
		commentReads++
		_, _ = fmt.Fprint(w, `[{"id":3,"body":"ownership","created_at":"2026-10-06T00:00:00Z","updated_at":"2026-10-06T00:00:00Z"}]`)
	}))
	defer s.Close()
	gh, _ := github.New(github.Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	c, _ := NewGitHub("o/r", gh)
	for range 2 {
		if _, err := c.List(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := c.SnapshotComments(context.Background(), "ZII-42"); err != nil {
			t.Fatal(err)
		}
	}
	if issueReads != 1 || commentReads != 1 {
		t.Fatalf("unchanged snapshots repeated history: issues=%d comments=%d", issueReads, commentReads)
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
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
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
