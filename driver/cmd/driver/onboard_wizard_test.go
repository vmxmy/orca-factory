package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// wizardFeishu serves the base/v3 endpoints the real issues.Feishu client
// uses, reusing the feishuFake* helpers from resume_deployment_feishu_test.go.
// Beyond reads it applies the writes onboarding performs — the 项目 row
// create, the 启用=true update and the enable 事件 create — and counts the
// first two, so the tests can assert how often the wizard touched the ledger.
type wizardFeishu struct {
	mu             sync.Mutex
	rows           []feishuFakeRow
	projectCreates int
	enableWrites   int
}

func newWizardFeishuServer(t *testing.T) (*wizardFeishu, *httptest.Server) {
	t.Helper()
	tables := map[string]feishuFakeTable{
		"tblP": {id: "tblP", name: "项目", fields: []feishuFakeField{
			{id: "fldPName", name: "名称", typ: "text"},
			{id: "fldPRemote", name: "仓库远端", typ: "text"},
			{id: "fldPPrefix", name: "编号前缀", typ: "text"},
			{id: "fldPOn", name: "启用", typ: "checkbox"},
		}},
		"tblT": {id: "tblT", name: "票据", fields: []feishuFakeField{
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
		}},
		"tblE": {id: "tblE", name: "事件", fields: []feishuFakeField{
			{id: "fldESummary", name: "摘要", typ: "text"},
			{id: "fldETicket", name: "票据", typ: "link"},
			{id: "fldEBody", name: "正文", typ: "text"},
			{id: "fldESource", name: "来源", typ: "select", options: []string{"driver", "resolver", "operator"}},
			{id: "fldEOperator", name: "操作者", typ: "text"},
			{id: "fldECreated", name: "创建时间", typ: "created_at"},
			{id: "fldEUpdated", name: "修改时间", typ: "updated_at"},
		}},
	}
	f := &wizardFeishu{}
	basePrefix := "/open-apis/base/v3/bases/BASE_TOKEN"
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "test-token", "expire": 7200}); err != nil {
			t.Fatal(err)
		}
	})
	mux.HandleFunc(basePrefix+"/tables", func(w http.ResponseWriter, _ *http.Request) {
		list := []map[string]any{{"id": "tblP", "name": "项目"}, {"id": "tblT", "name": "票据"}, {"id": "tblE", "name": "事件"}}
		feishuEnvelope(t, w, map[string]any{"tables": list})
	})
	mux.HandleFunc(basePrefix+"/tables/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, basePrefix+"/tables/")
		tableID, sub, _ := strings.Cut(rest, "/")
		tbl := tables[tableID]
		switch sub {
		case "fields":
			fields := make([]map[string]any, 0, len(tbl.fields))
			for _, field := range tbl.fields {
				def := map[string]any{"id": field.id, "name": field.name, "type": field.typ, "multiple": field.multiple}
				if len(field.options) > 0 {
					options := make([]map[string]any, 0, len(field.options))
					for _, o := range field.options {
						options = append(options, map[string]any{"name": o})
					}
					def["options"] = options
				}
				fields = append(fields, def)
			}
			feishuEnvelope(t, w, map[string]any{"fields": fields})
		case "records":
			f.serveRecords(t, w, tbl)
		case "records/batch_create":
			f.serveCreate(t, w, r, tbl)
		case "records/batch_update":
			f.serveUpdate(t, w, r, tbl)
		default:
			t.Fatalf("unexpected fake Feishu request %s", r.URL.Path)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return f, server
}

func (f *wizardFeishu) serveRecords(t *testing.T, w http.ResponseWriter, tbl feishuFakeTable) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.rows
	if tbl.id != "tblP" {
		rows = nil
	}
	feishuEnvelope(t, w, feishuRecordsPage(t, feishuFakeTable{id: tbl.id, name: tbl.name, fields: tbl.fields, rows: rows}))
}

func (f *wizardFeishu) serveCreate(t *testing.T, w http.ResponseWriter, r *http.Request, tbl feishuFakeTable) {
	var body struct {
		CreateRecords []map[string]any `json:"create_records"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(body.CreateRecords))
	for _, record := range body.CreateRecords {
		id := fmt.Sprintf("rec%d", len(f.rows)+len(ids)+1)
		if tbl.id == "tblP" {
			cells := map[string]any{}
			for _, field := range tbl.fields {
				if v, ok := record[field.id]; ok {
					cells[field.name] = v
				}
			}
			f.rows = append(f.rows, feishuFakeRow{id: id, cells: cells})
			f.projectCreates++
		}
		ids = append(ids, id)
	}
	feishuEnvelope(t, w, map[string]any{"record_id_list": ids})
}

func (f *wizardFeishu) serveUpdate(t *testing.T, w http.ResponseWriter, r *http.Request, tbl feishuFakeTable) {
	var body struct {
		UpdateRecords map[string]map[string]any `json:"update_records"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(body.UpdateRecords))
	for id, record := range body.UpdateRecords {
		for i := range f.rows {
			if f.rows[i].id != id {
				continue
			}
			for _, field := range tbl.fields {
				v, ok := record[field.id]
				if !ok {
					continue
				}
				f.rows[i].cells[field.name] = v
				if tbl.id == "tblP" && field.name == "启用" && v == true {
					f.enableWrites++
				}
			}
			ids = append(ids, id)
		}
	}
	feishuEnvelope(t, w, map[string]any{"record_id_list": ids})
}

