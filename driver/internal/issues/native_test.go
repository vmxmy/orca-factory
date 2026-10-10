package issues

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
)

func TestNativeIssueVisibleHeldAndStable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[{"node_id":"I_native_7","number":7,"title":"native","body":"## 要做\n施工","state":"open","labels":[{"name":"ready-for-agent"}],"updated_at":"2026-10-08T00:00:00Z"}]`)
	}))
	defer server.Close()
	gh, _ := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	c, _ := NewGitHub("o/r", gh)
	rows, err := c.List(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("native issue disappeared: %v %v", rows, err)
	}
	first := rows[0]
	if first.Complete || !first.Blocked() || first.ID == "" || first.Identifier != "GH-7" {
		t.Fatalf("native issue gained implicit authorization: %+v", first)
	}
	rows, err = c.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ID != first.ID {
		t.Fatalf("native identity changed: %v %v", rows, err)
	}
}

func TestNativeCreateUsesNodeIdentity(t *testing.T) {
	c, f := newImportServer(t)
	task, err := c.Create(context.Background(), CreateInput{Title: "native", Description: "## 要做\n施工\nCHECK: make test\nSCOPE: driver/\nDEPENDENCIES: {\"blockers\":[],\"related\":[]}", Labels: []string{}, Blockers: []string{}, Related: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	raw := f.rows[0]
	raw["body"] = "native body without metadata"
	rows, err := c.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ID != task.ID {
		t.Fatalf("created identity differs from node: %v %v", rows, err)
	}
}

func TestAdoptRejectsIncompleteBrief(t *testing.T) {
	c, f := newImportServer(t)
	f.rows = []map[string]any{{"node_id": "I_native_7", "number": 1, "title": "native", "body": "ready-for-agent", "state": "open", "labels": []any{}}}
	_, err := c.Adopt(context.Background(), "GH-1", []byte(`{"blockers":[],"related":[]}`))
	if err == nil || f.writes != 0 {
		t.Fatalf("incomplete brief adopted: writes=%d err=%v", f.writes, err)
	}
}

func TestAdoptRejectsIncompleteDependency(t *testing.T) {
	c, f := newImportServer(t)
	f.rows = []map[string]any{
		{"node_id": "I_native_1", "number": 1, "title": "native", "body": "## 要做\n施工\nCHECK: make test\nSCOPE: driver/\nDEPENDENCIES: {\"blockers\":[\"" + importID + "\"],\"related\":[]}", "state": "open"},
		{"number": 2, "body": `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Done","complete":true,"blockers":[],"related":["missing"]} -->`, "state": "closed"},
	}
	_, err := c.Adopt(context.Background(), "GH-1", []byte(`{"blockers":["01234567-1234-1234-1234-012345678901"],"related":[]}`))
	if err == nil || f.writes != 0 {
		t.Fatalf("incomplete dependency adopted: writes=%d err=%v", f.writes, err)
	}
}

func TestNativeMutationRequiresAdoption(t *testing.T) {
	c, f := newImportServer(t)
	f.rows = []map[string]any{{"node_id": "I_native_7", "number": 1, "title": "native", "body": "ready-for-agent", "state": "open", "labels": []any{}}}
	if _, err := c.UpdateIssue(context.Background(), "GH-1", "In Progress", nil, nil); err == nil {
		t.Fatal("unmanaged state update accepted")
	}
	if _, err := c.Edit(context.Background(), "GH-1", []byte(`{"title":"edited"}`)); err == nil {
		t.Fatal("unmanaged edit accepted")
	}
	if f.writes != 0 {
		t.Fatalf("unmanaged mutation wrote %d requests", f.writes)
	}
}

func TestNativeCreateRejectsStateWithoutAdoption(t *testing.T) {
	c, f := newImportServer(t)
	if task, err := c.Create(context.Background(), CreateInput{Title: "native", State: "Done", Labels: []string{}}); err == nil {
		t.Fatalf("unmanaged native create silently ignored state: %+v", task)
	}
	if _, err := (&LedgerStore{Client: c}).CreateIssue(context.Background(), "ordinary task", "Backlog"); err == nil {
		t.Fatal("ledger creation bypassed ordinary task adoption")
	}
	if f.writes != 0 {
		t.Fatalf("rejected unmanaged creation wrote %d requests", f.writes)
	}
}

func TestRuntimeFreshnessUsesNativeTimestamp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":"open","updated_at":"2026-10-08T00:00:00Z"}]`, `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Todo","complete":true,"blockers":[],"related":[],"updatedAt":"2026-10-01T00:00:00Z"} -->`)
	}))
	defer server.Close()
	gh, _ := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	c, _ := NewGitHub("o/r", gh)
	task, err := c.Read(context.Background(), "ZII-42")
	if err != nil || !task.UpdatedAt.Equal(time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("source provenance replaced live freshness: %+v %v", task, err)
	}
}
