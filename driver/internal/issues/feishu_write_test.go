package issues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

// fastReadBack replaces the read-back clock so the 60 second default elapses
// without waiting.
func fastReadBack(c *Feishu) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
}

func unmanagedFeishuRow() feishuRow {
	return feishuRow{"_id": "recD", "标题": "D 待纳管", "序号": "NO.004", "项目": []feishu.LinkRef{{ID: "recP1"}}, "状态": []string{"Todo"}, "标签": []string{"ready-for-agent"},
		"要做": "做 D", "CHECK": "make test", "SCOPE": "driver/**", "正文": "## 现状\nD", "创建时间": "2026-10-15T00:10:39.000+08:00", "修改时间": "2026-10-15T00:10:39.000+08:00"}
}

func feishuCell[T any](t *testing.T, w feishuWrite, record, field string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.records[record][field], &v); err != nil {
		t.Fatalf("write %s.%s = %s: %v", record, field, w.records[record][field], err)
	}
	return v
}

func (f *feishuFake) eventWrites() []feishuWrite {
	var out []feishuWrite
	for _, w := range f.writes {
		if w.table == "tblE" {
			out = append(out, w)
		}
	}
	return out
}

func TestFeishuWriteUpdateIssueReadsBackStateAndLabels(t *testing.T) {
	f := newFeishuFake(t)
	f.staleReads = 1
	c := newFeishuTest(t, f)
	fastReadBack(c)
	ctx := context.Background()
	issue, err := c.UpdateIssue(ctx, "OF-2", "In Progress", []string{"auto-impl"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	labels := []string{}
	for _, l := range issue.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	if issue.State.Name != "In Progress" || !slices.Equal(labels, []string{"auto-impl", "ready-for-agent"}) {
		t.Fatalf("issue state=%q labels=%v", issue.State.Name, labels)
	}
	if len(f.writes) != 1 || f.writes[0].action != "batch_update" || len(f.writes[0].records["recB"]) != 2 {
		t.Fatalf("writes=%+v", f.writes)
	}
	if got := feishuCell[[]string](t, f.writes[0], "recB", "fldKbaSVNc"); !slices.Equal(got, []string{"In Progress"}) {
		t.Fatalf("状态 written %v", got)
	}
	if n := len(slices.DeleteFunc(slices.Clone(f.batchGets), func(ids []string) bool { return !slices.Equal(ids, []string{"recB"}) })); n != 3 {
		t.Fatalf("want current-row read, stale read-back and settled read-back; batch_get calls %v", f.batchGets)
	}
	read, err := c.Read(ctx, "OF-2")
	if err != nil || read.State.Name != "In Progress" || len(read.Labels.Nodes) != 2 {
		t.Fatalf("read back %+v err=%v", read.Issue, err)
	}
	if _, err = c.UpdateIssue(ctx, "OF-2", "", nil, []string{"ready-for-agent"}); err != nil {
		t.Fatal(err)
	}
	last := f.writes[len(f.writes)-1]
	if _, ok := last.records["recB"]["fldKbaSVNc"]; ok || len(last.records["recB"]) != 1 {
		t.Fatalf("label-only update wrote %v", last.records["recB"])
	}
	if got := feishuCell[[]string](t, last, "recB", "fld4uIyrWj"); !slices.Equal(got, []string{"auto-impl"}) {
		t.Fatalf("labels written %v", got)
	}
}

func TestFeishuWriteReadBackTimeoutUnconfirmed(t *testing.T) {
	f := newFeishuFake(t)
	f.staleReads = 1 << 20
	c := newFeishuTest(t, f)
	fastReadBack(c)
	_, err := c.UpdateIssue(context.Background(), "OF-2", "In Progress", []string{"auto-impl"}, nil)
	if !errors.Is(err, infra.ErrWriteUnconfirmed) || errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("stale read-back error %v", err)
	}
	if len(f.eventWrites()) != 0 {
		t.Fatalf("timeout wrote events %+v", f.eventWrites())
	}
}

func TestFeishuWriteReadBackOverwrittenIsConflict(t *testing.T) {
	f := newFeishuFake(t)
	f.afterWrite = func(f *feishuFake) {
		if f.writes[len(f.writes)-1].table == "tblT" {
			f.rows["tblT"][1]["标签"] = []string{"ready-for-agent", "auto-failed"}
		}
	}
	c := newFeishuTest(t, f)
	fastReadBack(c)
	_, err := c.UpdateIssue(context.Background(), "OF-2", "In Progress", []string{"auto-impl"}, nil)
	if !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("overwritten read-back error %v", err)
	}
	events := f.eventWrites()
	if len(events) != 1 {
		t.Fatalf("conflict events %+v", events)
	}
	var cells map[string]json.RawMessage
	for _, v := range events[0].records {
		cells = v
	}
	var body string
	var owner []feishu.LinkRef
	if json.Unmarshal(cells["fldutke6EY"], &body) != nil || json.Unmarshal(cells["fldd5wlUCT"], &owner) != nil {
		t.Fatalf("conflict event cells %v", cells)
	}
	if !strings.HasPrefix(body, "转人: ") || !strings.Contains(body, "auto-failed") || !strings.Contains(body, "\n归属: 人") || !slices.Equal(owner, []feishu.LinkRef{{ID: "recB"}}) {
		t.Fatalf("conflict event body=%q owner=%v", body, owner)
	}
}

