package issues

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
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
	lag      int
	lists    int
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
			f.lists++
			result = f.rows
			if f.lag > 0 {
				f.lag--
				result = f.rows[:len(f.rows)-1]
			}
		case path == "" && r.Method == "POST":
			n := len(f.rows) + 1
			row := map[string]any{"node_id": fmt.Sprintf("I_native_%d", n), "number": n, "title": input["title"], "body": input["body"], "state": "open", "created_at": "2026-10-06T00:00:00Z", "updated_at": "2026-10-06T00:00:00Z"}
			labels := []map[string]any{}
			for _, v := range input["labels"].([]any) {
				labels = append(labels, map[string]any{"name": v})
			}
			row["labels"] = labels
			f.rows = append(f.rows, row)
			result = row
			if f.fail == "list-lag" {
				f.lag = 2
			}
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
				page := 1
				if _, err := fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page); err != nil {
					t.Error(err)
					return
				}
				start := min((page-1)*100, len(comments))
				result = comments[start:min(start+100, len(comments))]
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
func TestImportPreservesNativeIssues(t *testing.T) {
	c, f := newImportServer(t)
	native, err := c.Create(context.Background(), CreateInput{Title: "existing native", Labels: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	before := f.writes
	raw := importRaw(t, importFixture())
	if _, err = Import(context.Background(), c, raw, false); err != nil || f.writes != before {
		t.Fatalf("native issue blocked preview: %v writes=%d", err, f.writes-before)
	}
	if _, err = Import(context.Background(), c, raw, true); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(context.Background(), native.ID)
	if err != nil || !reflect.DeepEqual(got, native) {
		t.Fatalf("native issue changed: %+v error=%v", got, err)
	}
	before = f.writes
	if _, err = Import(context.Background(), c, raw, true); err != nil || f.writes != before {
		t.Fatalf("repeat import: %v writes=%d", err, f.writes-before)
	}
	missing := importFixture()
	missing["issues"] = []any{}
	if _, err = Import(context.Background(), c, importRaw(t, missing), false); err == nil {
		t.Fatal("existing imported identity outside manifest accepted")
	}
}

func TestImportOversizedCommentRoundTrip(t *testing.T) {
	c, f := newImportServer(t)
	m := importFixture()
	comment := importIssue(m)["comments"].([]any)[0].(map[string]any)
	comment["body"] = strings.Repeat("中文\\\"\n", 15000)
	raw := importRaw(t, m)
	preview, err := Preview(raw, "o/r")
	if err != nil || preview["fragmentedComments"] != 1 {
		t.Fatalf("large source preview: %+v %v", preview, err)
	}
	f.fail = "comment-accepted"
	if _, err = Import(context.Background(), c, raw, true); err == nil {
		t.Fatal("uncertain fragment write succeeded")
	}
	if _, err = c.ListComments(context.Background(), importID); err == nil {
		t.Fatal("partial comment exposed as complete history")
	}
	if _, err = Import(context.Background(), c, raw, true); err != nil {
		t.Fatal(err)
	}
	got, err := c.ListComments(context.Background(), importID)
	var want []linear.Comment
	encoded, _ := json.Marshal(importIssue(m)["comments"])
	_ = json.Unmarshal(encoded, &want)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("large comment roundtrip: count=%d err=%v", len(got), err)
	}
	for _, row := range f.comments[1] {
		if len(row["body"].(string)) > importCommentLimit {
			t.Fatal("fragment exceeds destination byte limit")
		}
	}
	before := f.writes
	if _, err = Import(context.Background(), c, raw, true); err != nil || f.writes != before {
		t.Fatalf("fragment replay: %v writes=%d", err, f.writes-before)
	}
	f.comments[1][1]["body"] = f.comments[1][0]["body"]
	if _, err = c.ListComments(context.Background(), importID); err == nil {
		t.Fatal("duplicate fragment accepted")
	}
}

func TestImportFragmentTamperFailsClosed(t *testing.T) {
	rewrite := func(t *testing.T, row map[string]any, edit func(*importedFragment)) {
		t.Helper()
		body := row["body"].(string)
		encoded, caption, _ := strings.Cut(strings.TrimPrefix(body, importedFragmentMarker), " -->\n\n")
		var fragment importedFragment
		if err := json.Unmarshal([]byte(encoded), &fragment); err != nil {
			t.Fatal(err)
		}
		edit(&fragment)
		raw, err := json.Marshal(fragment)
		if err != nil {
			t.Fatal(err)
		}
		row["body"] = importedFragmentMarker + string(raw) + " -->\n\n" + caption
	}
	for _, tc := range []struct {
		name   string
		tamper func(*testing.T, []map[string]any) []map[string]any
	}{
		{"missing middle", func(_ *testing.T, rows []map[string]any) []map[string]any { return append(rows[:1:1], rows[2:]...) }},
		{"missing last", func(_ *testing.T, rows []map[string]any) []map[string]any {
			return append(rows[:len(rows)-2:len(rows)-2], rows[len(rows)-1])
		}},
		{"reordered", func(_ *testing.T, rows []map[string]any) []map[string]any {
			rows[0], rows[1] = rows[1], rows[0]
			return rows
		}},
		{"digest conflict", func(t *testing.T, rows []map[string]any) []map[string]any {
			rewrite(t, rows[1], func(f *importedFragment) {
				data, _ := base64.StdEncoding.DecodeString(f.Data)
				f.Data = base64.StdEncoding.EncodeToString(bytes.Replace(data, []byte("中"), []byte("文"), 1))
			})
			return rows
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_DRIVER_STATE_ROOT", t.TempDir())
			c, f := newImportServer(t)
			m := importFixture()
			importIssue(m)["comments"].([]any)[0].(map[string]any)["body"] = strings.Repeat("中文\\\"\n", 15000)
			raw := importRaw(t, m)
			if _, err := Import(context.Background(), c, raw, true); err != nil {
				t.Fatal(err)
			}
			if len(f.comments[1]) < 4 {
				t.Fatalf("fixture needs at least three fragments, got %d comments", len(f.comments[1]))
			}
			f.comments[1] = tc.tamper(t, f.comments[1])
			if _, err := c.ListComments(context.Background(), importID); err == nil {
				t.Fatal("tampered fragments decoded as complete history")
			}
			before := f.writes
			if _, err := Import(context.Background(), c, raw, true); err == nil || f.writes != before {
				t.Fatalf("tampered import resumed: %v writes=%d", err, f.writes-before)
			}
			if err := maintenance.Set("draining"); err != nil {
				t.Fatal(err)
			}
			if _, err := ReleaseHold(context.Background(), c, raw); err == nil || f.writes != before {
				t.Fatalf("tampered import released: %v writes=%d", err, f.writes-before)
			}
		})
	}
}

func TestPreviewRejectsCredentialShapesWithoutEcho(t *testing.T) {
	secrets := map[string]string{
		"github":      "gh" + "p_" + strings.Repeat("A1", 18),
		"linear":      "lin_" + "api_" + strings.Repeat("b2", 20),
		"anthropic":   "sk-" + "ant-" + strings.Repeat("c3", 20),
		"aws":         "AK" + "IA" + strings.Repeat("Q", 16),
		"private key": "-----BEGIN " + "RSA PRIVATE KEY-----",
		"url":         "https://user:" + "hunter2" + "@example.com/repo.git",
	}
	for name, secret := range secrets {
		for field, place := range map[string]func(map[string]any){
			"description": func(m map[string]any) { importIssue(m)["description"] = "see " + secret },
			"comment": func(m map[string]any) {
				importIssue(m)["comments"].([]any)[1].(map[string]any)["body"] = secret + " leaked"
			},
		} {
			t.Run(name+" in "+field, func(t *testing.T) {
				m := importFixture()
				place(m)
				_, err := Preview(importRaw(t, m), "o/r")
				if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "ZII-1") {
					t.Fatalf("credential shape not rejected by location: %v", err)
				}
			})
		}
	}
	m := importFixture()
	importIssue(m)["description"] = "task-runner risk-based desk-check sk-learn ghp_short https://example.com/a:b@c"
	if _, err := Preview(importRaw(t, m), "o/r"); err != nil {
		t.Fatalf("ordinary text rejected: %v", err)
	}
}

