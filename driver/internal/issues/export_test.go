package issues_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

func TestExportRejectsIncompleteSource(t *testing.T) {
	_, err := issues.Export(context.Background(), nil, "o/r", "p")
	if err == nil {
		t.Fatal("missing authorized source exported")
	}
	if _, err := issues.Preview([]byte(`{"version":1,"repository":"o/r","complete":false,"issues":[]}`), "o/r"); err == nil {
		t.Fatal("incomplete export accepted")
	}
}

func exportFixture(t *testing.T, missingAlias *bool) (*linear.Client, *int) {
	t.Helper()
	t.Setenv("LINEAR_API_KEY", "test")
	calls := 0
	page := func(nodes any, more bool, after string) map[string]any {
		return map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": after}}
	}
	id := func(n int) string { return fmt.Sprintf("01234567-1234-1234-1234-%012d", n) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		var data any
		switch {
		case strings.Contains(q.Query, "Metadata"):
			field := "states"
			if strings.Contains(q.Query, "labels(") {
				field = "labels"
			}
			if strings.Contains(q.Query, "projects(") {
				field = "projects"
			}
			data = map[string]any{"team": map[string]any{field: page([]any{}, false, "")}}
		case strings.Contains(q.Query, "query ExportIssues("):
			calls++
			if !strings.Contains(q.Query, "includeArchived: true") || !strings.Contains(q.Query, "title description estimate parent { id }") {
				t.Error("export must select complete archived-inclusive source fields")
			}
			start := 1
			if cursor, ok := q.Variables["after"].(string); ok {
				start, _ = strconv.Atoi(cursor)
			}
			rows := []any{}
			end := min(start+20, 124)
			for n := start; n < end; n++ {
				row := map[string]any{"id": id(n), "identifier": fmt.Sprintf("ZII-%d", n), "project": map[string]any{"id": id(999)}, "state": map[string]any{"id": "todo", "name": "Todo"}, "createdAt": "2026-10-01T00:00:00Z", "updatedAt": "2026-10-02T00:00:00Z", "labels": page([]any{}, false, ""), "relations": page([]any{}, false, ""), "inverseRelations": page([]any{}, false, ""), "title": "source", "description": "body", "estimate": 2, "parent": nil}
				if n == 2 {
					row["parent"] = map[string]any{"id": id(1)}
					row["inverseRelations"] = page([]any{}, true, "inverse-next")
				}
				rows = append(rows, row)
			}
			data = map[string]any{"issues": page(rows, end < 124, strconv.Itoa(end))}
		case strings.Contains(q.Query, "query FreshIssueConnection("):
			calls++
			data = map[string]any{"issue": map[string]any{"id": id(2), "inverseRelations": page([]any{map[string]any{"id": "dep", "type": "blocks", "issue": map[string]any{"id": id(1), "identifier": "ZII-1", "state": map[string]any{"type": "unstarted"}}}}, false, "")}}
		case strings.Contains(q.Query, "query ListCommentsBatch("):
			calls++
			if !strings.Contains(q.Query, "user { id name }") {
				t.Error("batch export omits source authors")
			}
			batch := map[string]any{}
			for i := 0; i < 5; i++ {
				issueID, ok := q.Variables[fmt.Sprintf("id%d", i)].(string)
				if !ok || (*missingAlias && i == 0) {
					continue
				}
				n := 0
				if q.Variables[fmt.Sprintf("after%d", i)] == "comments-next" {
					n = 1
				}
				body := fmt.Sprintf("body%d", n)
				if issueID == id(1) {
					body = strings.Repeat("中文", 10000) + body
				}
				comment := map[string]any{"id": fmt.Sprintf("%s-c%d", issueID, n), "body": body, "createdAt": "2026-10-01T00:00:00Z", "updatedAt": "2026-10-02T00:00:00Z", "user": map[string]any{"id": "author-id", "name": "来源作者"}}
				batch[fmt.Sprintf("i%d", i)] = map[string]any{"id": issueID, "comments": page([]any{comment}, issueID == id(1) && n == 0, "comments-next")}
			}
			data = batch
		default:
			t.Errorf("unexpected query %s", q.Query)
			http.Error(w, "unknown", http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := linear.New(context.Background(), linear.Options{TeamID: "team", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return client, &calls
}

func TestExportPreservesCompletePaginationAuthorsAndDependencyClosure(t *testing.T) {
	missing := false
	source, calls := exportFixture(t, &missing)
	manifest, err := issues.Export(context.Background(), source, "o/r", "01234567-1234-1234-1234-000000000999")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Issues) != 123 || *calls != 34 {
		t.Fatalf("issues=%d requests=%d, want 123 and 34", len(manifest.Issues), *calls)
	}
	first, second := manifest.Issues[0], manifest.Issues[1]
	if first.Title != "source" || first.Description != "body" || first.Estimate == nil || *first.Estimate != 2 || second.Parent != first.ID || len(second.Blockers) != 1 || second.Blockers[0] != first.ID || len(first.Comments) != 2 || len(first.Comments[0].Body) < 50000 || first.Comments[1].User == nil || first.Comments[1].User.Name != "来源作者" || first.Comments[1].Source != "linear:"+first.ID {
		t.Fatal("export lost source fields, pagination, authors or dependency closure")
	}
}

func TestExportMissingBatchAliasDiscardsManifest(t *testing.T) {
	missing := true
	source, _ := exportFixture(t, &missing)
	manifest, err := issues.Export(context.Background(), source, "o/r", "01234567-1234-1234-1234-000000000999")
	if err == nil || manifest.Complete || len(manifest.Issues) != 0 {
		t.Fatalf("partial source accepted: complete=%v issues=%d err=%v", manifest.Complete, len(manifest.Issues), err)
	}
}

var _ issues.Client = (*linear.Client)(nil)
