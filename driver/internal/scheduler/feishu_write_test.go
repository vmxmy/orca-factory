package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
)

const feishuClaimRepo = "github.com/vmxmy/orca-factory"

type feishuClaimField struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Type     string           `json:"type"`
	Multiple bool             `json:"multiple,omitempty"`
	Options  []map[string]any `json:"options,omitempty"`
}

// feishuClaimBase serves one managed, claimable ticket. With settle false a
// ticket write is accepted but never becomes visible to reads.
type feishuClaimBase struct {
	t       *testing.T
	settle  bool
	mu      sync.Mutex
	fields  map[string][]feishuClaimField
	rows    map[string][]map[string]any
	updates int
	events  int
}

func newFeishuClaimBase(t *testing.T, settle bool) *feishuClaimBase {
	raw, err := os.ReadFile("../../testdata/recorded/feishu/fields-list.json")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Response struct {
			Data struct {
				Fields []feishuClaimField `json:"fields"`
			} `json:"data"`
		} `json:"response"`
	}
	if err = json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	brief := issues.FeishuBrief{Todo: "做 A", Check: "make test", Scope: "driver/**", Blockers: []string{}, Related: []string{}}
	created := "2026-10-15T00:09:26.000+08:00"
	return &feishuClaimBase{t: t, settle: settle, fields: map[string][]feishuClaimField{
		"tblP": {{ID: "fldPName", Name: "名称", Type: "text"}, {ID: "fldPRemote", Name: "仓库远端", Type: "text"}, {ID: "fldPPrefix", Name: "编号前缀", Type: "text"}, {ID: "fldPOn", Name: "启用", Type: "checkbox"}},
		"tblT": rec.Response.Data.Fields,
		"tblE": {{ID: "fldESum", Name: "摘要", Type: "text"}, {ID: "fldEBy", Name: "操作者", Type: "text"}, {ID: "fldETicket", Name: "票据", Type: "link"}, {ID: "fldESource", Name: "来源", Type: "select", Options: []map[string]any{{"name": "driver"}, {"name": "resolver"}, {"name": "operator"}}}, {ID: "fldECreated", Name: "创建时间", Type: "created_at"}, {ID: "fldEUpdated", Name: "修改时间", Type: "updated_at"}, {ID: "fldEBody", Name: "正文", Type: "text"}},
	}, rows: map[string][]map[string]any{
		"tblP": {{"_id": "recP1", "名称": "orca", "仓库远端": feishuClaimRepo, "编号前缀": "OF", "启用": true}},
		"tblT": {{"_id": "recA", "标题": "A", "序号": "NO.001", "规范ID": "11111111-2222-4333-8444-555555555555", "项目": []feishu.LinkRef{{ID: "recP1"}}, "状态": []string{"Todo"}, "标签": []string{"ready-for-agent"},
			"要做": brief.Todo, "CHECK": brief.Check, "SCOPE": brief.Scope, "纳管指纹": brief.Fingerprint(), "创建时间": created, "修改时间": created}},
		"tblE": {},
	}}
}

func (f *feishuClaimBase) reply(w http.ResponseWriter, data any) {
	if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success", "data": data}); err != nil {
		f.t.Error(err)
	}
}

func (f *feishuClaimBase) page(w http.ResponseWriter, table string, rows []map[string]any) {
	ids, types, names, recs, cells := []string{}, []string{}, []string{}, []string{}, [][]any{}
	for _, d := range f.fields[table] {
		ids, types, names = append(ids, d.ID), append(types, d.Type), append(names, d.Name)
	}
	for _, row := range rows {
		recs = append(recs, row["_id"].(string))
		line := []any{}
		for _, d := range f.fields[table] {
			line = append(line, row[d.Name])
		}
		cells = append(cells, line)
	}
	f.reply(w, map[string]any{"has_more": false, "rev": 1, "field_id_list": ids, "field_type_list": types, "fields": names, "record_id_list": recs, "data": cells})
}

func (f *feishuClaimBase) name(table, id string) string {
	i := slices.IndexFunc(f.fields[table], func(d feishuClaimField) bool { return d.ID == id })
	if i < 0 {
		f.t.Errorf("unknown field %s", id)
		return ""
	}
	return f.fields[table][i].Name
}

