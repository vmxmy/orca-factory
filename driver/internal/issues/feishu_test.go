package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
)

const (
	feishuRepo  = "github.com/vmxmy/orca-factory"
	feishuUUIDA = "11111111-2222-4333-8444-555555555555"
	feishuUUIDB = "22222222-2222-4333-8444-555555555555"
	feishuUUIDC = "33333333-2222-4333-8444-555555555555"
)

type feishuFieldDef struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Multiple bool     `json:"multiple,omitempty"`
	Options  []string `json:"-"`
}

func (f feishuFieldDef) MarshalJSON() ([]byte, error) {
	type plain feishuFieldDef
	options := []map[string]string{}
	for _, o := range f.Options {
		options = append(options, map[string]string{"name": o})
	}
	return json.Marshal(struct {
		plain
		Options []map[string]string `json:"options,omitempty"`
	}{plain(f), options})
}

type feishuRow map[string]any

// feishuFake serves base/v3 in the recorded column-oriented shape and
// paginates by its own page size, so the backend's limit does not matter.
type feishuFake struct {
	t        *testing.T
	mu       sync.Mutex
	fields   map[string][]feishuFieldDef
	rows     map[string][]feishuRow
	pageSize int
	revs     []int64
	// revQueue, when set for a table, serves one rev per page request of that
	// table before falling back to revs, as a concurrent write bumps it mid-read.
	revQueue map[string][]int64
	filters  []string
	// staleReads serves the rows from before the last write to that many
	// later reads, as base/v3 does right after a write (R2).
	staleReads int
	stale      map[string][]feishuRow
	// unnumberedReads leaves 序号 empty on created tickets for that many
	// later reads, as base/v3 assigns auto numbers asynchronously.
	unnumberedReads int
	afterWrite      func(f *feishuFake)
	writes          []feishuWrite
	batchGets       [][]string
	deletes         [][]string
	created         int
}

// feishuWrite is one batch_create or batch_update request; cells are keyed
// by field ID as sent.
type feishuWrite struct {
	table, action string
	records       map[string]map[string]json.RawMessage
}