func TestFeishuWriteUnmanagedRowRejectsUpdate(t *testing.T) {
	f := newFeishuFake(t)
	f.rows["tblT"] = append(f.rows["tblT"], unmanagedFeishuRow())
	c := newFeishuTest(t, f)
	ctx := context.Background()
	if _, err := c.UpdateIssue(ctx, "OF-4", "In Progress", []string{"auto-impl"}, nil); err == nil || !strings.Contains(err.Error(), "adoption required") {
		t.Fatalf("unmanaged update: %v", err)
	}
	if _, err := c.Edit(ctx, "OF-4", []byte(`{"title":"x"}`)); err == nil || !strings.Contains(err.Error(), "adoption required") {
		t.Fatalf("unmanaged edit: %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("unmanaged row written: %+v", f.writes)
	}
}

func TestFeishuWriteCommentRejectsOversizedBody(t *testing.T) {
	f := newFeishuFake(t)
	c := newFeishuTest(t, f)
	if _, err := c.CreateComment(context.Background(), "OF-2", strings.Repeat("证", feishuBodyRunes+1)); err == nil {
		t.Fatal("oversized body accepted")
	}
	if events := f.eventWrites(); len(events) != 0 {
		t.Fatalf("oversized body written: %+v", events)
	}
}

func TestFeishuWriteCommentBodyByteForByte(t *testing.T) {
	f := newFeishuFake(t)
	c := newFeishuTest(t, f)
	ctx := context.Background()
	body := "merge-progress-v1: {\"phase\":\"merged\"}\r\n  缩进 \t尾随空白  \n\n<!-- 标记 -->\n"
	comment, err := c.CreateComment(ctx, "OF-2", body)
	if err != nil {
		t.Fatal(err)
	}
	events := f.eventWrites()
	if len(events) != 1 || events[0].action != "batch_create" {
		t.Fatalf("event writes %+v", events)
	}
	id := comment.ID
	if got := feishuCell[string](t, events[0], id, "fldutke6EY"); got != body {
		t.Fatalf("正文 written %q", got)
	}
	if got := feishuCell[string](t, events[0], id, "fldmppaTNw"); got != `merge-progress-v1: {"phase":"merged"}` {
		t.Fatalf("摘要 written %q", got)
	}
	if got := feishuCell[[]string](t, events[0], id, "fldi56syPw"); !slices.Equal(got, []string{"driver"}) {
		t.Fatalf("来源 written %v", got)
	}
	if got := feishuCell[[]feishu.LinkRef](t, events[0], id, "fldd5wlUCT"); !slices.Equal(got, []feishu.LinkRef{{ID: "recB"}}) {
		t.Fatalf("票据 written %v", got)
	}
	if comment.Body != body || comment.CreatedAt.IsZero() || !comment.UpdatedAt.Equal(comment.CreatedAt) {
		t.Fatalf("comment %+v", comment)
	}
	history, err := c.ListComments(ctx, "OF-2")
	if err != nil || len(history) != 1 || history[0].ID != id || history[0].Body != body {
		t.Fatalf("history %+v err=%v", history, err)
	}
}

func TestFeishuWriteAdoptFingerprintMatchesBrief(t *testing.T) {
	f := newFeishuFake(t)
	f.rows["tblT"] = append(f.rows["tblT"], unmanagedFeishuRow())
	f.staleReads = 2
	c := newFeishuTest(t, f)
	fastReadBack(c)
	ctx := context.Background()
	task, err := c.Adopt(ctx, "OF-4", []byte(`{"blockers":["`+feishuUUIDA+`"],"related":["`+feishuUUIDC+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 1 {
		t.Fatalf("writes %+v", f.writes)
	}
	w := f.writes[0]
	want := FeishuBrief{Todo: "做 D", Check: "make test", Scope: "driver/**", Body: "## 现状\nD", Blockers: []string{feishuUUIDA}, Related: []string{feishuUUIDC}}.Fingerprint()
	if got := feishuCell[string](t, w, "recD", "fldVPgAPid"); got != want {
		t.Fatalf("纳管指纹 %s, want %s", got, want)
	}
	canonical := feishuCell[string](t, w, "recD", "fldrlw1tU9")
	if !uuid.MatchString(canonical) || task.ID != canonical {
		t.Fatalf("规范ID %q task %q", canonical, task.ID)
	}
	if got := feishuCell[[]feishu.LinkRef](t, w, "recD", "fldzRJUcyu"); !slices.Equal(got, []feishu.LinkRef{{ID: "recA"}}) {
		t.Fatalf("阻塞于 %v", got)
	}
	if got := feishuCell[[]feishu.LinkRef](t, w, "recD", "fldWwMkQEK"); !slices.Equal(got, []feishu.LinkRef{{ID: "recC"}}) {
		t.Fatalf("相关 %v", got)
	}
	for field := range w.records["recD"] {
		if !slices.Contains([]string{"fldVPgAPid", "fldrlw1tU9", "fldzRJUcyu", "fldWwMkQEK"}, field) {
			t.Fatalf("adopt wrote field %s", field)
		}
	}
	if !task.Metadata.Complete || task.Held || task.Identifier != "OF-4" || !slices.Equal(task.Metadata.Blockers, []string{feishuUUIDA}) {
		t.Fatalf("adopted task %+v", task.Metadata)
	}
	if _, err = c.Adopt(ctx, "OF-4", []byte(`{"blockers":[],"related":[]}`)); err == nil || !strings.Contains(err.Error(), "already managed") {
		t.Fatalf("re-adopt of unheld task: %v", err)
	}
	if _, err = c.Adopt(ctx, "OF-2", []byte(`{"blockers":[]}`)); err == nil {
		t.Fatal("adopt accepted incomplete dependency lists")
	}
}

func TestFeishuWriteAdoptRequiresBriefAndManagedDependencies(t *testing.T) {
	f := newFeishuFake(t)
	row := unmanagedFeishuRow()
	delete(row, "CHECK")
	f.rows["tblT"] = append(f.rows["tblT"], row)
	c := newFeishuTest(t, f)
	ctx := context.Background()
	if _, err := c.Adopt(ctx, "OF-4", []byte(`{"blockers":[],"related":[]}`)); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("adopt without CHECK: %v", err)
	}
	f.rows["tblT"][3]["CHECK"] = "make test"
	if _, err := c.Adopt(ctx, "OF-4", []byte(`{"blockers":["44444444-2222-4333-8444-555555555555"],"related":[]}`)); err == nil {
		t.Fatal("adopt accepted unknown dependency")
	}
	if len(f.writes) != 0 {
		t.Fatalf("rejected adopt wrote %+v", f.writes)
	}
}

func TestFeishuWriteHeldTaskReadopts(t *testing.T) {
	f := newFeishuFake(t)
	f.rows["tblT"][1]["SCOPE"] = "driver/** core/ docs/"
	c := newFeishuTest(t, f)
	fastReadBack(c)
	ctx := context.Background()
	if _, err := c.Edit(ctx, "OF-2", []byte(`{"title":"x"}`)); err == nil || !strings.Contains(err.Error(), "adopt") {
		t.Fatalf("held edit: %v", err)
	}
	task, err := c.Adopt(ctx, "OF-2", []byte(`{"blockers":["`+feishuUUIDA+`"],"related":["`+feishuUUIDC+`"]}`))
	if err != nil || task.Held || task.ID != feishuUUIDB {
		t.Fatalf("re-adopt %+v err=%v", task.Metadata, err)
	}
	if _, ok := f.writes[0].records["recB"]["fldrlw1tU9"]; ok {
		t.Fatal("re-adopt rewrote 规范ID")
	}
}

func TestFeishuWriteCreateStructuredAndAdopted(t *testing.T) {
	f := newFeishuFake(t)
	f.staleReads = 2
	c := newFeishuTest(t, f)
	fastReadBack(c)
	ctx := context.Background()
	estimate := 5.0
	brief := FeishuBrief{Todo: "1. 写\n2. 读", Check: "cd driver && go test ./...", Scope: "driver/internal/issues\ndriver/internal/feishu", Body: "## 现状\n说明", Blockers: []string{feishuUUIDA}, Related: []string{}}
	task, err := c.Create(ctx, CreateInput{Title: "新票", Description: brief.Description(), State: "Todo", Labels: []string{"ready-for-agent"}, Estimate: &estimate, Blockers: brief.Blockers, Related: brief.Related})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 1 || f.writes[0].action != "batch_create" {
		t.Fatalf("writes %+v", f.writes)
	}
	w := f.writes[0]
	record := "recNew1"
	for field, want := range map[string]string{"fld0NXHDCU": brief.Todo, "fldc0s7zHF": brief.Check, "fldEydnY5L": brief.Scope, "fld5HgAe9Q": brief.Body, "fldQDpx79I": "新票", "fldVPgAPid": brief.Fingerprint()} {
		if got := feishuCell[string](t, w, record, field); got != want {
			t.Fatalf("field %s = %q, want %q", field, got, want)
		}
	}
	if got := feishuCell[[]feishu.LinkRef](t, w, record, "fldzRJUcyu"); !slices.Equal(got, []feishu.LinkRef{{ID: "recA"}}) {
		t.Fatalf("阻塞于 %v", got)
	}
	if task.Identifier != "OF-10" || task.ID != feishuCell[string](t, w, record, "fldrlw1tU9") || !task.Metadata.Complete || task.Held || task.Description != brief.Description() || *task.Estimate != 5 {
		t.Fatalf("created task %+v", task)
	}
	plain, err := c.Create(ctx, CreateInput{Title: "未纳管", Description: "随便写"})
	if err != nil || plain.Metadata.Complete || !uuid.MatchString(plain.ID) {
		t.Fatalf("unmanaged create %+v err=%v", plain.Metadata, err)
	}
	if _, ok := f.writes[1].records["recNew2"]["fldVPgAPid"]; ok {
		t.Fatal("unmanaged create wrote 纳管指纹")
	}
	if _, err = c.Create(ctx, CreateInput{Title: "x", State: "In Progress"}); err == nil {
		t.Fatal("unmanaged create accepted a non-Todo state")
	}
}

func TestFeishuWriteBriefDescriptionRoundTrip(t *testing.T) {
	brief := FeishuBrief{Todo: "做 A\n第二步", Check: "cd driver && go test ./... -run 'TestX'", Scope: "driver/internal/issues\ndriver/internal/feishu", Body: "## 现状\n说明\n\n## 背景\n更多", Blockers: []string{feishuUUIDA}, Related: []string{feishuUUIDC}}
	description := brief.Description()
	parsed, deps, err := parseFeishuDescription(description)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Blockers, parsed.Related = brief.Blockers, brief.Related
	if parsed.Todo != brief.Todo || parsed.Check != brief.Check || parsed.Scope != brief.Scope || parsed.Body != brief.Body || parsed.Description() != description || parsed.Fingerprint() != brief.Fingerprint() {
		t.Fatalf("round trip %+v", parsed)
	}
	if deps == nil || !slices.Equal(deps.Blockers, brief.Blockers) || !slices.Equal(deps.Related, brief.Related) {
		t.Fatalf("declared dependencies %+v", deps)
	}
	intake := "## 现状\n现在\n\n## 要做\n1. 做\n\n## 验收\nCHECK: make test\n断言: 能读回\n\n## SCOPE\ndriver/a driver/b\n\n## 依赖\nDEPENDENCIES: {\"blockers\":[],\"related\":[]}\n说明依赖\n\n## intake 自检\n- 是\n"
	parsed, deps, err = parseFeishuDescription(intake)
	if err != nil || parsed.Todo != "1. 做" || parsed.Check != "make test" || parsed.Scope != "driver/a\ndriver/b" || deps == nil || len(deps.Blockers) != 0 {
		t.Fatalf("intake parsed %+v deps=%+v err=%v", parsed, deps, err)
	}
	if parsed.Body != "## 现状\n现在\n\n## 验收\n断言: 能读回\n\n## 依赖\n说明依赖\n\n## intake 自检\n- 是" {
		t.Fatalf("intake body %q", parsed.Body)
	}
}

func TestFeishuWriteEditRefreshesFingerprint(t *testing.T) {
	f := newFeishuFake(t)
	c := newFeishuTest(t, f)
	fastReadBack(c)
	ctx := context.Background()
	task, err := c.Edit(ctx, "OF-2", []byte(`{"title":"新标题","estimate":8,"related":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "新标题" || *task.Estimate != 8 || task.Held || len(task.Metadata.Related) != 0 || !task.Metadata.Complete {
		t.Fatalf("edited %+v", task)
	}
	w := f.writes[0]
	for field := range w.records["recB"] {
		if !slices.Contains([]string{"fldQDpx79I", "fld9tF9J2S", "fldWwMkQEK", "fldVPgAPid"}, field) {
			t.Fatalf("edit wrote unsupplied field %s", field)
		}
	}
	if _, err = c.Edit(ctx, "OF-2", []byte(`{"priority":2}`)); err == nil {
		t.Fatal("edit accepted a field the Base does not store")
	}
}

func TestFeishuWriteSetMetadataState(t *testing.T) {
	f := newFeishuFake(t)
	c := newFeishuTest(t, f)
	fastReadBack(c)
	ctx := context.Background()
	b, err := c.Read(ctx, "OF-2")
	if err != nil {
		t.Fatal(err)
	}
	m := b.Metadata
	m.State = "Done"
	got, err := c.SetMetadata(ctx, "OF-2", m)
	if err != nil || got.State.Name != "Done" {
		t.Fatalf("set metadata %+v err=%v", got.Metadata, err)
	}
	m.ID = feishuUUIDC
	if _, err = c.SetMetadata(ctx, "OF-2", m); err == nil {
		t.Fatal("canonical identity changed")
	}
}

func TestFeishuBriefFingerprintGolden(t *testing.T) {
	brief := FeishuBrief{Todo: " 做 A\r\n第二步 ", Check: "make test", Scope: "driver/**\ncore/", Body: "不参与", Blockers: []string{feishuUUIDB, feishuUUIDA}}
	if got := brief.Fingerprint(); got != "sha256:babada5b6672005f1be8b8607f5e52e2837695fd54ee48c3ba9a094b874235bf" {
		t.Fatalf("fingerprint format changed: %s", got)
	}
}

func TestFeishuReadDependenciesBatchGetChunked(t *testing.T) {
	f := newFeishuFake(t)
	related := []string{}
	links := []feishu.LinkRef{}
	for i := range 201 {
		id := strings.Replace(feishuUUIDC, "33333333", fmt.Sprintf("%08d", 40000000+i), 1)
		rec := fmt.Sprintf("recX%03d", i)
		f.rows["tblT"] = append(f.rows["tblT"], feishuTicket(rec, fmt.Sprintf("NO.%03d", 100+i), id, "recP2", "Todo", nil, nil))
		related = append(related, id)
		links = append(links, feishu.LinkRef{ID: rec})
	}
	f.rows["tblT"][1]["相关"] = links
	f.rows["tblT"][1]["纳管指纹"] = FeishuBrief{Todo: "做 recB", Check: "make test", Scope: "driver/**\ncore/", Blockers: []string{feishuUUIDA}, Related: related}.Fingerprint()
	c := newFeishuTest(t, f)
	b, err := c.Read(context.Background(), "OF-2")
	if err != nil || b.Held || len(b.Metadata.Related) != 201 {
		t.Fatalf("B=%+v err=%v", b.Metadata, err)
	}
	if len(f.batchGets) != 2 || len(f.batchGets[0]) != 200 || len(f.batchGets[1]) != 1 {
		t.Fatalf("batch_get sizes %d", len(f.batchGets))
	}
}

// A created row has no auto number until base/v3 assigns one: discovery
// skips it, and create keeps reading back until the number appears.
func TestFeishuWriteCreateWaitsForAutoNumber(t *testing.T) {
	f := newFeishuFake(t)
	c := newFeishuTest(t, f)
	fastReadBack(c)
	ctx := context.Background()
	f.unnumberedReads = 3
	task, err := c.Create(ctx, CreateInput{Title: "新票", Description: "随便写"})
	if err != nil || task.Identifier != "OF-10" {
		t.Fatalf("create %s err=%v", task.Identifier, err)
	}
	f.unnumberedReads = 2
	if _, err = c.Create(ctx, CreateInput{Title: "又一张", Description: "随便写"}); err != nil {
		t.Fatal(err)
	}
	f.unnumberedReads = 5
	f.mu.Lock()
	f.rows["tblT"] = append(f.rows["tblT"], feishuRow{"_id": "recPending", "标题": "刚建", "序号": "", "_serial": "NO.099", "项目": []feishu.LinkRef{{ID: "recP1"}}, "状态": []string{"Todo"}, "创建时间": "2026-10-15T01:00:00.000+08:00", "修改时间": "2026-10-15T01:00:00.000+08:00"})
	f.mu.Unlock()
	tasks, err := c.List(ctx)
	if err != nil {
		t.Fatalf("unnumbered row failed discovery: %v", err)
	}
	for _, task := range tasks {
		if task.Title == "刚建" {
			t.Fatal("unnumbered row listed")
		}
	}
}

// TestFeishuWriteCreateLedgerProject covers OF-50: onboarding adds one 项目
// row, 启用 false, read back from the table; repeating the same row (a retry
// after an unconfirmed write) converges without a second write; a row whose
// remote or 编号前缀 another row already uses is refused before any write.
func TestFeishuWriteCreateLedgerProject(t *testing.T) {
	f := newFeishuFake(t)
	c := newFeishuTest(t, f)
	fastReadBack(c)
	want := LedgerProject{Name: "ziikoo-control-plane", Remote: "github.com/vmxmy/ziikoo-control-plane", Prefix: "ZCP"}
	got, err := c.CreateLedgerProject(context.Background(), want)
	if err != nil || got != want {
		t.Fatalf("CreateLedgerProject=%+v, %v; want %+v", got, err, want)
	}
	if len(f.writes) != 1 || f.writes[0].table != "tblP" || f.writes[0].action != "batch_create" {
		t.Fatalf("writes=%+v", f.writes)
	}
	for record := range f.writes[0].records {
		if on := feishuCell[bool](t, f.writes[0], record, "fldPOn"); on {
			t.Fatal("启用 written as true, want false")
		}
	}
	if got, err = c.CreateLedgerProject(context.Background(), want); err != nil || got != want || len(f.writes) != 1 {
		t.Fatalf("repeat=%+v, %v; writes=%d", got, err, len(f.writes))
	}
	for _, clash := range []LedgerProject{
		{Name: "x", Remote: "github.com/vmxmy/new", Prefix: "OF"},
		{Name: "x", Remote: feishuRepo, Prefix: "NEW"},
	} {
		if _, err := c.CreateLedgerProject(context.Background(), clash); err == nil || len(f.writes) != 1 {
			t.Fatalf("clash %+v: err=%v writes=%d", clash, err, len(f.writes))
		}
	}
}

// TestFeishuEnableLedgerProject covers OF-54: an approver's click flips a
// disabled 项目 row's 启用 to true and records one 事件 row naming the
// operator and the time, not tied to any ticket; an already-enabled row
// converges without a second write or event; an unknown remote is refused.
func TestFeishuEnableLedgerProject(t *testing.T) {
	f := newFeishuFake(t)
	f.mu.Lock()
	f.rows["tblP"] = append(f.rows["tblP"], feishuRow{"_id": "recP3", "名称": "dujiao-next", "仓库远端": "github.com/dujiao-next/dujiao-next", "编号前缀": "DJ", "启用": false})
	f.mu.Unlock()
	c := newFeishuTest(t, f)
	fastReadBack(c)
	ctx := context.Background()
	const remote = "github.com/dujiao-next/dujiao-next"

	want := LedgerProject{Name: "dujiao-next", Remote: remote, Prefix: "DJ", Enabled: true}
	got, err := c.EnableLedgerProject(ctx, remote, "ou_APPROVER")
	if err != nil || got != want {
		t.Fatalf("EnableLedgerProject=%+v, %v; want %+v", got, err, want)
	}
	if len(f.writes) != 2 || f.writes[0].table != "tblP" || f.writes[0].action != "batch_update" || f.writes[1].table != "tblE" || f.writes[1].action != "batch_create" {
		t.Fatalf("writes=%+v", f.writes)
	}
	if on := feishuCell[bool](t, f.writes[0], "recP3", "fldPOn"); !on {
		t.Fatal("启用 not written as true")
	}
	events := f.eventWrites()
	if len(events) != 1 {
		t.Fatalf("event writes=%+v", events)
	}
	for record := range events[0].records {
		if op := feishuCell[string](t, events[0], record, "fld6umIs07"); op != "ou_APPROVER" {
			t.Fatalf("操作者=%q", op)
		}
		if _, ok := events[0].records[record]["fldd5wlUCT"]; ok {
			t.Fatal("票据 written for a project-level event, want it left empty")
		}
	}

	// A repeated click converges: already enabled, no further write or event.
	if got, err = c.EnableLedgerProject(ctx, remote, "ou_APPROVER"); err != nil || got != want || len(f.writes) != 2 {
		t.Fatalf("repeat=%+v, %v; writes=%d", got, err, len(f.writes))
	}

	if _, err := c.EnableLedgerProject(ctx, "github.com/nope/nope", "ou_APPROVER"); err == nil {
		t.Fatal("unknown remote: want an error")
	}
}

func selfCheckDescription(redReason string) string {
	return FeishuBrief{Todo: "1. 修", Check: "cd web && npx vitest run src/x.test.ts", Scope: "web/src", Body: "## intake 自检\n- CHECK 可执行且会红：是\n- CHECK 红因：" + redReason}.Description()
}

func TestCreateAcceptsAssertionFailureCheckRed(t *testing.T) {
	f := newFeishuFake(t)
	c := newFeishuTest(t, f)
	fastReadBack(c)
	if _, err := c.Create(context.Background(), CreateInput{Title: "有效红", Description: selfCheckDescription("断言失败 AssertionError: expected 1 to be 0")}); err != nil {
		t.Fatalf("assertion failure red rejected: %v", err)
	}
}

func TestCreateRejectsInvalidCheckRed(t *testing.T) {
	for _, reason := range []string{"CACError: Unknown option `--repeat`", "Usage: vitest [options]", "No test files found, exiting with code 1", "sh: vitest: command not found", "ran 0 tests", "testing: warning: no tests to run"} {
		f := newFeishuFake(t)
		if _, err := newFeishuTest(t, f).Create(context.Background(), CreateInput{Title: "无效红", Description: selfCheckDescription(reason)}); err == nil || len(f.writes) != 0 {
			t.Fatalf("feishu created ticket with invalid red %q: writes=%d err=%v", reason, len(f.writes), err)
		}
		gh, fake := newImportServer(t)
		if _, err := gh.Create(context.Background(), CreateInput{Title: "无效红", Description: selfCheckDescription(reason), Labels: []string{}}); err == nil || fake.writes != 0 {
			t.Fatalf("github created ticket with invalid red %q: writes=%d err=%v", reason, fake.writes, err)
		}
	}
}
