package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestRecordedInverseRelations(t *testing.T) {
	b, err := os.ReadFile("../../testdata/recorded/linear-inverse-relations.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Data struct{ Issue Issue } `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Data.Issue.Blocked() {
		t.Fatalf("ZII-894 was blocked by an unfinished ZII-893: %+v", resp.Data.Issue.InverseRelations)
	}
}

// Linear rejects the whole issueUpdate when it removes a label the issue does not carry.
func TestRecordedRemoveAbsentLabel(t *testing.T) {
	rejection, err := os.ReadFile("../../testdata/recorded/linear-remove-absent-label.json")
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	c, _ := fixture(t, func(w http.ResponseWriter, q request) {
		if strings.Contains(q.Query, "IssueLabels") {
			fmt.Fprint(w, `{"data":{"issue":{"labels":{"nodes":[{"id":"l2","name":"old"}],"pageInfo":{"hasNextPage":false}}}}}`)
			return
		}
		sent = q.Variables["input"].(map[string]any)
		for _, id := range sent["removedLabelIds"].([]any) {
			if id != "l2" {
				w.Write(rejection)
				return
			}
		}
		fmt.Fprint(w, `{"data":{"issueUpdate":{"success":true,"issue":{"id":"i1","identifier":"ENG-1"}}}}`)
	})
	// "bug" (l1) is absent and must not be removed; "old" (l2) is present and must not be re-added.
	if _, err := c.UpdateIssue(context.Background(), "i1", "", []string{"old"}, []string{"bug", "old"}); err != nil {
		t.Fatalf("update err=%v", err)
	}
	if !reflect.DeepEqual(sent["removedLabelIds"], []any{"l2"}) || !reflect.DeepEqual(sent["addedLabelIds"], []any{}) {
		t.Fatalf("sent=%v", sent)
	}
}