func newFeishuFake(t *testing.T) *feishuFake {
	raw, err := os.ReadFile("../../testdata/recorded/feishu/fields-list.json")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Response struct {
			Data struct {
				Fields []struct {
					feishuFieldDef
					Options []struct {
						Name string `json:"name"`
					} `json:"options"`
				} `json:"fields"`
			} `json:"data"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	tickets := []feishuFieldDef{}
	for _, f := range rec.Response.Data.Fields {
		def := f.feishuFieldDef
		for _, o := range f.Options {
			def.Options = append(def.Options, o.Name)
		}
		tickets = append(tickets, def)
	}
	f := &feishuFake{t: t, pageSize: 100, revs: []int64{4}, fields: map[string][]feishuFieldDef{
		"tblP": {{ID: "fldPName", Name: "名称", Type: "text"}, {ID: "fldPRemote", Name: "仓库远端", Type: "text"}, {ID: "fldPPrefix", Name: "编号前缀", Type: "text"}, {ID: "fldPOn", Name: "启用", Type: "checkbox"}},
		"tblT": tickets,
		"tblE": {{ID: "fldmppaTNw", Name: "摘要", Type: "text"}, {ID: "fld6umIs07", Name: "操作者", Type: "text"}, {ID: "fldd5wlUCT", Name: "票据", Type: "link"}, {ID: "fldi56syPw", Name: "来源", Type: "select", Options: []string{"driver", "resolver", "operator"}}, {ID: "fldjvgpvzU", Name: "创建时间", Type: "created_at"}, {ID: "fldEupdate", Name: "修改时间", Type: "updated_at"}, {ID: "fldutke6EY", Name: "正文", Type: "text"}},
	}}
	f.rows = map[string][]feishuRow{
		"tblP": {
			{"_id": "recP1", "名称": "orca", "仓库远端": feishuRepo, "编号前缀": "OF", "启用": true},
			{"_id": "recP2", "名称": "other", "仓库远端": "github.com/x/y", "编号前缀": "XY", "启用": true},
		},
		"tblT": {
			feishuTicket("recA", "NO.001", feishuUUIDA, "recP1", "In Progress", nil, nil),
			feishuTicket("recB", "NO.002", feishuUUIDB, "recP1", "Todo", []string{feishuUUIDA}, []string{feishuUUIDC}),
			feishuTicket("recC", "NO.005", feishuUUIDC, "recP2", "Done", nil, nil),
		},
	}
	f.rows["tblT"][0]["标签"] = []string{"ready-for-agent", "auto-impl"}
	f.rows["tblT"][1]["阻塞于"] = []feishu.LinkRef{{ID: "recA"}}
	f.rows["tblT"][1]["相关"] = []feishu.LinkRef{{ID: "recC"}}
	return f
}

func feishuTicket(rec, no, id, project, state string, blockers, related []string) feishuRow {
	brief := FeishuBrief{Todo: "做 " + rec, Check: "make test", Scope: "driver/**\ncore/", Blockers: blockers, Related: related}
	return feishuRow{
		"_id": rec, "标题": rec + " 票", "序号": no, "规范ID": id, "项目": []feishu.LinkRef{{ID: project}},
		"状态": []string{state}, "标签": []string{"ready-for-agent"}, "要做": brief.Todo, "CHECK": brief.Check, "SCOPE": brief.Scope,
		"正文": "## 现状\n说明", "估算": 3, "纳管指纹": brief.Fingerprint(),
		"创建时间": "2026-10-15T00:09:26.000+08:00", "修改时间": "2026-10-15T00:09:43.000+08:00",
	}
}

func (f *feishuFake) page(w http.ResponseWriter, table string, rows []feishuRow, hasMore bool, rev int64) {
	defs := f.fields[table]
	data := map[string]any{"has_more": hasMore, "rev": rev, "field_id_list": []string{}, "field_type_list": []string{}, "fields": []string{}, "record_id_list": []string{}, "data": [][]any{}}
	for _, d := range defs {
		data["field_id_list"] = append(data["field_id_list"].([]string), d.ID)
		data["field_type_list"] = append(data["field_type_list"].([]string), d.Type)
		data["fields"] = append(data["fields"].([]string), d.Name)
	}
	for _, row := range rows {
		data["record_id_list"] = append(data["record_id_list"].([]string), row["_id"].(string))
		cells := []any{}
		for _, d := range defs {
			cells = append(cells, row[d.Name])
		}
		data["data"] = append(data["data"].([][]any), cells)
	}
	f.reply(w, data)
}

func (f *feishuFake) reply(w http.ResponseWriter, data any) {
	if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success", "data": data}); err != nil {
		f.t.Error(err)
	}
}

func (f *feishuFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "t", "expire": 7200})
		return
	}
	prefix := "/open-apis/base/v3/bases/BASE_TOKEN/tables"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		return
	}
	if rest == "" {
		f.reply(w, map[string]any{"tables": []map[string]string{{"id": "tblP", "name": "项目"}, {"id": "tblT", "name": "票据"}, {"id": "tblE", "name": "事件"}}})
		return
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	table := parts[0]
	switch {
	case len(parts) == 2 && parts[1] == "fields":
		f.reply(w, map[string]any{"fields": f.fields[table]})
	case len(parts) == 3 && parts[2] == "batch_get":
		var in struct {
			IDs []string `json:"record_id_list"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			f.t.Error("batch_get body")
		}
		f.batchGets = append(f.batchGets, in.IDs)
		rows := []feishuRow{}
		for _, id := range in.IDs {
			for _, row := range f.read(table) {
				if row["_id"] == id {
					rows = append(rows, row)
				}
			}
		}
		f.page(w, table, rows, false, 9)
	case len(parts) == 2 && parts[1] == "records":
		f.list(w, r, table)
	case len(parts) == 3 && (parts[2] == "batch_update" || parts[2] == "batch_create"):
		f.write(w, r, table, parts[2])
	case len(parts) == 3 && parts[2] == "batch_delete":
		var in struct {
			IDs []string `json:"record_id_list"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			f.t.Error("batch_delete body")
		}
		f.deletes = append(f.deletes, in.IDs)
		f.rows[table] = slices.DeleteFunc(f.rows[table], func(row feishuRow) bool { return slices.Contains(in.IDs, row["_id"].(string)) })
		f.reply(w, map[string]any{"record_id_list": in.IDs})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}
}

func (f *feishuFake) read(table string) []feishuRow {
	if f.unnumberedReads > 0 {
		f.unnumberedReads--
		if f.unnumberedReads == 0 {
			for _, row := range f.rows["tblT"] {
				if serial, ok := row["_serial"]; ok {
					row["序号"] = serial
				}
			}
		}
	}
	if rows, ok := f.stale[table]; ok && f.staleReads > 0 {
		f.staleReads--
		return rows
	}
	return f.rows[table]
}

func (f *feishuFake) write(w http.ResponseWriter, r *http.Request, table, action string) {
	var in struct {
		Update map[string]map[string]json.RawMessage `json:"update_records"`
		Create []map[string]json.RawMessage          `json:"create_records"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		f.t.Error("write body")
	}
	f.stale = map[string][]feishuRow{}
	for name, rows := range f.rows {
		f.stale[name] = slices.Clone(rows)
	}
	records := in.Update
	ids := []string{}
	if action == "batch_create" {
		records = map[string]map[string]json.RawMessage{}
		for _, cells := range in.Create {
			f.created++
			id := "recNew" + strconv.Itoa(f.created)
			row := feishuRow{"_id": id, "创建时间": "2026-10-15T01:00:00.000+08:00", "修改时间": "2026-10-15T01:00:00.000+08:00"}
			if table == "tblT" {
				row["序号"] = fmt.Sprintf("NO.%03d", 9+f.created)
				if f.unnumberedReads > 0 {
					row["_serial"], row["序号"] = row["序号"], ""
				}
			}
			f.rows[table] = append(f.rows[table], row)
			records[id] = cells
			ids = append(ids, id)
		}
	}
	for id, cells := range records {
		i := slices.IndexFunc(f.rows[table], func(row feishuRow) bool { return row["_id"] == id })
		if i < 0 {
			f.t.Errorf("write to unknown record %s", id)
			continue
		}
		row := maps.Clone(f.rows[table][i])
		for fieldID, raw := range cells {
			def := f.fields[table][slices.IndexFunc(f.fields[table], func(d feishuFieldDef) bool { return d.ID == fieldID })]
			var value any
			switch def.Type {
			case "link":
				value = &[]feishu.LinkRef{}
			case "select":
				value = &[]string{}
			case "number":
				value = new(float64)
			case "checkbox":
				value = new(bool)
			default:
				value = new(string)
			}
			if string(raw) == "null" {
				delete(row, def.Name)
				continue
			}
			if err := json.Unmarshal(raw, value); err != nil {
				f.t.Errorf("field %s value %s: %v", def.Name, raw, err)
			}
			row[def.Name] = reflect.ValueOf(value).Elem().Interface()
		}
		f.rows[table][i] = row
		if action == "batch_update" {
			ids = append(ids, id)
		}
	}
	f.writes = append(f.writes, feishuWrite{table: table, action: action, records: records})
	if f.afterWrite != nil {
		f.afterWrite(f)
	}
	f.reply(w, map[string]any{"record_id_list": ids})
}

