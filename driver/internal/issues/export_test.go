package issues_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestExportPreservesCompletePaginationAuthorsAndDependencyClosure(t *testing.T) {
	// #given
	t.Setenv("LINEAR_API_KEY", "test")
	ids := []string{"01234567-1234-1234-1234-012345678901", "01234567-1234-1234-1234-012345678902"}
	project := "01234567-1234-1234-1234-012345678903"
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		id, _ := q.Variables["id"].(string)
		row := map[string]any{"id": id, "identifier": "ZII-1", "project": map[string]any{"id": project}, "state": map[string]any{"id": "todo", "name": "Todo"}, "createdAt": "2026-10-01T00:00:00Z", "updatedAt": "2026-10-02T00:00:00Z", "labels": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}, "relations": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}, "inverseRelations": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}
		if id == ids[1] {
			row["identifier"] = "ZII-2"
			row["inverseRelations"] = map[string]any{"nodes": []any{map[string]any{"id": "dep", "type": "blocks", "issue": map[string]any{"id": ids[0], "identifier": "ZII-1", "state": map[string]any{"type": "unstarted"}}}}, "pageInfo": map[string]any{"hasNextPage": false}}
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
			data = map[string]any{"team": map[string]any{field: map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}
		case strings.Contains(q.Query, "query Snapshot("):
			n := 0
			if q.Variables["after"] == "next" {
				n = 1
			}
			row["id"] = ids[n]
			row["identifier"] = fmt.Sprintf("ZII-%d", n+1)
			data = map[string]any{"issues": map[string]any{"nodes": []any{row}, "pageInfo": map[string]any{"hasNextPage": n == 0, "endCursor": "next"}}}
		case strings.Contains(q.Query, "query FreshIssue("):
			data = map[string]any{"issue": row}
		case strings.Contains(q.Query, "query ExportIssue("):
			row["title"] = "source"
			row["description"] = "body"
			data = map[string]any{"issue": row}
		case strings.Contains(q.Query, "query ListComments("):
			pages++
			n := 0
			if q.Variables["after"] == "comments-next" {
				n = 1
			}
			data = map[string]any{"issue": map[string]any{"comments": map[string]any{"nodes": []any{map[string]any{"id": fmt.Sprintf("%s-c%d", id, n), "body": fmt.Sprintf("body%d", n), "createdAt": "2026-10-01T00:00:00Z", "updatedAt": "2026-10-02T00:00:00Z", "user": map[string]any{"id": "author-id", "name": "来源作者"}}}, "pageInfo": map[string]any{"hasNextPage": n == 0, "endCursor": "comments-next"}}}}
		default:
			t.Errorf("unexpected query %s", q.Query)
			http.Error(w, "unknown", 500)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	source, err := linear.New(context.Background(), linear.Options{TeamID: "team", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	// #when
	manifest, err := issues.Export(context.Background(), source, "o/r", project)
	// #then
	if err != nil || len(manifest.Issues) != 2 || pages != 4 || len(manifest.Issues[1].Blockers) != 1 || manifest.Issues[1].Blockers[0] != ids[0] || len(manifest.Issues[0].Comments) != 2 || (manifest.Issues[0].Comments[1].User == nil || manifest.Issues[0].Comments[1].User.Name != "来源作者") || manifest.Issues[0].Comments[1].Source != "linear:"+ids[0] {
		t.Fatalf("export lost pagination, authors or closure: %+v err=%v pages=%d", manifest, err, pages)
	}
}

var _ issues.Client = (*linear.Client)(nil)