// fakeOrcaTwoRepos lists the target repository and the factory itself: the
// factory's own Feishu profile is what discovery's ledger join reads through,
// so the rows the wizard writes are the rows its next observe reads back.
func fakeOrcaTwoRepos(t *testing.T, repoPath, repoKey, factoryPath, factoryKey string) string {
	t.Helper()
	body := `{"id":"3b3ee508-1f2b-4b8e-9b5a-6fbb2c5b8a3b","ok":true,"result":{"repos":[` +
		`{"path":"` + repoPath + `","gitRemoteIdentity":{"canonicalKey":"` + repoKey + `"}},` +
		`{"path":"` + factoryPath + `","gitRemoteIdentity":{"canonicalKey":"` + factoryKey + `"}}]}}`
	script := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '"+body+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// wizardFixture is one machine ready for the onboard wizard: the fake Feishu
// ledger behind the factory's own profile, the factory Orca-listed so
// discovery joins that ledger, the target repository cloned from a bare
// origin with a commit on main, a factory check binary that passes, and
// stand-ins for claude and gh on PATH. The claude stand-in opens PR #7 on gh
// only when its prompt says 续接, so run one (a fresh start) ends with no PR
// and run two (the resume) lands on wait-merge.
type wizardFixture struct {
	repo                             string
	ghMarker, claudeRuns, claudeArgs string
	ledger                           *wizardFeishu
	prOpen                           string
}

func newWizardFixture(t *testing.T) *wizardFixture {
	t.Helper()
	f := newOnboardFixture(t)
	root := filepath.Dir(f.factory)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := &wizardFixture{repo: f.repo,
		ghMarker: filepath.Join(root, "gh.prs"), claudeRuns: filepath.Join(root, "claude.runs"), claudeArgs: filepath.Join(root, "claude.args"),
		prOpen: `[{"number":7,"state":"OPEN","headRefName":"feat/agent-graph","isCrossRepository":false,"headRepository":{"name":"ziikoo-control-plane"},"headRepositoryOwner":{"login":"vmxmy"}}]`}
	ledger, server := newWizardFeishuServer(t)
	fx.ledger = ledger
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "test-app")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "test-secret")
	t.Setenv("AGENT_ORCA_BIN", fakeOrcaTwoRepos(t, f.repo, onboardRemote, f.factory, "github.com/vmxmy/orca-factory"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))

	writeExecutable(t, filepath.Join(f.factory, "bin", "orca-factory"), "#!/bin/sh\ncase \"$1\" in check) exit 0 ;; *) exit 2 ;; esac\n")
	writeExecutable(t, filepath.Join(bin, "gh"), "#!/bin/sh\ncase \"$1 $2\" in\n"+
		"  \"pr list\") if [ -f "+fx.ghMarker+" ]; then cat "+fx.ghMarker+"; else printf '[]'; fi ;;\n"+
		"  *) exit 9 ;;\nesac\n")
	const agentResult = `{"type":"result","subtype":"success","is_error":false,"result":"PR https://github.com/vmxmy/ziikoo-control-plane/pull/7"}`
	writeExecutable(t, filepath.Join(bin, "claude"), "#!/bin/sh\n"+
		"printf 'run\\n' >> "+fx.claudeRuns+"\n"+
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done >> "+fx.claudeArgs+"\n"+
		"case \"$2\" in *续接*) printf '%s' "+shQuote(fx.prOpen)+" > "+fx.ghMarker+" ;; esac\n"+
		"printf '%s' "+shQuote(agentResult)+"\n")

	origin := filepath.Join(root, "origin.git")
	gitIn(t, root, "init", "--quiet", "--bare", "-b", "main", origin)
	if err := os.RemoveAll(f.repo); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "clone", "--quiet", origin, f.repo)
	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "add", "README.md")
	gitIn(t, f.repo, "commit", "--quiet", "-m", "init")
	gitIn(t, f.repo, "push", "--quiet", "origin", "HEAD:main")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fx
}

func (fx *wizardFixture) claudeRunCount(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(fx.claudeRuns)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// withStdin runs body with os.Stdin replaced by a pipe preloaded with script.
func withStdin(t *testing.T, script string, body func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(script); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = original
		_ = r.Close()
	}()
	body()
}

