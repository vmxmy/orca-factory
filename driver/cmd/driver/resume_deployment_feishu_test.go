package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

// feishuFakeField describes one field of a fake Feishu table, matching the
// shape internal/issues/feishu.go's feishuTables requires (name, type,
// multiple, and the option values ValidateSchema checks for).
type feishuFakeField struct {
	id, name, typ string
	multiple      bool
	options       []string
}

// feishuFakeRow is one record: its Base record ID and its cell values keyed
// by field name (not field ID; feishuRecordsPage looks the ID up).
type feishuFakeRow struct {
	id    string
	cells map[string]any
}

type feishuFakeTable struct {
	id, name string
	fields   []feishuFakeField
	rows     []feishuFakeRow
}

// feishuEnvelope wraps data the way base/v3 wraps every response this driver
// reads: {"code":0,"msg":"success","data":...}.
func feishuEnvelope(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success", "data": data}); err != nil {
		t.Fatal(err)
	}
}

// feishuRecordsPage renders tbl's rows in the column-oriented shape
// internal/feishu.Client.readPage decodes: parallel field_id_list,
// field_type_list, fields and record_id_list arrays alongside a data matrix,
// one page, nothing more pending.
func feishuRecordsPage(t *testing.T, tbl feishuFakeTable) map[string]any {
	t.Helper()
	fieldIDs := make([]string, len(tbl.fields))
	fieldTypes := make([]string, len(tbl.fields))
	fieldNames := make([]string, len(tbl.fields))
	for i, f := range tbl.fields {
		fieldIDs[i], fieldTypes[i], fieldNames[i] = f.id, f.typ, f.name
	}
	recordIDs := make([]string, 0, len(tbl.rows))
	data := make([][]any, 0, len(tbl.rows))
	for _, row := range tbl.rows {
		recordIDs = append(recordIDs, row.id)
		cells := make([]any, len(tbl.fields))
		for i, f := range tbl.fields {
			cells[i] = row.cells[f.name]
		}
		data = append(data, cells)
	}
	return map[string]any{
		"data": data, "field_id_list": fieldIDs, "field_type_list": fieldTypes, "fields": fieldNames,
		"record_id_list": recordIDs, "has_more": false, "rev": int64(1),
	}
}

