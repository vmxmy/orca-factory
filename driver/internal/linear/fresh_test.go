package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestFreshIssueCompletesSchedulingConnections(t *testing.T) {
	for _, mode := range []string{"complete", "null-label", "null-blocker", "missing-blocker-type", "missing-page", "loop", "later-failure"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := fixture(t, func(w http.ResponseWriter, q request) {
				connection := func(nodes any, next bool, cursor string) map[string]any {
					return map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}}
				}
				if !strings.Contains(q.Query, "FreshIssueConnection") {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{"id": "i1", "identifier": "ENG-1", "project": map[string]any{"id": "p1"}, "state": map[string]any{"id": "todo", "name": "Todo"}, "labels": connection([]any{}, true, "labels-next"), "relations": connection([]any{}, true, "relations-next"), "inverseRelations": connection([]any{}, true, "inverseRelations-next")}}})
					return
				}
				field := "labels"
				nodes := []any{map[string]any{"id": "ready", "name": "ready-for-agent"}}
				if strings.Contains(q.Query, " inverseRelations(") {
					field = "inverseRelations"
					nodes = []any{map[string]any{"id": "r2", "type": "blocks", "issue": map[string]any{"id": "i2", "identifier": "ENG-2", "state": map[string]any{"type": "started"}}}}
				} else if strings.Contains(q.Query, " relations(") {
					field = "relations"
					nodes = []any{map[string]any{"id": "r1", "type": "related", "relatedIssue": map[string]any{"id": "i3", "identifier": "ENG-3"}}}
				}
				if q.Variables["after"] != field+"-next" {
					t.Errorf("cursor crossed fields: %v", q.Variables)
				}
				switch mode {
				case "null-label":
					if field == "labels" {
						nodes = []any{nil}
					}
				case "null-blocker":
					if field == "inverseRelations" {
						nodes = []any{nil}
					}
				case "missing-blocker-type":
					if field == "inverseRelations" {
						delete(nodes[0].(map[string]any), "type")
					}
				case "later-failure":
					http.Error(w, "unavailable", 500)
					return
				}
				page := connection(nodes, mode == "loop", field+"-next")
				if mode == "missing-page" {
					delete(page, "pageInfo")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{"id": "i1", field: page}}})
			})
			issue, err := c.FreshIssue(context.Background(), "i1")
			if mode != "complete" {
				if err == nil {
					t.Fatalf("incomplete fresh issue admitted: %+v", issue)
				}
				return
			}
			if err != nil || len(issue.Labels.Nodes) != 1 || len(issue.Relations.Nodes) != 1 || len(issue.InverseRelations.Nodes) != 1 || !issue.Blocked() || issue.InverseRelations.PageInfo.HasNextPage {
				t.Fatalf("issue=%+v err=%v", issue, err)
			}
		})
	}
}

func TestCommentsBatchDeduplicatesWithoutCachingAndMapsReorderedAliases(t *testing.T) {
	c, calls := fixture(t, func(w http.ResponseWriter, q request) {
		if len(q.Variables) != 4 || q.Variables["id0"] != "a" || q.Variables["id1"] != "b" {
			t.Errorf("duplicate ids or unexpected variables: %v", q.Variables)
		}
		_, _ = fmt.Fprint(w, `{"data":{"i1":{"id":"b","comments":{"nodes":[{"id":"cb","body":"B","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}],"pageInfo":{"hasNextPage":false}}},"i0":{"id":"a","comments":{"nodes":[{"id":"ca","body":"A","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}],"pageInfo":{"hasNextPage":false}}}}}`)
	})
	before := *calls
	empty, err := c.ListCommentsBatch(context.Background(), nil)
	if err != nil || len(empty) != 0 || *calls != before {
		t.Fatalf("empty read sent HTTP: rows=%v err=%v", empty, err)
	}
	for round := 1; round <= 2; round++ {
		rows, err := c.ListCommentsBatch(context.Background(), []string{"a", "b", "a"})
		if err != nil || len(rows["a"]) != 1 || len(rows["b"]) != 1 || rows["a"][0].Body != "A" || rows["b"][0].Body != "B" || *calls != before+round {
			t.Fatalf("rows=%v calls=%d err=%v", rows, *calls, err)
		}
	}
}
