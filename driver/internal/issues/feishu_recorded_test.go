package issues

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

const (
	recProjectTable = "tblSKMqG6YfKPNik"
	recTicketTable  = "tblZQsD8lnjliPcn"
	recEventTable   = "tbl7jbXwoOR0WHZq"
	recProject      = "reczz28NLqBOtI9B"
	recTicketA      = "reczz28NLqDRHkSp"
	recTicketB      = "reczz28NLqDRHJnT"
	recBasePath     = "/open-apis/base/v3/bases/BASE_TOKEN"
)

type feishuRecording struct {
	Request struct {
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
		Params struct {
			Filter string `json:"filter"`
		} `json:"params"`
	} `json:"request"`
	Response struct {
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	} `json:"response"`
}

func loadFeishuRecording(t *testing.T, name string) feishuRecording {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/recorded/feishu/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var r feishuRecording
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

type feishuReply struct {
	code int
	data json.RawMessage
}

// recordedFeishuBase serves base/v3 replies keyed by method, path, filter and
// batch_get record IDs. A route's replies are served in order; the last one
// repeats.
type recordedFeishuBase struct {
	t        *testing.T
	mu       sync.Mutex
	routes   map[string][]feishuReply
	requests map[string][]json.RawMessage
}

func routeKey(t *testing.T, method, path, filter string, body []byte) string {
	t.Helper()
	if filter != "" {
		var v any
		if err := json.Unmarshal([]byte(filter), &v); err != nil {
			t.Fatalf("filter %s: %v", filter, err)
		}
		normalized, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		filter = string(normalized)
	}
	var in struct {
		IDs []string `json:"record_id_list"`
	}
	if len(body) > 0 && strings.HasSuffix(path, "/batch_get") {
		if err := json.Unmarshal(body, &in); err != nil {
			t.Fatalf("batch_get body %s: %v", body, err)
		}
	}
	return method + " " + path + " " + filter + " " + strings.Join(in.IDs, ",")
}

func (f *recordedFeishuBase) on(method, path, filter string, body []byte, replies ...feishuReply) {
	key := routeKey(f.t, method, path, filter, body)
	f.routes[key] = append(f.routes[key], replies...)
}

// replay serves each recording at the request it was recorded for.
func (f *recordedFeishuBase) replay(names ...string) {
	for _, name := range names {
		r := loadFeishuRecording(f.t, name)
		f.on(r.Request.Method, r.Request.Path, r.Request.Params.Filter, r.Request.Body, feishuReply{code: r.Response.Error.Code, data: r.Response.Data})
	}
}

func (f *recordedFeishuBase) sent(method, path string) []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[method+" "+path]
}

func (f *recordedFeishuBase) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "t", "expire": 7200})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Error(err)
	}
	f.requests[r.Method+" "+r.URL.Path] = append(f.requests[r.Method+" "+r.URL.Path], body)
	key := routeKey(f.t, r.Method, r.URL.Path, r.URL.Query().Get("filter"), body)
	replies := f.routes[key]
	if len(replies) == 0 {
		f.t.Errorf("unrecorded request %s", key)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	reply := replies[0]
	if len(replies) > 1 {
		f.routes[key] = replies[1:]
	}
	out := map[string]any{"code": reply.code, "msg": "success"}
	if reply.data != nil {
		out["data"] = reply.data
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		f.t.Error(err)
	}
}

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newRecordedFeishuBase replays the app-identity recordings of the test Base.
// Ticket B relates to a record deleted
// before recording, so its batch_get reply is an empty page.
func newRecordedFeishuBase(t *testing.T) *recordedFeishuBase {
	f := &recordedFeishuBase{t: t, routes: map[string][]feishuReply{}, requests: map[string][]json.RawMessage{}}
	f.replay("bot-tables-list.json", "fields-list.json", "bot-fields-list-events.json", "bot-tickets-list-filter.json")
	f.replay("bot-project-fields.json", "bot-project-records.json")
	f.on(http.MethodPost, recBasePath+"/tables/"+recTicketTable+"/records/batch_get", "", []byte(`{"record_id_list":["reczz28NLqDRJWLV"]}`), feishuReply{data: rawJSON(t, map[string]any{
		"fields": []string{}, "field_id_list": []string{}, "field_type_list": []string{}, "record_id_list": []string{}, "data": [][]any{}, "has_more": false, "rev": 15,
	})})
	return f
}

func (f *recordedFeishuBase) backend(t *testing.T) *Feishu {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "cli_test")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "secret")
	client, err := New(context.Background(), map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_BASE": "BASE_TOKEN", "AGENT_FEISHU_PROJECT": feishuRepo}, config.Tuning{})
	if err != nil {
		t.Fatal(err)
	}
	return client.(*Feishu)
}