// TestOnboardWizardResumesFromExternalFacts drives the wizard (OF-197) as a
// black box through one repository's lifecycle in three runs against the
// same machine: a first session whose agent opens no PR (exit 1 with the
// resume hint, exactly one ledger row written), a rerun that resumes the
// same worktree (续接 prompt, no second row) and reaches wait-merge once gh
// reports PR #7, and a third run after the pipeline file reached origin/main
// that enables the row without ever starting the agent again.
func TestOnboardWizardResumesFromExternalFacts(t *testing.T) {
	fx := newWizardFixture(t)

	var out []byte
	var code int
	out = captureStdout(t, func() {
		withStdin(t, "1\n\n", func() { code = runOnboard(nil) })
	})
	if code != 1 {
		t.Fatalf("round 1 exit=%d, want 1 (agent ran, gh reports no PR)\noutput=%s", code, out)
	}
	if !strings.Contains(string(out), "续接") {
		t.Fatalf("round 1 output missing the resume hint:\n%s", out)
	}
	if fx.ledger.projectCreates != 1 {
		t.Fatalf("round 1 项目表 writes=%d, want 1", fx.ledger.projectCreates)
	}

	out = captureStdout(t, func() {
		withStdin(t, "\nq\n", func() { code = runOnboard([]string{"--repo", fx.repo}) })
	})
	if code != 0 {
		t.Fatalf("round 2 exit=%d, want 0 (quit at wait-merge)\noutput=%s", code, out)
	}
	if !strings.Contains(string(out), "pull/7") {
		t.Fatalf("round 2 output missing the PR link:\n%s", out)
	}
	prompt, err := os.ReadFile(fx.claudeArgs)
	if err != nil || !strings.Contains(string(prompt), "续接") {
		t.Fatalf("round 2 agent prompt missing 续接 (err=%v):\n%s", err, prompt)
	}
	if fx.ledger.projectCreates != 1 {
		t.Fatalf("round 2 项目表 writes=%d, want the resumed session to write no new row", fx.ledger.projectCreates)
	}

	writeTestFile(t, filepath.Join(fx.repo, "scripts/agent-gates/agent-graph.env"),
		"AGENT_ISSUE_BACKEND=feishu\nAGENT_FEISHU_BASE=BASE_TOKEN\nAGENT_FEISHU_PROJECT="+onboardRemote+"\n")
	gitIn(t, fx.repo, "add", "scripts/agent-gates/agent-graph.env")
	gitIn(t, fx.repo, "commit", "--quiet", "-m", "onboard merged")
	gitIn(t, fx.repo, "push", "--quiet", "origin", "HEAD:main")
	if err := os.WriteFile(fx.ghMarker, []byte(strings.Replace(fx.prOpen, `"OPEN"`, `"MERGED"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		withStdin(t, "", func() { code = runOnboard([]string{"--repo", fx.repo}) })
	})
	if code != 0 {
		t.Fatalf("round 3 exit=%d, want 0 (enabled)\noutput=%s", code, out)
	}
	if runs := fx.claudeRunCount(t); runs != 2 {
		t.Fatalf("round 3 claude runs=%d, want the 2 runs from rounds 1 and 2", runs)
	}
	if fx.ledger.enableWrites != 1 {
		t.Fatalf("round 3 启用 writes=%d, want 1", fx.ledger.enableWrites)
	}
}

// TestOnboardWizardBlocksOnResidue covers the wizard's dead end (OF-197): a
// remote feat/agent-graph branch plus a leftover directory at the onboarding
// worktree path that is no longer a git worktree is residue. The wizard
// reports both with the fix and exits 1 without touching the ledger or
// starting the agent.
func TestOnboardWizardBlocksOnResidue(t *testing.T) {
	fx := newWizardFixture(t)
	gitIn(t, fx.repo, "push", "--quiet", "origin", "main:refs/heads/"+onboardBranch)
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(onboardWorktree(cache, onboardRemote), 0o755); err != nil {
		t.Fatal(err)
	}
	var out []byte
	var code int
	out = captureStdout(t, func() {
		withStdin(t, "", func() { code = runOnboard([]string{"--repo", fx.repo}) })
	})
	if code != 1 {
		t.Fatalf("exit=%d, want 1\noutput=%s", code, out)
	}
	if !strings.Contains(string(out), "受阻") || !strings.Contains(string(out), "worktree 已存在") || !strings.Contains(string(out), "远端分支") {
		t.Fatalf("output missing 受阻 with both residue items:\n%s", out)
	}
	if fx.ledger.projectCreates != 0 {
		t.Fatalf("项目表 writes=%d, want 0", fx.ledger.projectCreates)
	}
	if runs := fx.claudeRunCount(t); runs != 0 {
		t.Fatalf("claude runs=%d, want 0", runs)
	}
}