func TestImportAndReleaseKeepLocalRecoveryState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_DRIVER_STATE_ROOT", root)
	ticket := filepath.Join(root, "r", "ZII-1")
	if err := os.MkdirAll(ticket, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"executor.json": `{"Corrections":2,"Completed":false}`, "budget.json": `{"spent":7}`}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(ticket, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := newImportServer(t)
	raw := importRaw(t, importFixture())
	if _, err := Import(context.Background(), c, raw, true); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Set("draining"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseHold(context.Background(), c, raw); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(ticket)
	if err != nil || len(entries) != len(files) {
		t.Fatalf("recovery state entries changed: %d %v", len(entries), err)
	}
	for name, body := range files {
		got, err := os.ReadFile(filepath.Join(ticket, name))
		if err != nil || string(got) != body {
			t.Fatalf("%s changed: %q %v", name, got, err)
		}
	}
}

func TestImportWaitsForCreatedIssueToBeListed(t *testing.T) {
	c, f := newImportServer(t)
	m := importFixture()
	importIssue(m)["state"] = "Done"
	f.fail = "list-lag"
	if _, err := Import(context.Background(), c, importRaw(t, m), true); err != nil {
		t.Fatalf("import failed while the created issue was not yet listed: %v", err)
	}
	task, err := c.Read(context.Background(), importID)
	if err != nil || task.NativeState != "closed" || !task.Metadata.ImportHold {
		t.Fatalf("closed held import not converged: %+v %v", task, err)
	}
}

func TestReleaseHoldReadsDestinationListOnce(t *testing.T) {
	t.Setenv("AGENT_DRIVER_STATE_ROOT", t.TempDir())
	c, f := newImportServer(t)
	m := importFixture()
	base := importIssue(m)
	base["importHold"] = true
	issues := []any{base}
	for i := 2; i <= 3; i++ {
		clone := map[string]any{}
		for k, v := range base {
			clone[k] = v
		}
		clone["id"] = fmt.Sprintf("01234567-1234-1234-1234-01234567890%d", i)
		clone["identifier"] = fmt.Sprintf("ZII-%d", i)
		clone["comments"] = []any{map[string]any{"id": fmt.Sprintf("clone-%d", i), "body": "body", "createdAt": importTime, "updatedAt": importTime}}
		issues = append(issues, clone)
	}
	m["issues"] = issues
	raw := importRaw(t, m)
	if _, err := Import(context.Background(), c, raw, true); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Set("draining"); err != nil {
		t.Fatal(err)
	}
	f.lists = 0
	if _, err := ReleaseHold(context.Background(), c, raw); err != nil {
		t.Fatal(err)
	}
	if f.lists != 1 {
		t.Fatalf("release read-back listed the destination %d times for %d issues", f.lists, len(issues))
	}
}

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
	held, readErr := c.Read(context.Background(), importID)
	if readErr != nil || !held.Metadata.ImportHold || held.Complete {
		t.Fatalf("import released before controlled cutover: %+v %v", held, readErr)
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
	if task.ID != importID || task.Identifier != "ZII-1" || task.Description != importIssue(m)["description"] || !task.CreatedAt.Equal(at) || !task.Metadata.UpdatedAt.Equal(at) || !task.Metadata.ImportHold {
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
func TestReleaseHoldRequiresDrainAndCompleteSource(t *testing.T) {
	t.Setenv("AGENT_DRIVER_STATE_ROOT", t.TempDir())
	c, f := newImportServer(t)
	raw := importRaw(t, importFixture())
	if _, err := Import(context.Background(), c, raw, true); err != nil {
		t.Fatal(err)
	}
	before := f.writes
	if _, err := ReleaseHold(context.Background(), c, raw); err == nil || f.writes != before {
		t.Fatal("release allowed without drain")
	}
	if err := maintenance.Set("draining"); err != nil {
		t.Fatal(err)
	}
	original := f.comments[1][0]["body"]
	f.comments[1][0]["body"] = strings.Replace(original.(string), "frozen", "edited", 1)
	if _, err := ReleaseHold(context.Background(), c, raw); err == nil || f.writes != before {
		t.Fatal("tampered source released")
	}
	f.comments[1][0]["body"] = original
	result, err := ReleaseHold(context.Background(), c, raw)
	if err != nil || result.Released != 1 {
		t.Fatalf("controlled release failed: %+v %v", result, err)
	}
	task, err := c.Read(context.Background(), importID)
	if err != nil || task.Metadata.ImportHold {
		t.Fatalf("hold retained: %+v %v", task, err)
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
	if !first.Blocked() || !first.Metadata.ImportHold || !second.Metadata.ImportHold || len(first.Metadata.Related) != 1 || len(f.rows) != 2 {
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
			if task.NativeState != "closed" || task.State.Name != state || !task.Metadata.ImportHold || task.Complete {
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
	task, err := c.Read(context.Background(), importID)
	if err != nil {
		t.Fatal(err)
	}
	metadata := task.Metadata
	metadata.ImportHold = false
	if _, err = c.SetMetadata(context.Background(), importID, metadata); err != nil {
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
			if !ok || !strings.Contains(visible, "2 条来源评论") || !strings.Contains(visible, "driver issues read") {
				t.Fatal("history has no visible retrieval guidance")
			}
		})
	}
}