func TestRecordedFeishuBotSchemaAndTables(t *testing.T) {
	f := newRecordedFeishuBase(t)
	f.replay("tables-list.json")
	c := f.backend(t)
	if c.tables["事件"].id != recEventTable || c.tables["事件"].fields["修改时间"] != "fldeQcyziv" || c.tables["票据"].id != recTicketTable {
		t.Fatalf("tables=%+v", c.tables)
	}
	user, err := c.client.Tables(context.Background(), c.base)
	if err != nil || len(user) != 3 || user[1] != (feishu.Table{ID: recTicketTable, Name: "票据"}) {
		t.Fatalf("user-identity tables=%+v err=%v", user, err)
	}
}

// The 修改时间 field was added to 事件 after its rows existed. The service
// backfilled every row with its real last edit: unedited events of ticket A
// keep 修改时间 = 创建时间, while the edited event of ticket B lists a later
// 修改时间 (bot-events-list-ticket-b.json).
func TestRecordedFeishuBotEventsUpdatedAtBackfilled(t *testing.T) {
	f := newRecordedFeishuBase(t)
	f.replay("bot-events-list.json", "bot-events-list-ticket-b.json")
	const edited = "reczz28NLqZ0kk1a"

	c := f.backend(t)
	ctx := context.Background()
	comments, err := c.ListComments(ctx, feishuUUIDA)
	if err != nil {
		t.Fatalf("backfilled events rejected: %v", err)
	}
	want := []string{"merge-handoff-v1: {\"version\":1,\"queue_id\":\"q-test\"}\n归属: 合并通道", "merge-progress-v1: {\"version\":1,\"phase\":\"merge-intent\"}", "recorded as app identity"}
	if len(comments) != len(want) {
		t.Fatalf("comments=%+v", comments)
	}
	for i, body := range want {
		if comments[i].Body != body || !comments[i].UpdatedAt.Equal(comments[i].CreatedAt) {
			t.Fatalf("comment %d = %+v", i, comments[i])
		}
	}
	if _, err := c.ListComments(ctx, "OF-2"); err == nil || !strings.Contains(err.Error(), "evidence conflict") || !strings.Contains(err.Error(), edited) {
		t.Fatalf("edited event accepted: %v", err)
	}
}

func TestRecordedFeishuMultiTicketEventFilter(t *testing.T) {
	f := newRecordedFeishuBase(t)
	f.replay("events-list-multi-ticket.json")
	c := f.backend(t)
	filter := feishu.Filter{Logic: "and", Conditions: []feishu.Condition{{Field: "票据", Operator: "intersects", Value: []feishu.LinkRef{{ID: recTicketA}, {ID: recTicketB}}}}}
	page, err := c.client.ListRecordsWhere(context.Background(), c.base, recEventTable, filter, 0, 100)
	if err != nil || len(page.Records) != 3 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	owners := map[string]int{}
	for _, r := range page.Records {
		ids, err := c.links("事件", r, "票据")
		if err != nil || len(ids) != 1 {
			t.Fatalf("owners=%v err=%v", ids, err)
		}
		owners[ids[0]]++
	}
	if owners[recTicketA] != 2 || owners[recTicketB] != 1 {
		t.Fatalf("owners=%v", owners)
	}
}