func (f *feishuFake) list(w http.ResponseWriter, r *http.Request, table string) {
	rows := f.read(table)
	if raw := r.URL.Query().Get("filter"); raw != "" {
		f.filters = append(f.filters, raw)
		var filter struct {
			Logic      string              `json:"logic"`
			Conditions [][]json.RawMessage `json:"conditions"`
		}
		if json.Unmarshal([]byte(raw), &filter) != nil || filter.Logic != "and" || len(filter.Conditions) != 1 {
			f.t.Errorf("filter %s", raw)
		}
		var field string
		var refs []feishu.LinkRef
		_ = json.Unmarshal(filter.Conditions[0][0], &field)
		_ = json.Unmarshal(filter.Conditions[0][2], &refs)
		all := rows
		rows = nil
		for _, row := range all {
			links, _ := row[field].([]feishu.LinkRef)
			for _, ref := range refs {
				if slices.Contains(links, ref) {
					rows = append(rows, row)
					break
				}
			}
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		_ = json.Unmarshal([]byte(v), &offset)
	}
	end := min(offset+f.pageSize, len(rows))
	if offset > len(rows) {
		offset = len(rows)
	}
	page := offset / f.pageSize
	rev := f.revs[min(page, len(f.revs)-1)]
	if queue := f.revQueue[table]; len(queue) > 0 {
		rev, f.revQueue[table] = queue[0], queue[1:]
	}
	f.page(w, table, rows[offset:end], end < len(rows), rev)
}

func newFeishuTest(t *testing.T, f *feishuFake) *Feishu {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "cli_test")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "secret")
	env := map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_BASE": "https://TENANT.feishu.cn/base/BASE_TOKEN?table=tblT", "AGENT_FEISHU_PROJECT": feishuRepo}
	if backend, err := Backend(env); err != nil || backend != "feishu" || Project(env) != feishuRepo {
		t.Fatalf("feishu env not native: project=%q", Project(env))
	}
	client, err := New(context.Background(), env, config.Tuning{})
	if err != nil {
		t.Fatal(err)
	}
	store, ok := client.(*Feishu)
	if !ok {
		t.Fatalf("backend %T", client)
	}
	return store
}

