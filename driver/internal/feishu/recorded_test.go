package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

// recordedFake serves the recordings keyed by "METHOD path" and checks the
// request body and query against the recording.
func recordedFake(t *testing.T, names ...string) *Client {
	t.Helper()
	routes := map[string]recording{}
	for _, name := range names {
		r := loadRecording(t, name)
		routes[r.Request.Method+" "+r.Request.Path+"?"+wantQuery(t, name)] = r
	}
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		rec, ok := routes[r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery]
		if !ok {
			t.Errorf("unexpected %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if len(rec.Request.Body) > 0 && string(rec.Request.Body) != "null" {
			var got, want any
			if json.NewDecoder(r.Body).Decode(&got) != nil || json.Unmarshal(rec.Request.Body, &want) != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("%s body=%v want %v", r.URL.Path, got, want)
			}
		}
		envelope(t, w, 0, rec.Response.Data)
	}, Options{})
	return c
}

func wantQuery(t *testing.T, name string) string {
	t.Helper()
	switch name {
	case "tickets-list-filter-page1.json":
		return "limit=1&offset=0"
	case "tickets-list-filter-page2.json":
		return "limit=1&offset=1"
	}
	return ""
}

func TestRecordedFieldsAndSchema(t *testing.T) {
	c := recordedFake(t, "fields-list.json")
	fields, err := c.Fields(context.Background(), testBase, ticketTable)
	if err != nil || len(fields) != 17 {
		t.Fatalf("fields=%d err=%v", len(fields), err)
	}
	i := slices.IndexFunc(fields, func(f Field) bool { return f.Name == "状态" })
	if i < 0 || fields[i].ID != "fldKbaSVNc" || fields[i].Type != "select" || fields[i].Multiple || !slices.Equal(fields[i].Options, []string{"Backlog", "Todo", "In Progress", "Done", "Canceled", "Duplicate"}) {
		t.Fatalf("状态=%+v", fields[i])
	}
	spec := TableSpec{Name: "票据表", Fields: []FieldSpec{
		{Name: "标题", Type: "text"},
		{Name: "序号", Type: "auto_number"},
		{Name: "状态", Type: "select", Options: []string{"Todo", "In Progress", "Done"}},
		{Name: "标签", Type: "select", Multiple: true, Options: []string{"ready-for-agent", "auto-impl", "auto-failed"}},
		{Name: "阻塞于", Type: "link"},
		{Name: "估算", Type: "number"},
	}}
	if err := ValidateSchema(spec, fields); err != nil {
		t.Fatalf("valid schema rejected: %v", err)
	}

	spec.Fields = append(spec.Fields,
		FieldSpec{Name: "合并SHA", Type: "text"},
		FieldSpec{Name: "估算", Type: "text"},
		FieldSpec{Name: "标签", Type: "select", Multiple: true, Options: []string{"auto-merged"}},
	)
	err = ValidateSchema(spec, fields)
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) || schemaErr.Table != "票据表" || len(schemaErr.Problems) != 3 {
		t.Fatalf("err=%v", err)
	}
	for _, want := range []string{"合并SHA", "估算", "auto-merged"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestRecordedListPagesCarryRev(t *testing.T) {
	c := recordedFake(t, "tickets-list-filter-page1.json", "tickets-list-filter-page2.json")
	ctx := context.Background()
	first, err := c.ListRecords(ctx, testBase, ticketTable, 0, 1)
	if err != nil || !first.HasMore || first.Rev != 4 || len(first.Records) != 1 {
		t.Fatalf("page1=%+v err=%v", first, err)
	}
	second, err := c.ListRecords(ctx, testBase, ticketTable, 1, 1)
	if err != nil || second.HasMore || second.Rev != first.Rev || len(second.Records) != 1 || second.Records[0].ID == first.Records[0].ID {
		t.Fatalf("page2=%+v err=%v", second, err)
	}
	a := first.Records[0]
	if a.ID != "reczz28NLqDRHkSp" || first.FieldNames["fldKbaSVNc"] != "状态" || first.FieldTypes["fld6s350cd"] != "auto_number" {
		t.Fatalf("record=%+v", a)
	}
	var state []string
	var serial string
	var project []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(a.Fields["fldKbaSVNc"], &state) != nil || !slices.Equal(state, []string{"In Progress"}) ||
		json.Unmarshal(a.Fields["fld6s350cd"], &serial) != nil || serial != "NO.001" ||
		json.Unmarshal(a.Fields["fld53YwCWg"], &project) != nil || len(project) != 1 || project[0].ID != "reczz28NLqBOtI9B" ||
		string(a.Fields["fldzRJUcyu"]) != "null" {
		t.Fatalf("fields=%s", a.Fields)
	}
}

func TestRecordedBatchGet(t *testing.T) {
	c := recordedFake(t, "tickets-batch-get-settled.json")
	page, err := c.BatchGet(context.Background(), testBase, ticketTable, []string{"reczz28NLqDRHkSp", "reczz28NLqDRHJnT"})
	if err != nil || page.Rev != 4 || len(page.Records) != 2 || page.Records[1].ID != "reczz28NLqDRHJnT" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	var blockers []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(page.Records[1].Fields["fldzRJUcyu"], &blockers) != nil || len(blockers) != 1 || blockers[0].ID != "reczz28NLqDRHkSp" {
		t.Fatalf("blockers=%s", page.Records[1].Fields["fldzRJUcyu"])
	}
}

func TestRecordedBatchCreateIgnoredFields(t *testing.T) {
	rec := loadRecording(t, "tickets-create.json")
	var body struct {
		CreateRecords []map[string]any `json:"create_records"`
	}
	if err := json.Unmarshal(rec.Request.Body, &body); err != nil {
		t.Fatal(err)
	}
	c := recordedFake(t, "tickets-create.json")
	res, err := c.BatchCreate(context.Background(), testBase, ticketTable, body.CreateRecords)
	if err != nil || len(res.RecordIDs) != 3 || len(res.IgnoredFields) != 1 || res.IgnoredFields[0].ID != "fld6s350cd" || res.IgnoredFields[0].Name != "序号" || !strings.HasPrefix(res.IgnoredFields[0].Reason, "READONLY") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRecordedBatchUpdate(t *testing.T) {
	rec := loadRecording(t, "tickets-update-claim.json")
	var body struct {
		UpdateRecords map[string]map[string]any `json:"update_records"`
	}
	if err := json.Unmarshal(rec.Request.Body, &body); err != nil {
		t.Fatal(err)
	}
	c := recordedFake(t, "tickets-update-claim.json")
	res, err := c.BatchUpdate(context.Background(), testBase, ticketTable, body.UpdateRecords)
	if err != nil || !slices.Equal(res.RecordIDs, []string{"reczz28NLqDRHJnT", "reczz28NLqDRHkSp"}) || len(res.IgnoredFields) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRecordedMissingScopeNotRetried(t *testing.T) {
	rec := loadRecording(t, "bot-scope-denied.json")
	var calls atomic.Int64
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		violations := []map[string]string{}
		for _, scope := range rec.Response.Error.MissingScopes {
			violations = append(violations, map[string]string{"type": "action_scope_required", "subject": scope})
		}
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"code": rec.Response.Error.Code, "msg": rec.Response.Error.Message,
			"error": map[string]any{"permission_violations": violations},
		}); err != nil {
			t.Error(err)
		}
	}, Options{})
	_, err := c.BatchCreate(context.Background(), testBase, ticketTable, []map[string]any{{"标题": "x"}})
	var apiErr *APIError
	var global *infra.Global
	if !errors.As(err, &apiErr) || apiErr.Code != CodeMissingScope || !slices.Equal(apiErr.MissingScopes, []string{"base:app:create"}) ||
		!errors.As(err, &global) || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}