func (f *feishuClaimBase) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "t", "expire": 7200})
		return
	}
	rest, _ := strings.CutPrefix(r.URL.Path, "/open-apis/base/v3/bases/BASE_TOKEN/tables")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	switch {
	case rest == "":
		f.reply(w, map[string]any{"tables": []map[string]string{{"id": "tblP", "name": "项目"}, {"id": "tblT", "name": "票据"}, {"id": "tblE", "name": "事件"}}})
	case len(parts) == 2 && parts[1] == "fields":
		f.reply(w, map[string]any{"fields": f.fields[parts[0]]})
	case len(parts) == 2:
		f.page(w, parts[0], f.rows[parts[0]])
	case parts[2] == "batch_get":
		var in struct {
			IDs []string `json:"record_id_list"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.page(w, parts[0], slices.DeleteFunc(slices.Clone(f.rows[parts[0]]), func(row map[string]any) bool { return !slices.Contains(in.IDs, row["_id"].(string)) }))
	case parts[2] == "batch_update":
		var in struct {
			Records map[string]map[string]json.RawMessage `json:"update_records"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.updates++
		ids := []string{}
		for id, cells := range in.Records {
			ids = append(ids, id)
			if !f.settle {
				continue
			}
			for field, raw := range cells {
				var v []string
				if json.Unmarshal(raw, &v) != nil {
					f.t.Errorf("ticket write %s=%s", field, raw)
				}
				f.rows["tblT"][0][f.name("tblT", field)] = v
			}
		}
		f.reply(w, map[string]any{"record_id_list": ids})
	case parts[2] == "batch_create":
		var in struct {
			Records []map[string]json.RawMessage `json:"create_records"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.events++
		row := map[string]any{"_id": "recE1", "票据": []feishu.LinkRef{{ID: "recA"}}, "创建时间": "2026-10-15T00:10:00.000+08:00", "修改时间": "2026-10-15T00:10:00.000+08:00"}
		var body string
		_ = json.Unmarshal(in.Records[0]["fldEBody"], &body)
		row["正文"] = body
		f.rows["tblE"] = append(f.rows["tblE"], row)
		f.reply(w, map[string]any{"record_id_list": []string{"recE1"}})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}
}

func TestFeishuWriteClaimUnconfirmedDoesNotLaunch(t *testing.T) {
	for _, settle := range []bool{true, false} {
		name := map[bool]string{true: "confirmed", false: "unconfirmed"}[settle]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			base := newFeishuClaimBase(t, settle)
			server := httptest.NewServer(base)
			t.Cleanup(server.Close)
			client, err := feishu.New(feishu.Options{Endpoint: server.URL, AppID: "cli_test", AppSecret: "secret"})
			if err != nil {
				t.Fatal(err)
			}
			clock := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
			store, err := issues.NewFeishu(ctx, client, "BASE_TOKEN", "", feishuClaimRepo, issues.FeishuOptions{
				Now:   func() time.Time { return clock },
				Sleep: func(_ context.Context, d time.Duration) error { clock = clock.Add(d); return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			repo := config.Repository{Path: "/repo", MaxInFlight: 1, StaleHours: 8, Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_PROJECT": feishuClaimRepo}}
			oc := &fakeOrca{automations: receipt(`{"automations":[]}`)}
			r := &Runtime{Clients: map[string]LinearClient{"/repo": store}, Repositories: map[string]config.Repository{"/repo": repo}, Orca: oc, DriverPath: "/driver"}
			s := &Scheduler{Config: config.Config{MaxInFlight: 1, TokenBucket: config.TokenBucket{Capacity: 10, RefillPerSecond: 1}, Tuning: config.Tuning{Scheduler: config.DefaultScheduler()}}, Repositories: []config.Repository{repo}, Backend: r}
			err = s.Tick(ctx)
			if settle {
				if err != nil || base.updates != 1 || base.events != 1 || len(oc.launches) != 1 {
					t.Fatalf("confirmed claim: err=%v updates=%d events=%d launches=%d", err, base.updates, base.events, len(oc.launches))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "ticket claim failed") {
				t.Fatalf("unconfirmed claim error %v", err)
			}
			if base.updates != 1 || base.events != 0 || len(oc.launches) != 0 {
				t.Fatalf("unconfirmed claim dispatched: updates=%d events=%d launches=%d", base.updates, base.events, len(oc.launches))
			}
		})
	}
}