func TestFeishuReadSnapshotManagedTasksAndDependencies(t *testing.T) {
	f := newFeishuFake(t)
	c := newFeishuTest(t, f)
	ctx := context.Background()
	rows, err := c.Snapshot(ctx, []string{feishuRepo}, time.Time{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("snapshot=%+v err=%v", rows, err)
	}
	if !strings.Contains(f.filters[0], `"项目","intersects",[{"id":"recP1"}]`) {
		t.Fatalf("ticket filter %s", f.filters[0])
	}
	a, err := c.Read(ctx, "OF-1")
	if err != nil || a.ID != feishuUUIDA || a.Number != 0 || a.Issue.Number != 0 || a.State.Name != "In Progress" || a.State.ID != "started" || a.NativeState != "open" || !a.Complete || !a.Metadata.Complete || a.Held || a.Project.ID != feishuRepo {
		t.Fatalf("A=%+v err=%v", a, err)
	}
	if a.URL != "https://TENANT.feishu.cn/base/BASE_TOKEN?table=tblT&record=recA" || a.Estimate == nil || *a.Estimate != 3 || len(a.Labels.Nodes) != 2 || a.Labels.Nodes[1].Name != "auto-impl" {
		t.Fatalf("A fields=%+v", a)
	}
	b, err := c.Read(ctx, feishuUUIDB)
	if err != nil || b.Identifier != "OF-2" || !b.Complete || !b.Blocked() || len(b.BlockedBy) != 1 {
		t.Fatalf("B=%+v err=%v", b, err)
	}
	blocker := b.BlockedBy[0]
	if blocker.Type != "blocks" || blocker.Issue.ID != feishuUUIDA || blocker.Issue.Identifier != "OF-1" || blocker.Issue.State.Type != "started" {
		t.Fatalf("blocker=%+v", blocker)
	}
	if len(b.Relations.Nodes) != 1 || b.Relations.Nodes[0].RelatedIssue.ID != feishuUUIDC || b.Relations.Nodes[0].RelatedIssue.Identifier != "XY-5" {
		t.Fatalf("related=%+v", b.Relations)
	}
	if !slices.Equal(b.Metadata.Blockers, []string{feishuUUIDA}) || !slices.Equal(b.Metadata.Related, []string{feishuUUIDC}) {
		t.Fatalf("metadata=%+v", b.Metadata)
	}
	f.rows["tblT"][0]["状态"] = []string{"Done"}
	b, err = c.Read(ctx, "OF-2")
	if err != nil || b.Blocked() || b.BlockedBy[0].Issue.State.Type != "completed" {
		t.Fatalf("done blocker still blocks: %+v err=%v", b.BlockedBy, err)
	}
	a, _ = c.Read(ctx, "OF-1")
	if a.NativeState != "closed" {
		t.Fatalf("done task native state %q", a.NativeState)
	}
	description, err := c.Description(ctx, "OF-2")
	if err != nil || !strings.Contains(description, "## SCOPE\ndriver/** core/") {
		t.Fatalf("description=%q err=%v", description, err)
	}
}

func TestFeishuReadSnapshotFailsWhenPageRevDiffers(t *testing.T) {
	f := newFeishuFake(t)
	f.pageSize = 1
	f.revs = []int64{4, 5}
	c := newFeishuTest(t, f)
	if rows, err := c.Snapshot(context.Background(), []string{feishuRepo}, time.Time{}); err == nil {
		t.Fatalf("partial snapshot returned: %+v", rows)
	}
	f.revs = []int64{6}
	if rows, err := c.Snapshot(context.Background(), []string{feishuRepo}, time.Time{}); err != nil || len(rows) != 2 {
		t.Fatalf("consistent pages rejected: %v", err)
	}
}

func TestFeishuReadRetriesWhenTableChangesMidRead(t *testing.T) {
	f := newFeishuFake(t)
	f.pageSize = 1
	f.revQueue = map[string][]int64{"tblT": {4, 5, 6, 6}}
	c := newFeishuTest(t, f)
	if rows, err := c.Snapshot(context.Background(), []string{feishuRepo}, time.Time{}); err != nil || len(rows) != 2 {
		t.Fatalf("read after a concurrent write rejected: rows=%d err=%v", len(rows), err)
	}
}

func TestFeishuReadFingerprintMismatchHeld(t *testing.T) {
	f := newFeishuFake(t)
	f.rows["tblT"][1]["SCOPE"] = "driver/** core/ docs/"
	c := newFeishuTest(t, f)
	b, err := c.Read(context.Background(), "OF-2")
	if err != nil || !b.Held || !b.Metadata.ImportHold || !b.Metadata.Complete || b.Complete || !b.Blocked() {
		t.Fatalf("edited brief not held: %+v err=%v", b, err)
	}
	a, _ := c.Read(context.Background(), "OF-1")
	if a.Held || !a.Complete {
		t.Fatalf("unchanged brief held: %+v", a)
	}
}

func TestFeishuReadUnmanagedRowVisibleNotClaimable(t *testing.T) {
	f := newFeishuFake(t)
	f.rows["tblT"] = append(f.rows["tblT"], feishuRow{"_id": "recD", "标题": "D 删除后新建", "序号": "NO.004", "项目": []feishu.LinkRef{{ID: "recP1"}}, "状态": []string{"Todo"}, "标签": []string{"ready-for-agent"}, "创建时间": "2026-10-15T00:10:39.000+08:00", "修改时间": "2026-10-15T00:10:39.000+08:00"})
	c := newFeishuTest(t, f)
	ctx := context.Background()
	rows, err := c.List(ctx)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	d, err := c.Read(ctx, "OF-4")
	if err != nil || d.Metadata.Complete || d.Complete || !d.Blocked() || d.ID == "" || d.Held {
		t.Fatalf("unmanaged row claimable: %+v err=%v", d, err)
	}
	again, _ := c.Read(ctx, "OF-4")
	if again.ID != d.ID {
		t.Fatalf("unmanaged identity changed %s -> %s", d.ID, again.ID)
	}
}

func TestFeishuReadCommentsRejectEditedEvent(t *testing.T) {
	f := newFeishuFake(t)
	event := func(id, created, body string) feishuRow {
		return feishuRow{"_id": id, "摘要": "e", "票据": []feishu.LinkRef{{ID: "recB"}}, "来源": []string{"driver"}, "创建时间": created, "修改时间": created, "正文": body}
	}
	f.rows["tblE"] = []feishuRow{
		event("recE2", "2026-10-15T00:09:47.000+08:00", "merge-progress-v1: {}"),
		event("recE1", "2026-10-15T00:09:46.000+08:00", "merge-handoff-v1: {}"),
		{"_id": "recE3", "票据": []feishu.LinkRef{{ID: "recA"}}, "创建时间": "2026-10-15T00:09:40.000+08:00", "修改时间": "2026-10-15T00:09:40.000+08:00", "正文": "other ticket"},
	}
	c := newFeishuTest(t, f)
	ctx := context.Background()
	comments, err := c.ListComments(ctx, "OF-2")
	if err != nil || len(comments) != 2 || comments[0].ID != "recE1" || comments[1].Body != "merge-progress-v1: {}" {
		t.Fatalf("comments=%+v err=%v", comments, err)
	}
	if !strings.Contains(f.filters[len(f.filters)-1], `"票据","intersects",[{"id":"recB"}]`) {
		t.Fatalf("event filter %s", f.filters[len(f.filters)-1])
	}
	f.rows["tblE"][1]["修改时间"] = "2026-10-15T00:12:00.000+08:00"
	if _, err := c.ListComments(ctx, "OF-2"); err == nil {
		t.Fatal("edited event accepted as evidence")
	}
}

// A snapshot's event reads cost one request per page of matching events,
// however many tickets those events cover (OF-293).
func TestFeishuSnapshotCommentsRequestsFollowPages(t *testing.T) {
	for _, tickets := range []int{2, 40} {
		f := newFeishuFake(t)
		f.pageSize = 2
		for i := 3; i <= tickets; i++ {
			f.rows["tblT"] = append(f.rows["tblT"], feishuTicket(fmt.Sprintf("recX%d", i), fmt.Sprintf("NO.%03d", 100+i), fmt.Sprintf("%08d-2222-4333-8444-555555555555", i), "recP1", "Todo", nil, nil))
		}
		for i := range 5 {
			at := fmt.Sprintf("2026-10-15T00:09:4%d.000+08:00", i)
			f.rows["tblE"] = append(f.rows["tblE"], feishuRow{"_id": fmt.Sprintf("recE%d", i), "票据": []feishu.LinkRef{{ID: "recA"}}, "创建时间": at, "修改时间": at, "正文": fmt.Sprintf("event %d", i)})
		}
		c := newFeishuTest(t, f)
		ctx := context.Background()
		rows, err := c.Snapshot(ctx, []string{feishuRepo}, time.Time{})
		if err != nil || len(rows) != tickets {
			t.Fatalf("rows=%d err=%v", len(rows), err)
		}
		ids := []string{}
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		before := len(f.filters)
		got, err := c.SnapshotComments(ctx, ids)
		if err != nil || len(got) != tickets || len(got[feishuUUIDA]) != 5 || got[feishuUUIDA][4].Body != "event 4" {
			t.Fatalf("comments=%+v err=%v", got, err)
		}
		if reads := len(f.filters) - before; reads != 3 {
			t.Fatalf("%d tickets: %d event requests", tickets, reads)
		}
	}
}

func TestFeishuReadRejectsInvalidSchema(t *testing.T) {
	f := newFeishuFake(t)
	f.fields["tblE"] = f.fields["tblE"][:5]
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "cli_test")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "secret")
	_, err := New(context.Background(), map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_BASE": "BASE_TOKEN", "AGENT_FEISHU_PROJECT": feishuRepo}, config.Tuning{})
	if err == nil || !strings.Contains(err.Error(), "修改时间") {
		t.Fatalf("schema mismatch accepted: %v", err)
	}
}
