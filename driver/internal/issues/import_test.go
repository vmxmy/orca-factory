package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

const importID = "01234567-1234-1234-1234-012345678901"
const importID2 = "01234567-1234-1234-1234-012345678902"
const importTime = "2025-01-02T03:04:05Z"

type importServer struct {
	mu       sync.Mutex
	rows     []map[string]any
	comments map[int][]map[string]any
	writes   int
	fail     string
}

func newImportServer(t *testing.T) (*GitHub, *importServer) {
	t.Helper()
	f := &importServer{rows: []map[string]any{}, comments: map[int][]map[string]any{}}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var input map[string]any
		if r.Method != "GET" {
			f.writes++
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		var result any
		path := strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues")
		switch {
		case path == "" && r.Method == "GET":
			result = f.rows
		case path == "" && r.Method == "POST":
			n := len(f.rows) + 1
			row := map[string]any{"number": n, "title": input["title"], "body": input["body"], "state": "open", "created_at": "2026-10-06T00:00:00Z", "updated_at": "2026-10-06T00:00:00Z"}
			labels := []map[string]any{}
			for _, v := range input["labels"].([]any) {
				labels = append(labels, map[string]any{"name": v})
			}
			row["labels"] = labels
			f.rows = append(f.rows, row)
			result = row
			if f.fail == "create-accepted" {
				f.fail = ""
				http.Error(w, "lost response", 500)
				return
			}
		case strings.HasSuffix(path, "/comments"):
			var n int
			_, _ = fmt.Sscanf(path, "/%d/comments", &n)
			if r.Method == "POST" {
				if f.fail == "comment-rejected" {
					f.fail = ""
					http.Error(w, "rejected", 500)
					return
				}
				row := map[string]any{"id": len(f.comments[n]) + 1, "body": input["body"], "created_at": "2026-10-06T00:00:00Z", "updated_at": "2026-10-06T00:00:00Z"}
				f.comments[n] = append(f.comments[n], row)
				result = row
				if f.fail == "comment-accepted" {
					f.fail = ""
					http.Error(w, "lost response", 500)
					return
				}
			} else {
				comments := f.comments[n]
				if comments == nil {
					comments = []map[string]any{}
				}
				result = comments
			}
		case r.Method == "PATCH":
			var n int
			_, _ = fmt.Sscanf(path, "/%d", &n)
			for k, v := range input {
				if k == "labels" {
					labels := []map[string]any{}
					for _, name := range v.([]any) {
						labels = append(labels, map[string]any{"name": name})
					}
					f.rows[n-1][k] = labels
				} else {
					f.rows[n-1][k] = v
				}
			}
			result = f.rows[n-1]
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 400)
			return
		}
		if err := json.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(s.Close)
	clock := time.Now()
	gh, err := github.New(github.Options{BaseURL: s.URL, Now: func() time.Time { return clock }, Sleep: func(_ context.Context, d time.Duration) error { clock = clock.Add(d); return nil }, Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	c, err := NewGitHub("o/r", gh)
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}
func importFixture() map[string]any {
	comment := func(id, body, at string) any {
		return map[string]any{"id": id, "body": body, "createdAt": at, "updatedAt": at}
	}
	return map[string]any{"version": 1, "repository": "o/r", "complete": true, "issues": []any{map[string]any{
		"id": importID, "identifier": "ZII-1", "title": "Original title", "description": "raw\n description\n", "state": "Todo", "labels": []string{"ready-for-agent", "source"}, "priority": 2, "estimate": nil, "blockers": []string{}, "related": []string{}, "importHold": false, "createdAt": importTime, "updatedAt": importTime, "commentsComplete": true, "relationsComplete": true,
		"comments": []any{comment("source-2", " frozen\n<!-- agent:payload {\"x\":1} -->\n", "2025-01-03T03:04:05Z"), comment("source-1", "earlier timestamp but later source order", importTime)},
	}}}
}
func importRaw(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func importIssue(m map[string]any) map[string]any { return m["issues"].([]any)[0].(map[string]any) }
func TestImportDryRunAndRepeat(t *testing.T) {
	c, f := newImportServer(t)
	m := importFixture()
	raw := importRaw(t, m)
	result, err := Import(context.Background(), c, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || result.Issues != 1 || f.writes != 0 {
		t.Fatalf("dry run: %+v writes=%d", result, f.writes)
	}
	if _, err = Import(context.Background(), c, raw, true); err != nil {
		t.Fatal(err)
	}
	before := f.writes
	if _, err = Import(context.Background(), c, raw, true); err != nil {
		t.Fatal(err)
	}
	if f.writes != before {
		t.Fatalf("repeat wrote %d times", f.writes-before)
	}
	task, err := c.Read(context.Background(), importID)
	if err != nil {
		t.Fatal(err)
	}
	at, _ := time.Parse(time.RFC3339, importTime)
	if task.ID != importID || task.Identifier != "ZII-1" || task.Description != importIssue(m)["description"] || !task.CreatedAt.Equal(at) || !task.UpdatedAt.Equal(at) || task.Metadata.ImportHold {
		t.Fatalf("source or hold lost: %+v", task)
	}
	got, err := c.ListComments(context.Background(), importID)
	if err != nil {
		t.Fatal(err)
	}
	var want []linear.Comment
	encoded, _ := json.Marshal(importIssue(m)["comments"])
	_ = json.Unmarshal(encoded, &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("comments changed: got %+v want %+v", got, want)
	}
}
func TestImportInterruptionHeldAndResume(t *testing.T) {
	for _, failure := range []string{"create-accepted", "comment-accepted", "comment-rejected"} {
		t.Run(failure, func(t *testing.T) {
			c, f := newImportServer(t)
			raw := importRaw(t, importFixture())
			f.fail = failure
			if _, err := Import(context.Background(), c, raw, true); err == nil {
				t.Fatal("interruption hidden")
			}
			if len(f.rows) != 1 {
				t.Fatalf("created %d issues", len(f.rows))
			}
			task, err := c.Read(context.Background(), importID)
			if err != nil {
				t.Fatal(err)
			}
			if !task.Metadata.ImportHold || !task.Blocked() {
				t.Fatal("partial import runnable")
			}
			if _, err := Import(context.Background(), c, raw, true); err != nil {
				t.Fatal(err)
			}
			if len(f.rows) != 1 || len(f.comments[1]) != 1 {
				t.Fatalf("duplicate replay: issues=%d comments=%d", len(f.rows), len(f.comments[1]))
			}
		})
	}
}
func TestImportRejectsPartialSourceBeforeWrites(t *testing.T) {
	cases := map[string]func(map[string]any){
		"incomplete":            func(m map[string]any) { m["complete"] = false },
		"missing title":         func(m map[string]any) { delete(importIssue(m), "title") },
		"missing priority":      func(m map[string]any) { delete(importIssue(m), "priority") },
		"null labels":           func(m map[string]any) { importIssue(m)["labels"] = nil },
		"missing comments":      func(m map[string]any) { delete(importIssue(m), "comments") },
		"partial comments":      func(m map[string]any) { importIssue(m)["commentsComplete"] = false },
		"partial relations":     func(m map[string]any) { importIssue(m)["relationsComplete"] = false },
		"foreign repository":    func(m map[string]any) { m["repository"] = "other/repo" },
		"foreign identifier":    func(m map[string]any) { importIssue(m)["identifier"] = "other/repo#1" },
		"duplicate issue":       func(m map[string]any) { m["issues"] = append(m["issues"].([]any), importIssue(m)) },
		"unresolved dependency": func(m map[string]any) { importIssue(m)["blockers"] = []string{importID2} },
		"missing timestamp":     func(m map[string]any) { delete(importIssue(m), "updatedAt") },
		"unknown field":         func(m map[string]any) { importIssue(m)["oops"] = true },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c, f := newImportServer(t)
			m := importFixture()
			change(m)
			if _, err := Import(context.Background(), c, importRaw(t, m), true); err == nil {
				t.Fatal("invalid manifest accepted")
			}
			if f.writes != 0 {
				t.Fatalf("invalid source wrote %d", f.writes)
			}
		})
	}
}
func TestImportRejectsLiveConflict(t *testing.T) {
	c, f := newImportServer(t)
	raw := importRaw(t, importFixture())
	if _, err := Import(context.Background(), c, raw, true); err != nil {
		t.Fatal(err)
	}
	f.rows[0]["title"] = "live edit"
	before := f.writes
	if _, err := Import(context.Background(), c, raw, true); err == nil {
		t.Fatal("live edit overwritten")
	}
	if f.writes != before {
		t.Fatal("conflict caused writes")
	}
}
func TestImportPreservesHeldAndDependencies(t *testing.T) {
	c, f := newImportServer(t)
	m := importFixture()
	a := importIssue(m)
	b := importIssue(importFixture())
	b["id"] = importID2
	b["identifier"] = "ZII-2"
	b["comments"] = []any{}
	b["importHold"] = true
	a["blockers"] = []string{importID2}
	a["related"] = []string{importID2}
	m["issues"] = []any{a, b}
	if _, err := Import(context.Background(), c, importRaw(t, m), true); err != nil {
		t.Fatal(err)
	}
	first, err := c.Read(context.Background(), importID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Read(context.Background(), importID2)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Blocked() || first.Metadata.ImportHold || !second.Metadata.ImportHold || len(first.Metadata.Related) != 1 || len(f.rows) != 2 {
		t.Fatalf("holds/relations lost: %+v %+v", first, second)
	}
}
func TestImportCommentDecoder(t *testing.T) {
	normal := linear.Comment{ID: "normal", Body: "unchanged\n", CreatedAt: time.Now()}
	got, err := DecodeImportedComments(normal)
	if err != nil || !reflect.DeepEqual(got, []linear.Comment{normal}) {
		t.Fatal("normal comment changed")
	}
}

func TestImportBatchesHistoryWithinLimit(t *testing.T) {
	c, f := newImportServer(t)
	m := importFixture()
	comments := []any{}
	for i := 0; i < 60; i++ {
		comments = append(comments, map[string]any{"id": fmt.Sprintf("c-%d", i), "body": strings.Repeat("原始 ", 200), "createdAt": importTime, "updatedAt": importTime})
	}
	importIssue(m)["comments"] = comments
	if _, err := Import(context.Background(), c, importRaw(t, m), true); err != nil {
		t.Fatal(err)
	}
	if len(f.comments[1]) >= len(comments) || len(f.comments[1]) < 2 {
		t.Fatalf("history not bounded/batched: %d writes", len(f.comments[1]))
	}
	for _, batch := range f.comments[1] {
		if len(batch["body"].(string)) > 50000 {
			t.Fatal("history batch exceeds UTF8 byte limit")
		}
	}
	got, err := c.ListComments(context.Background(), importID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(comments) || got[59].ID != "c-59" || got[0].Body != comments[0].(map[string]any)["body"] {
		t.Fatalf("batched history lost data: %d comments", len(got))
	}
}

func TestImportRejectsAmbiguousJSON(t *testing.T) {
	raw := string(importRaw(t, importFixture()))
	cases := []string{
		strings.Replace(raw, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(raw, `"identifier":"ZII-1"`, `"identifier":"ZII-1","Identifier":"ZII-1"`, 1),
		strings.Replace(raw, `"title":"Original title"`, "\"title\":\"bad\xff\"", 1),
		raw + ` {}`, raw[:len(raw)-1],
	}
	for i, body := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			c, f := newImportServer(t)
			if _, err := Import(context.Background(), c, []byte(body), true); err == nil {
				t.Fatal("ambiguous JSON accepted")
			}
			if f.writes != 0 {
				t.Fatal("ambiguous JSON caused writes")
			}
		})
	}
}
func TestImportHistoryTamperStaysHeld(t *testing.T) {
	c, f := newImportServer(t)
	raw := importRaw(t, importFixture())
	f.fail = "comment-accepted"
	if _, err := Import(context.Background(), c, raw, true); err == nil {
		t.Fatal("expected uncertain write")
	}
	f.comments[1][0]["body"] = strings.Replace(f.comments[1][0]["body"].(string), "frozen", "edited", 1)
	before := f.writes
	if _, err := Import(context.Background(), c, raw, true); err == nil {
		t.Fatal("corrupt history accepted")
	}
	task, err := c.Read(context.Background(), importID)
	if err != nil {
		t.Fatal(err)
	}
	if !task.Metadata.ImportHold || f.writes != before {
		t.Fatal("corrupt history released or overwritten")
	}
}
func TestImportPreflightsWholeDestination(t *testing.T) {
	c, f := newImportServer(t)
	m := importFixture()
	b := importIssue(importFixture())
	b["id"] = importID2
	b["identifier"] = "ZII-2"
	b["comments"] = []any{}
	m["issues"] = append(m["issues"].([]any), b)
	raw := importRaw(t, m)
	if _, err := Import(context.Background(), c, raw, true); err != nil {
		t.Fatal(err)
	}
	// First source task is missing; a conflict on the later task must prevent its creation.
	f.rows = f.rows[1:]
	f.rows[0]["title"] = "human edit"
	before := f.writes
	if _, err := Import(context.Background(), c, raw, true); err == nil {
		t.Fatal("later conflict accepted")
	}
	if f.writes != before {
		t.Fatal("earlier missing issue written before full preflight")
	}
}

func TestImportTerminalCreateInterruption(t *testing.T) {
	for _, state := range []string{"Done", "Canceled", "Cancelled", "Duplicate"} {
		t.Run(state, func(t *testing.T) {
			c, f := newImportServer(t)
			m := importFixture()
			importIssue(m)["state"] = state
			raw := importRaw(t, m)
			f.fail = "create-accepted"
			if _, err := Import(context.Background(), c, raw, true); err == nil {
				t.Fatal("expected uncertain create")
			}
			if _, err := Import(context.Background(), c, raw, true); err != nil {
				t.Fatal(err)
			}
			task, err := c.Read(context.Background(), importID)
			if err != nil {
				t.Fatal(err)
			}
			if task.NativeState != "closed" || task.State.Name != state || task.Metadata.ImportHold || !task.Complete {
				t.Fatalf("terminal import not finished: native=%s state=%s hold=%v complete=%v", task.NativeState, task.State.Name, task.Metadata.ImportHold, task.Complete)
			}
		})
	}
}
func TestImportRejectsReopenedReleasedTask(t *testing.T) {
	c, f := newImportServer(t)
	m := importFixture()
	importIssue(m)["state"] = "Done"
	raw := importRaw(t, m)
	if _, err := Import(context.Background(), c, raw, true); err != nil {
		t.Fatal(err)
	}
	f.rows[0]["state"] = "open"
	before := f.writes
	if _, err := Import(context.Background(), c, raw, true); err == nil {
		t.Fatal("reopened completed import accepted")
	}
	if f.writes != before {
		t.Fatal("reopened task changed")
	}
}

func TestImportPreservesSourceTimestampSkew(t *testing.T) {
	c, _ := newImportServer(t)
	m := importFixture()
	source := importIssue(m)["comments"].([]any)[0].(map[string]any)
	source["createdAt"] = "2026-10-05T21:22:43.097Z"
	source["updatedAt"] = "2026-10-05T21:22:43.071Z"
	if _, err := Import(context.Background(), c, importRaw(t, m), true); err != nil {
		t.Fatal(err)
	}
	got, err := c.ListComments(context.Background(), importID)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].UpdatedAt.Format(time.RFC3339Nano) != source["updatedAt"] || got[0].CreatedAt.Format(time.RFC3339Nano) != source["createdAt"] {
		t.Fatal("source timestamps normalized")
	}
}

func TestImportOtherTeamAndVisibleHistory(t *testing.T) {
	for _, identifier := range []string{"OPS-42", "ZII-1"} {
		t.Run(identifier, func(t *testing.T) {
			c, f := newImportServer(t)
			m := importFixture()
			importIssue(m)["identifier"] = identifier
			if _, err := Import(context.Background(), c, importRaw(t, m), true); err != nil {
				t.Fatal(err)
			}
			body := f.comments[1][0]["body"].(string)
			_, visible, ok := strings.Cut(body, " -->")
			if !ok || !strings.Contains(visible, "2 source comments") || !strings.Contains(visible, "driver issues read") {
				t.Fatal("history has no visible retrieval guidance")
			}
		})
	}
}