// newFakeFeishuServer starts an httptest server answering exactly the base/v3
// endpoints the driver's Feishu backend calls: tenant auth, table listing,
// field listing, and record listing (any filter: this fixture always has one
// project and one ticket, so filtering by project or by ticket never excludes
// anything the test does not already expect to see).
func newFakeFeishuServer(t *testing.T, base string, tables []feishuFakeTable) *httptest.Server {
	t.Helper()
	byID := map[string]feishuFakeTable{}
	for _, tbl := range tables {
		byID[tbl.id] = tbl
	}
	basePrefix := "/open-apis/base/v3/bases/" + base
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, _ *http.Request) {
		// The tenant token endpoint returns tenant_access_token and expire at
		// the top level, not nested under "data" like every other base/v3
		// response this client decodes.
		if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "test-token", "expire": 7200}); err != nil {
			t.Fatal(err)
		}
	})
	mux.HandleFunc(basePrefix+"/tables", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != basePrefix+"/tables" {
			http.NotFound(w, r)
			return
		}
		list := make([]map[string]any, 0, len(tables))
		for _, tbl := range tables {
			list = append(list, map[string]any{"id": tbl.id, "name": tbl.name})
		}
		feishuEnvelope(t, w, map[string]any{"tables": list})
	})
	mux.HandleFunc(basePrefix+"/tables/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, basePrefix+"/tables/")
		tableID, sub, _ := strings.Cut(rest, "/")
		tbl, ok := byID[tableID]
		if !ok {
			t.Fatalf("unexpected fake Feishu table id %s (path %s)", tableID, r.URL.Path)
		}
		switch sub {
		case "fields":
			fields := make([]map[string]any, 0, len(tbl.fields))
			for _, f := range tbl.fields {
				field := map[string]any{"id": f.id, "name": f.name, "type": f.typ, "multiple": f.multiple}
				if len(f.options) > 0 {
					opts := make([]map[string]any, 0, len(f.options))
					for _, o := range f.options {
						opts = append(opts, map[string]any{"name": o})
					}
					field["options"] = opts
				}
				fields = append(fields, field)
			}
			feishuEnvelope(t, w, map[string]any{"fields": fields})
		case "records":
			feishuEnvelope(t, w, feishuRecordsPage(t, tbl))
		default:
			t.Fatalf("unexpected fake Feishu request %s", r.URL.Path)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// fakeOrcaForDeploymentRecovery writes a stub orca binary answering `repo
// list` with one repository and `automations list` with none (so
// Runtime.Track's AutomationDisabled check admits it), and returns its
// absolute path for AGENT_ORCA_BIN.
func fakeOrcaForDeploymentRecovery(t *testing.T, repoPath, canonicalKey string) string {
	t.Helper()
	repoListBody, err := json.Marshal(map[string]any{
		"ok": true, "result": map[string]any{"repos": []map[string]any{
			{"path": repoPath, "gitRemoteIdentity": map[string]any{"canonicalKey": canonicalKey}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	content := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"  \"repo list\") printf '%s' " + shQuote(string(repoListBody)) + " ;;\n" +
		"  \"automations list\") printf '%s' '{\"ok\":true,\"result\":{\"automations\":[]}}' ;;\n" +
		"  *) exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// shQuote wraps s in single quotes for embedding as one shell word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// captureStderr mirrors captureStdout (projects_test.go) for commands that
// write their result to os.Stderr, as resumeDeployment does.
func captureStderr(t *testing.T, run func()) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = original }()
	run()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestResumeDeploymentFindsBarrierRegardlessOfProjectEligibility is OF-44's
// acceptance test: the ticket's named scenario, a project-table row with 启用
// = false. The fixture repository's own pipeline is installed and its check
// passes, so projects.Discover rejects it for exactly ReasonProjectDisabled,
// not for any other reason, meaning discoverProjects/eligibleRepositories
// exclude the repository from the set resume-deployment used to search. A
// deployment barrier (merge-progress-v1, Deployment.State "paused") is
// durable in the ticket's Feishu event history regardless. Before OF-44,
// resume-deployment's repository source was eligibleRepositories
// (discoverProjects's output), so it could never find this ticket. After
// OF-44, it enumerates every repository with a readable agent-graph.env
// declaring a Feishu ledger, so it finds the ticket and, in --dry-run mode,
// reports the recovery as validated without writing anything.
func TestResumeDeploymentFindsBarrierRegardlessOfProjectEligibility(t *testing.T) {
	const canonicalKey = "github.com/example/of44-driver-test"
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "scripts", "agent-gates"), 0o755); err != nil {
		t.Fatal(err)
	}
	const feishuBase = "BASE1"
	// AGENT_FEISHU_PROJECT is conventionally the repository's own canonical
	// remote (README.md: "仓库远端 填 AGENT_FEISHU_PROJECT 的值"), the same
	// value the 项目 table row's 仓库远端 carries and Orca's repo list reports
	// as this repository's CanonicalKey.
	envContent := "AGENT_ISSUE_BACKEND=feishu\nAGENT_FEISHU_BASE=" + feishuBase + "\nAGENT_FEISHU_PROJECT=" + canonicalKey + "\nAGENT_SCOPE_AVOID=0\n"
	if err := os.WriteFile(filepath.Join(repoDir, "scripts", "agent-gates", "agent-graph.env"), []byte(envContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// factory_root and a passing check binary are configured so the fixture
	// reproduces the ticket's named scenario precisely: this repository is
	// rejected for ReasonProjectDisabled specifically (its own pipeline is
	// installed and its check passes; only its ledger row's 启用 is false),
	// not merely because factory_root is unconfigured.
	factoryRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(factoryRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(factoryRoot, "bin", "orca-factory"), []byte("#!/bin/sh\ncase \"$1\" in\n  check) exit 0 ;;\n  *) exit 2 ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "orca-factory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "orca-factory", "driver.toml"), []byte("factory_root = "+strconv.Quote(factoryRoot)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AGENT_FEISHU_ENDPOINT", "placeholder") // overwritten with the fake server URL below
	t.Setenv("AGENT_FEISHU_APP_ID", "test-app")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "test-secret")

	const (
		projectRecordID  = "recProject1"
		ticketRecordID   = "recTicket1"
		ticketCanonical  = "11111111-2222-4333-8444-555555555555"
		ticketIdentifier = "OF44-1"
		queueID          = "q1"
	)
	head := strings.Repeat("a", 40)
	baseline := strings.Repeat("b", 40)
	mainSHA := strings.Repeat("c", 40)
	mergeSHA := strings.Repeat("d", 40)
	created := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)

	brief := issues.FeishuBrief{
		Todo:  "验证 OF-44 恢复路径",
		Check: "resume-deployment --dry-run 命中部署屏障",
		Scope: "driver/cmd/driver/** driver/internal/projects/**",
		Body:  "手工构造的部署屏障票据",
	}
	fingerprint := brief.Fingerprint()

	handoff := merge.Handoff{
		Version: 1, TicketID: ticketCanonical, Identifier: ticketIdentifier, Repository: "o/r", Unit: "executor",
		PR: 1, Head: head, Baseline: baseline,
		GateSpec:     "UNIT: executor\nSCOPE: feature\nCHECK: true\nLANE: 1\n",
		GateOutput:   "UNIT executor\nVERDICT green\nSTEP all\nREASON passed\nEVIDENCE test\nSCOPE feature\nLANE 1",
		ReviewOutput: `{"verdict":"green","step":"review"}`,
		Lane:         1, Run: "run_1", Task: "task_1", Dispatch: "dispatch_1", Worktree: repoDir,
		Attempt: 1, EnqueuedAt: created, QueueID: queueID,
	}
	handoffJSON, err := json.Marshal(handoff)
	if err != nil {
		t.Fatal(err)
	}

	progressPending := merge.Progress{
		Version: 1, QueueID: queueID, TicketID: ticketCanonical, Repository: "o/r", PR: 1, Head: head,
		Main: mainSHA, MergeSHA: mergeSHA, Phase: "merged",
		Deployment: &merge.DeploymentProgress{State: "pending", Smoke: "smoke-test"},
	}
	progressPendingJSON, err := json.Marshal(progressPending)
	if err != nil {
		t.Fatal(err)
	}

	progressPaused := progressPending
	paused := *progressPending.Deployment
	paused.State, paused.Reason = "paused", "deployment workflow failed"
	progressPaused.Deployment = &paused
	progressPausedJSON, err := json.Marshal(progressPaused)
	if err != nil {
		t.Fatal(err)
	}

	projectTable := feishuFakeTable{
		id: "tblProject1", name: "项目",
		fields: []feishuFakeField{
			{id: "fldPName", name: "名称", typ: "text"},
			{id: "fldPRemote", name: "仓库远端", typ: "text"},
			{id: "fldPPrefix", name: "编号前缀", typ: "text"},
			{id: "fldPEnabled", name: "启用", typ: "checkbox"},
		},
		rows: []feishuFakeRow{{id: projectRecordID, cells: map[string]any{
			"名称": "OF44 Test Project", "仓库远端": canonicalKey, "编号前缀": "OF44", "启用": false,
		}}},
	}
	ticketTable := feishuFakeTable{
		id: "tblTicket1", name: "票据",
		fields: []feishuFakeField{
			{id: "fldTTitle", name: "标题", typ: "text"},
			{id: "fldTSerial", name: "序号", typ: "auto_number"},
			{id: "fldTCanonical", name: "规范ID", typ: "text"},
			{id: "fldTProject", name: "项目", typ: "link"},
			{id: "fldTState", name: "状态", typ: "select", options: []string{"Backlog", "Todo", "In Progress", "Done", "Canceled", "Duplicate"}},
			{id: "fldTLabels", name: "标签", typ: "select", multiple: true, options: []string{"ready-for-agent", "auto-impl", "auto-failed"}},
			{id: "fldTTodo", name: "要做", typ: "text"},
			{id: "fldTCheck", name: "CHECK", typ: "text"},
			{id: "fldTScope", name: "SCOPE", typ: "text"},
			{id: "fldTBody", name: "正文", typ: "text"},
			{id: "fldTBlockers", name: "阻塞于", typ: "link"},
			{id: "fldTRelated", name: "相关", typ: "link"},
			{id: "fldTEstimate", name: "估算", typ: "number"},
			{id: "fldTFingerprint", name: "纳管指纹", typ: "text"},
			{id: "fldTCreated", name: "创建时间", typ: "created_at"},
			{id: "fldTUpdated", name: "修改时间", typ: "updated_at"},
		},
		rows: []feishuFakeRow{{id: ticketRecordID, cells: map[string]any{
			"标题": "OF-44 恢复测试", "序号": "1", "规范ID": ticketCanonical,
			"项目": []map[string]any{{"id": projectRecordID}},
			"状态": []string{"In Progress"}, "标签": []string{},
			"要做": brief.Todo, "CHECK": brief.Check, "SCOPE": brief.Scope, "正文": brief.Body,
			"阻塞于": []map[string]any{}, "相关": []map[string]any{}, "估算": nil,
			"纳管指纹": fingerprint,
			"创建时间": created.Format(time.RFC3339), "修改时间": created.Format(time.RFC3339),
		}}},
	}
	eventTable := feishuFakeTable{
		id: "tblEvent1", name: "事件",
		fields: []feishuFakeField{
			{id: "fldESummary", name: "摘要", typ: "text"},
			{id: "fldETicket", name: "票据", typ: "link"},
			{id: "fldEBody", name: "正文", typ: "text"},
			{id: "fldESource", name: "来源", typ: "select", options: []string{"driver", "resolver", "operator"}},
			{id: "fldEOperator", name: "操作者", typ: "text"},
			{id: "fldECreated", name: "创建时间", typ: "created_at"},
			{id: "fldEUpdated", name: "修改时间", typ: "updated_at"},
		},
		rows: []feishuFakeRow{
			{id: "recEvent0", cells: map[string]any{
				"摘要": "合并交接", "票据": []map[string]any{{"id": ticketRecordID}},
				"正文": "阶段: 排队合并\n归属: 合并通道\n" + merge.HandoffPrefix + string(handoffJSON),
				"来源": []string{"driver"}, "操作者": "driver",
				"创建时间": created.Format(time.RFC3339), "修改时间": created.Format(time.RFC3339),
			}},
			{id: "recEvent1", cells: map[string]any{
				"摘要": "已合并", "票据": []map[string]any{{"id": ticketRecordID}},
				"正文": "已合并: PR #1 squash " + mergeSHA + "\n阶段: 已合并\n归属: 无\n" + merge.ProgressPrefix + string(progressPendingJSON),
				"来源": []string{"driver"}, "操作者": "driver",
				"创建时间": created.Add(time.Minute).Format(time.RFC3339), "修改时间": created.Add(time.Minute).Format(time.RFC3339),
			}},
			{id: "recEvent2", cells: map[string]any{
				"摘要": "部署处置", "票据": []map[string]any{{"id": ticketRecordID}},
				"正文": "Agent处置: deployment workflow failed\n处置要求: 核对部署与 smoke，返回绑定队列和 SHA 的恢复决策\n阶段: Agent处置\n归属: 处置Agent\n" + merge.ProgressPrefix + string(progressPausedJSON),
				"来源": []string{"driver"}, "操作者": "driver",
				"创建时间": created.Add(2 * time.Minute).Format(time.RFC3339), "修改时间": created.Add(2 * time.Minute).Format(time.RFC3339),
			}},
		},
	}

	server := newFakeFeishuServer(t, feishuBase, []feishuFakeTable{projectTable, ticketTable, eventTable})
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)

	t.Setenv("AGENT_ORCA_BIN", fakeOrcaForDeploymentRecovery(t, repoDir, canonicalKey))

	args := []string{
		"--issue", ticketIdentifier, "--queue", queueID, "--sha", mergeSHA,
		"--action", "confirm-passed", "--operator", "operator", "--reason", "checked production", "--dry-run",
	}
	var code int
	stderr := captureStderr(t, func() { code = resumeDeployment(args) })
	if code != 0 || !strings.Contains(string(stderr), "Recovery validated; no changes made") {
		t.Fatalf("resume-deployment did not find the barrier: code=%d stderr=%s", code, stderr)
	}
}
