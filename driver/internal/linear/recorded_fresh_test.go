package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Replay the sanitized HTTP envelopes without rebuilding aliases/connections.
// The companion provenance files describe the real API capture and redactions.
func recordedReadClient(t *testing.T, name string) (*Client, *int, request) {
	t.Helper()
	read := func(part string) []byte {
		t.Helper()
		b, err := os.ReadFile("../../testdata/recorded/linear-" + name + "-" + part + ".json")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var captured request
	if err := json.Unmarshal(read("request"), &captured); err != nil {
		t.Fatal(err)
	}
	response := read("response")
	c, calls := fixture(t, func(w http.ResponseWriter, q request) {
		if strings.Join(strings.Fields(q.Query), " ") != strings.Join(strings.Fields(captured.Query), " ") || !reflect.DeepEqual(q.Variables, captured.Variables) {
			t.Errorf("request differs from recorded contract: %+v", q)
		}
		_, _ = w.Write(response)
	})
	return c, calls, captured
}

func TestRecordedCommentsBatch(t *testing.T) {
	c, calls, captured := recordedReadClient(t, "batch")
	ids := []string{captured.Variables["id0"].(string), captured.Variables["id1"].(string)}
	before := *calls
	rows, err := c.ListCommentsBatch(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != before+1 || len(rows) != 2 || len(rows[ids[0]]) != 43 || len(rows[ids[1]]) != 0 {
		t.Fatalf("recorded alias histories: calls=%d rows=%v", *calls-before, rows)
	}
	first := rows[ids[0]][0]
	if first.ID != "00000000-0000-4000-8000-000000000002" || first.Body != "[redacted recorded comment]" || first.CreatedAt.Format("2006-01-02T15:04:05.000Z") != "2026-10-06T04:16:09.988Z" || first.UpdatedAt.Format("2006-01-02T15:04:05.000Z") != "2026-10-06T04:16:09.965Z" {
		t.Fatalf("recorded comment fields lost: %+v", first)
	}
}

func TestRecordedFreshIssue(t *testing.T) {
	c, calls, captured := recordedReadClient(t, "fresh")
	before := *calls
	issue, err := c.FreshIssue(context.Background(), captured.Variables["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if *calls != before+1 || issue.Identifier != "TEST-1" || issue.State.Name != "Backlog" || issue.Project.ID != "00000000-0000-4000-8000-000000000046" {
		t.Fatalf("recorded fresh issue: calls=%d issue=%+v", *calls-before, issue)
	}
	if len(issue.Labels.Nodes) != 0 || len(issue.Relations.Nodes) != 0 || len(issue.InverseRelations.Nodes) != 0 || issue.Labels.PageInfo.HasNextPage || issue.Relations.PageInfo.HasNextPage || issue.InverseRelations.PageInfo.HasNextPage || issue.Blocked() {
		t.Fatalf("recorded explicit empty final connections: %+v", issue)
	}
}