func TestRecordedFeishuAutoNumberNotReused(t *testing.T) {
	f := newRecordedFeishuBase(t)
	f.replay("tickets-get-D.json")
	c := f.backend(t)
	page, err := c.client.BatchGet(context.Background(), c.base, recTicketTable, []string{"reczz28NLrUy5Ztl"})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if string(page.Records[0].Fields["fld6s350cd"]) != `"NO.004"` {
		t.Fatalf("序号 %s is not the recorded string", page.Records[0].Fields["fld6s350cd"])
	}
	row, err := c.row(page.Records[0])
	if err != nil || row.number != 4 || row.state != "Backlog" || row.canonical != "" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

// Advanced permissions on, no restricted role for the app: the app reads every
// row and field (record_scope all_records).
func TestRecordedFeishuAdvancedPermissionUnfiltered(t *testing.T) {
	f := newRecordedFeishuBase(t)
	f.replay("bot-tickets-list-advperm.json")
	c := f.backend(t)
	page, err := c.client.ListRecords(context.Background(), c.base, recTicketTable, 0, 100)
	if err != nil || len(page.Records) != 3 || len(page.FieldNames) != 17 || page.HasMore {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	numbers := []int{}
	for _, r := range page.Records {
		row, err := c.row(r)
		if err != nil {
			t.Fatal(err)
		}
		numbers = append(numbers, row.number)
	}
	if !slices.Equal(numbers, []int{1, 2, 4}) {
		t.Fatalf("numbers=%v", numbers)
	}
}

func TestRecordedFeishuWriteThenReadStale(t *testing.T) {
	f := newRecordedFeishuBase(t)
	f.replay("tickets-batch-get-after-write.json", "tickets-batch-get-settled.json", "concurrent-update-1.json")
	c := f.backend(t)
	ctx := context.Background()
	res, err := c.client.BatchUpdate(ctx, c.base, recTicketTable, map[string]map[string]any{recTicketA: {"估算": 1}})
	if err != nil || !slices.Equal(res.RecordIDs, []string{recTicketA}) {
		t.Fatalf("write=%+v err=%v", res, err)
	}
	read := func() (feishu.Page, feishuTicketRow) {
		page, err := c.client.BatchGet(ctx, c.base, recTicketTable, []string{recTicketA, recTicketB})
		if err != nil || len(page.Records) != 2 {
			t.Fatalf("page=%+v err=%v", page, err)
		}
		row, err := c.row(page.Records[0])
		if err != nil {
			t.Fatal(err)
		}
		return page, row
	}
	stale, before := read()
	settled, after := read()
	if stale.Rev != 3 || before.state != "Todo" || slices.Contains(before.labels, "auto-impl") {
		t.Fatalf("first read rev=%d row=%+v", stale.Rev, before)
	}
	if settled.Rev != 4 || after.state != "In Progress" || !slices.Contains(after.labels, "auto-impl") {
		t.Fatalf("settled read rev=%d row=%+v", settled.Rev, after)
	}
}

func TestRecordedFeishuBotWritesKeyedByFieldID(t *testing.T) {
	f := newRecordedFeishuBase(t)
	f.replay("bot-tickets-update.json", "bot-tickets-batch-get.json", "bot-events-create.json")
	c := f.backend(t)
	ctx := context.Background()
	if err := c.updateTicket(ctx, recTicketA, map[string]any{"估算": 9}); err != nil {
		t.Fatal(err)
	}
	updates := f.sent(http.MethodPost, recBasePath+"/tables/"+recTicketTable+"/records/batch_update")
	if len(updates) != 1 || string(updates[0]) != `{"update_records":{"`+recTicketA+`":{"fld9tF9J2S":9}}}` {
		t.Fatalf("update body %s", updates)
	}
	row, err := c.ticketRow(ctx, recTicketA)
	if err != nil || row.estimate == nil || *row.estimate != 9 {
		t.Fatalf("read back %+v err=%v", row, err)
	}
	comment, err := c.CreateComment(ctx, feishuUUIDA, "recorded as app identity")
	if err != nil || comment.ID != "reczz28NNoYOyt5T" || comment.Body != "recorded as app identity" {
		t.Fatalf("comment=%+v err=%v", comment, err)
	}
	creates := f.sent(http.MethodPost, recBasePath+"/tables/"+recEventTable+"/records/batch_create")
	var body struct {
		Records []map[string]json.RawMessage `json:"create_records"`
	}
	if len(creates) != 1 || json.Unmarshal(creates[0], &body) != nil || len(body.Records) != 1 {
		t.Fatalf("create body %s", creates)
	}
	keys := slices.Sorted(maps.Keys(body.Records[0]))
	if !slices.Equal(keys, []string{"fldd5wlUCT", "fldi56syPw", "fldmppaTNw", "fldutke6EY"}) {
		t.Fatalf("event cells keyed %v", keys)
	}
}

func TestRecordedFeishuIgnoredFieldsRejected(t *testing.T) {
	f := newRecordedFeishuBase(t)
	created := loadFeishuRecording(t, "tickets-create.json")
	f.on(http.MethodPost, recBasePath+"/tables/"+recEventTable+"/records/batch_create", "", nil, feishuReply{data: created.Response.Data})
	c := f.backend(t)
	if _, err := c.CreateComment(context.Background(), feishuUUIDA, "x"); err == nil || !strings.Contains(err.Error(), "ignored fields 序号") {
		t.Fatalf("ignored field accepted: %v", err)
	}
}

// 91403 was observed live (2026-10-15) for an app granted base scopes but not
// added as a Base collaborator. It is not retried and keeps its code.
func TestFeishuNoCollaboratorWriteNotRetried(t *testing.T) {
	f := newRecordedFeishuBase(t)
	path := recBasePath + "/tables/" + recTicketTable + "/records/batch_update"
	f.replay("bot-no-collaborator-write.json")
	c := f.backend(t)
	err := c.updateTicket(context.Background(), recTicketA, map[string]any{"估算": 11})
	var apiErr *feishu.APIError
	var global *infra.Global
	var transient *infra.Transient
	if !errors.As(err, &apiErr) || apiErr.Code != feishu.CodeNoPermission || !errors.As(err, &global) || global.Dep != feishu.Dep || errors.As(err, &transient) {
		t.Fatalf("err=%v", err)
	}
	if calls := len(f.sent(http.MethodPost, path)); calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}
