package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/hostmem"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestDoctorReadsFeishuSchemaOncePerBase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_FEISHU_APP_ID", "doctor-test-app")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "doctor-test-secret")
	type field struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	schemas := map[string]struct {
		Fields []field `json:"fields"`
	}{}
	responses := map[string]json.RawMessage{}
	for table, fixture := range map[string]string{
		"projects": "bot-project-fields.json",
		"tickets":  "fields-list.json",
		"events":   "bot-fields-list-events.json",
	} {
		raw, err := os.ReadFile(filepath.Join("../../testdata/recorded/feishu", fixture))
		if err != nil {
			t.Fatal(err)
		}
		var recorded struct {
			Response struct {
				Data json.RawMessage `json:"data"`
			} `json:"response"`
		}
		if err := json.Unmarshal(raw, &recorded); err != nil {
			t.Fatal(err)
		}
		responses[table] = recorded.Response.Data
		schema := schemas[table]
		if err := json.Unmarshal(recorded.Response.Data, &schema); err != nil {
			t.Fatal(err)
		}
		schemas[table] = schema
	}
	page := func(table string, ids []string, rows []map[string]any) map[string]any {
		fieldIDs, names, types := []string{}, []string{}, []string{}
		data := [][]any{}
		for _, field := range schemas[table].Fields {
			fieldIDs = append(fieldIDs, field.ID)
			names = append(names, field.Name)
			types = append(types, field.Type)
		}
		for _, row := range rows {
			cells := []any{}
			for _, name := range names {
				cells = append(cells, row[name])
			}
			data = append(data, cells)
		}
		return map[string]any{"field_id_list": fieldIDs, "fields": names, "field_type_list": types, "record_id_list": ids, "data": data, "rev": 1, "has_more": false}
	}
	var mu sync.Mutex
	requests := map[string]int{}
	filters := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests[request.URL.Path]++
		var data any
		switch request.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			if err := json.NewEncoder(writer).Encode(map[string]any{"code": 0, "tenant_access_token": "doctor-token", "expire": 7200}); err != nil {
				t.Error(err)
			}
			return
		case "/open-apis/base/v3/bases/doctor-base/tables":
			data = map[string]any{"tables": []map[string]string{{"id": "projects", "name": "项目"}, {"id": "tickets", "name": "票据"}, {"id": "events", "name": "事件"}}}
		case "/open-apis/base/v3/bases/doctor-base/tables/projects/records":
			data = page("projects", []string{"rec-one", "rec-two"}, []map[string]any{
				{"名称": "one", "仓库远端": "github.com/test/one", "编号前缀": "ONE", "启用": true},
				{"名称": "two", "仓库远端": "github.com/test/two", "编号前缀": "TWO", "启用": true},
			})
		case "/open-apis/base/v3/bases/doctor-base/tables/tickets/records":
			filter := request.URL.Query().Get("filter")
			filters[filter]++
			project, serial := "", ""
			switch filter {
			case `{"logic":"and","conditions":[["项目","intersects",[{"id":"rec-one"}]]]}`:
				project, serial = "rec-one", "NO.001"
			case `{"logic":"and","conditions":[["项目","intersects",[{"id":"rec-two"}]]]}`:
				project, serial = "rec-two", "NO.002"
			default:
				t.Errorf("unexpected ticket filter: %s", filter)
				http.Error(writer, "invalid filter", http.StatusBadRequest)
				return
			}
			data = page("tickets", []string{"ticket-" + project}, []map[string]any{{
				"标题": project, "序号": serial, "项目": []map[string]string{{"id": project}}, "状态": []string{"In Progress"},
				"创建时间": "2026-11-04T00:00:00Z", "修改时间": "2026-11-04T00:00:00Z",
			}})
		case "/open-apis/base/v3/bases/doctor-base/tables/events/records":
			data = page("events", []string{}, nil)
		default:
			table := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/open-apis/base/v3/bases/doctor-base/tables/"), "/fields")
			var ok bool
			if data, ok = responses[table]; !ok || !strings.HasSuffix(request.URL.Path, "/fields") {
				t.Errorf("unexpected request: %s", request.URL.Path)
				http.NotFound(writer, request)
				return
			}
		}
		if err := json.NewEncoder(writer).Encode(map[string]any{"code": 0, "data": data}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	eligible := []projects.Project{}
	for _, name := range []string{"one", "two"} {
		eligible = append(eligible, projects.Project{Name: name, Repository: config.Repository{Env: map[string]string{
			"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_BASE": "doctor-base", "AGENT_FEISHU_PROJECT": "github.com/test/" + name,
		}}})
	}
	eligible[1].Repository.Env["AGENT_FEISHU_BASE"] = "https://example.feishu.cn/base/doctor-base"
	for run := 1; run <= 2; run++ {
		probe, err := newDoctorProbe()
		if err != nil {
			t.Fatal(err)
		}
		check := probe.checkLedger(context.Background(), eligible)
		if check.Status != healthOK {
			t.Fatalf("ledger check = %+v", check)
		}
		want := []doctorLedgerTicket{{Project: "one", Identifier: "ONE-1", Headroom: issues.FeishuBodyRunes}, {Project: "two", Identifier: "TWO-2", Headroom: issues.FeishuBodyRunes}}
		if got := check.Details.(map[string]any)["tickets"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("ledger tickets = %+v, want %+v", got, want)
		}
		mu.Lock()
		for _, path := range []string{"/open-apis/auth/v3/tenant_access_token/internal", "/open-apis/base/v3/bases/doctor-base/tables", "/open-apis/base/v3/bases/doctor-base/tables/projects/fields", "/open-apis/base/v3/bases/doctor-base/tables/tickets/fields", "/open-apis/base/v3/bases/doctor-base/tables/events/fields"} {
			if got := requests[path]; got != run {
				t.Errorf("run %d: %s requested %d times, want %d", run, path, got, run)
			}
		}
		if len(filters) != 2 {
			t.Errorf("project filters = %v, want two distinct filters", filters)
		}
		mu.Unlock()
	}
}

type doctorFakeLedger struct {
	tasks    []issues.Task
	comments map[string][]task.Comment
}

func (l doctorFakeLedger) List(context.Context) ([]issues.Task, error) { return l.tasks, nil }
func (l doctorFakeLedger) ListComments(_ context.Context, id string) ([]task.Comment, error) {
	return l.comments[id], nil
}

type doctorFixture struct {
	repository                       config.Repository
	root, factory, previous, current string
	now                              time.Time
}

// newDoctorFixture builds a factory checkout whose origin/main is one commit
// past previous, both committed two hours ago, a gh stand-in on PATH, and an empty driver state root.
func newDoctorFixture(t *testing.T) doctorFixture {
	t.Helper()
	root := t.TempDir()
	origin, factory := filepath.Join(root, "origin.git"), filepath.Join(root, "factory")
	gitIn(t, root, "init", "--quiet", "--bare", "-b", "main", origin)
	gitIn(t, root, "clone", "--quiet", origin, factory)
	now := time.Now()
	t.Setenv("GIT_COMMITTER_DATE", now.Add(-2*time.Hour).Format(time.RFC3339))
	gitIn(t, factory, "commit", "--quiet", "--allow-empty", "-m", "previous")
	previous := gitIn(t, factory, "rev-parse", "--short", "HEAD")
	gitIn(t, factory, "commit", "--quiet", "--allow-empty", "-m", "current")
	gitIn(t, factory, "push", "--quiet", "origin", "HEAD:main")
	gitIn(t, factory, "fetch", "--quiet", "origin")
	current := gitIn(t, factory, "rev-parse", "--short", "HEAD")

	bin := filepath.Join(root, "bin")
	gh := "#!/bin/sh\ncase \"$1 $2\" in\n\"pr list\") cat \"$DOCTOR_GH_PRS\" ;;\n\"auth status\") printf '%s' \"$DOCTOR_GH_AUTH_STDERR\" >&2; exit \"$DOCTOR_GH_AUTH\" ;;\n*) exit 9 ;;\nesac\n"
	writeDoctorFile(t, filepath.Join(bin, "gh"), gh, 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := filepath.Join(root, "state")
	t.Setenv("AGENT_DRIVER_STATE_ROOT", state)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return doctorFixture{root: root, factory: factory, previous: previous, current: current, now: now,
		repository: config.Repository{Env: map[string]string{
			"AGENT_TIER_DEFAULT": "codex:m:high", "AGENT_ROLE_REVIEWER": "codex:m:high", "AGENT_ROLE_INTAKE": "claude:m:high",
		}},
	}
}

func writeDoctorFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func modelFixtureProbe(now time.Time, stateRoot string) doctorProbe {
	return doctorProbe{
		now: now, stateRoot: stateRoot, recordModels: true,
		modelSettings: func() doctorModelSettings { return doctorModelSettings{} },
		modelProbe: func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
			return modelhealth.Classify(ref, "ok", nil, now)
		},
	}
}

func (f doctorFixture) probe(t *testing.T, serviceTools []string, ledger doctorFakeLedger) doctorProbe {
	t.Helper()
	serviceBin := filepath.Join(f.root, "service-bin")
	for _, tool := range serviceTools {
		writeDoctorFile(t, filepath.Join(serviceBin, tool), "#!/bin/sh\n", 0o755)
	}
	p := modelFixtureProbe(f.now, filepath.Join(f.root, "state"))
	p.factoryRoot = f.factory
	p.serviceEnv = func(context.Context) (map[string]string, error) {
		return map[string]string{"PATH": serviceBin, "AGENT_FEISHU_APP_ID": "TEST-PLACEHOLDER-CRED", "AGENT_FEISHU_APP_SECRET": "TEST-PLACEHOLDER-CRED"}, nil
	}
	p.projects = func(context.Context) (projects.Result, error) {
		return projects.Result{Eligible: []projects.Project{{Path: f.factory, Prefix: "OF", Name: "orca-factory", Repository: f.repository}}}, nil
	}
	p.ledger = func(context.Context, projects.Project) (doctorLedger, error) { return ledger, nil }
	p.githubReachable = func(context.Context) error { return nil }
	return p
}

func (f doctorFixture) logLine(t *testing.T, name string, at time.Time, fields string) {
	t.Helper()
	path := filepath.Join(f.root, "state", name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := fmt.Fprintf(file, "{\"time\":%q,%s}\n", at.Format(time.RFC3339Nano), fields); err != nil {
		t.Fatal(err)
	}
}

type doctorResult struct {
	code     int
	statuses map[string]string
	reasons  map[string]string
	errors   []string
	details  map[string]json.RawMessage
	out      []byte
}

func runDoctorProbe(t *testing.T, probe doctorProbe) doctorResult {
	t.Helper()
	var out bytes.Buffer
	code := probe.main(context.Background(), []string{"--json"}, &out)
	var report struct {
		Checks map[string]struct {
			Status  string          `json:"status"`
			Reason  string          `json:"reason"`
			Details json.RawMessage `json:"details"`
		} `json:"checks"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("doctor --json is not JSON: %v\n%s", err, out.String())
	}
	if report.Errors == nil {
		t.Fatalf("errors must be an array:\n%s", out.String())
	}
	if strings.Contains(out.String(), "TEST-PLACEHOLDER-CRED") {
		t.Fatalf("doctor output leaks a credential value:\n%s", out.String())
	}
	result := doctorResult{code: code, statuses: map[string]string{}, reasons: map[string]string{}, errors: report.Errors, details: map[string]json.RawMessage{}, out: out.Bytes()}
	for name, check := range report.Checks {
		result.statuses[name] = check.Status
		result.reasons[name] = check.Reason
		result.details[name] = check.Details
	}
	return result
}

func (r doctorResult) want(t *testing.T, code int, statuses map[string]string) {
	t.Helper()
	for name, status := range statuses {
		if r.statuses[name] != status {
			t.Errorf("%s = %q, want %q", name, r.statuses[name], status)
		}
	}
	if r.code != code {
		t.Errorf("exit %d, want %d", r.code, code)
	}
	if t.Failed() {
		t.Fatalf("doctor output:\n%s", r.out)
	}
}

func inFlightLedger(id string, bodies ...string) doctorFakeLedger {
	comments := []task.Comment{}
	for i, body := range bodies {
		comments = append(comments, task.Comment{ID: fmt.Sprintf("c%d", i), Body: body})
	}
	inFlight := issues.Task{Issue: task.Issue{ID: id, Identifier: id, State: task.NamedID{ID: "started", Name: "In Progress"}}}
	return doctorFakeLedger{tasks: []issues.Task{inFlight}, comments: map[string][]task.Comment{id: comments}}
}

func (f doctorFixture) syncPR(t *testing.T, version string, created time.Time) {
	t.Helper()
	prs := fmt.Sprintf(`[{"number":7,"headRefName":"%s%s","url":"https://example.invalid/pr/7","createdAt":%q}]`, coreSyncBranchPrefix, version, created.UTC().Format(time.RFC3339))
	writeDoctorFile(t, filepath.Join(f.root, "prs.json"), prs, 0o600)
	t.Setenv("DOCTOR_GH_PRS", filepath.Join(f.root, "prs.json"))
}

func (f doctorFixture) ageFetch(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	path := filepath.Join(dir, gitIn(t, dir, "rev-parse", "--git-path", "FETCH_HEAD"))
	when := f.now.Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestDriverDoctorReportsHealthyRuntime(t *testing.T) {
	f := newDoctorFixture(t)
	writeDoctorFile(t, filepath.Join(f.root, "prs.json"), `[]`, 0o600)
	t.Setenv("DOCTOR_GH_PRS", filepath.Join(f.root, "prs.json"))
	t.Setenv("DOCTOR_GH_AUTH", "0")
	f.logLine(t, "scheduler.log", f.now.Add(-time.Hour), `"msg":"command started","component":"scheduler","event":"start","version":"`+f.current+`"`)
	f.logLine(t, "launch-OF-1.log", f.now.Add(-10*time.Minute), `"msg":"command started","event":"start","version":"`+f.current+`"`)
	f.logLine(t, "launch-OF-1.log", f.now.Add(-9*time.Minute), `"msg":"command exited","event":"exit","exit_code":0`)

	result := runDoctorProbe(t, f.probe(t, []string{"go", "gh", "git", "orca", "claude"}, inFlightLedger("OF-1", "进度")))

	result.want(t, 0, map[string]string{"service_path": "ok", "version": "ok", "maintenance": "ok", "checkouts": "ok", "sync_prs": "ok", "ledger": "ok", "launches": "ok", "credentials": "ok", "models": "ok"})
	if len(result.errors) != 0 {
		t.Fatalf("errors = %v, want none", result.errors)
	}
	if _, err := os.Stat(filepath.Join(f.root, "state", "model-health.json")); err != nil {
		t.Fatalf("doctor --json must record model status: %v", err)
	}
	var models []doctorModelStatus
	if err := json.Unmarshal(result.details["models"], &models); err != nil {
		t.Fatal(err)
	}
	roles := map[string]int{}
	for _, row := range models {
		wantAgent := "codex"
		if row.Role == "intake" {
			wantAgent = "claude"
		}
		if row.Repository != "orca-factory" || row.Status.State != "ok" || row.Status.Ref.Agent != wantAgent {
			t.Fatalf("models main-path row = %+v", row)
		}
		roles[row.Role]++
	}
	if !reflect.DeepEqual(roles, map[string]int{"worker_default": 1, "reviewer": 1, "resolver": 1, "intake": 1}) {
		t.Fatalf("models main-path roles = %v", roles)
	}
	var version struct {
		MainCommittedAt string `json:"factory_main_committed_at"`
		FetchedAt       string `json:"factory_fetched_at"`
	}
	var checkouts struct {
		Projects []struct {
			MainCommittedAt string `json:"origin_main_committed_at"`
			FetchedAt       string `json:"fetched_at"`
		} `json:"projects"`
	}
	if json.Unmarshal(result.details["version"], &version) != nil || version.MainCommittedAt == "" || version.FetchedAt == "" {
		t.Fatalf("version details must carry origin/main commit time and FETCH_HEAD time:\n%s", result.out)
	}
	if json.Unmarshal(result.details["checkouts"], &checkouts) != nil || len(checkouts.Projects) != 1 || checkouts.Projects[0].MainCommittedAt == "" || checkouts.Projects[0].FetchedAt == "" {
		t.Fatalf("checkout details must carry origin/main commit time and FETCH_HEAD time:\n%s", result.out)
	}
}

// TestDriverDoctorReportsFailingRuntime covers problems that persist past
// every grace window: a settled version mismatch, a stale fetch, a day-old
// sync PR, a ledger near its body limit and repeated real launch failures.
func TestDriverDoctorReportsFailingRuntime(t *testing.T) {
	f := newDoctorFixture(t)
	gitIn(t, f.factory, "reset", "--quiet", "--hard", f.previous)
	f.ageFetch(t, f.factory, time.Hour)
	f.syncPR(t, f.previous, f.now.Add(-25*time.Hour))
	t.Setenv("DOCTOR_GH_AUTH", "0")
	f.logLine(t, "scheduler.log", f.now.Add(-time.Hour), `"msg":"command started","component":"scheduler","event":"start","version":"`+f.previous+`"`)
	for i := 0; i < 3; i++ {
		at := f.now.Add(-time.Duration(20-i) * time.Minute)
		f.logLine(t, "launch-ZCP-63.log", at, `"msg":"command started","event":"start"`)
		f.logLine(t, "launch-ZCP-63.log", at.Add(time.Second), `"msg":"command exited","event":"exit","exit_code":1,"err":"worker start uncertain"`)
	}
	probe := f.probe(t, []string{"git", "orca"}, inFlightLedger("ZCP-63", strings.Repeat("x", issues.FeishuBodyRunes-10)))
	probe.serviceEnv = func(context.Context) (map[string]string, error) {
		return map[string]string{"PATH": filepath.Join(f.root, "service-bin"), "AGENT_FEISHU_APP_ID": "TEST-PLACEHOLDER-CRED"}, nil
	}

	result := runDoctorProbe(t, probe)

	result.want(t, 1, map[string]string{"service_path": "fail", "version": "fail", "maintenance": "ok", "checkouts": "fail", "sync_prs": "fail", "ledger": "fail", "launches": "fail", "credentials": "fail"})
}

// TestDriverDoctorReportsStaleMaintenance covers a deploy that never returned
// the service to running.
func TestDriverDoctorReportsStaleMaintenance(t *testing.T) {
	f := newDoctorFixture(t)
	writeDoctorFile(t, filepath.Join(f.root, "state", "maintenance.json"), fmt.Sprintf(`{"mode":"frozen","updatedAt":%q}`, f.now.Add(-61*time.Minute).UTC().Format(time.RFC3339)), 0o600)

	got := f.probe(t, nil, doctorFakeLedger{}).checkMaintenance(maintenance.Read())

	if got.Status != healthFail {
		t.Fatalf("maintenance = %+v, want fail", got)
	}
}

// TestDriverDoctorWarnsOnTransientRuntime covers states a deploy or a fresh
// merge leaves for a while; none of them needs anyone to act yet.
func TestDriverDoctorWarnsOnTransientRuntime(t *testing.T) {
	f := newDoctorFixture(t)
	feature := filepath.Join(f.root, "feature")
	gitIn(t, f.root, "clone", "--quiet", filepath.Join(f.root, "origin.git"), feature)
	gitIn(t, feature, "checkout", "--quiet", "-b", "feat/x", f.previous)
	t.Setenv("GIT_COMMITTER_DATE", f.now.Format(time.RFC3339))
	gitIn(t, f.factory, "commit", "--quiet", "--allow-empty", "-m", "just merged")
	gitIn(t, f.factory, "push", "--quiet", "origin", "HEAD:main")
	gitIn(t, f.factory, "fetch", "--quiet", "origin")
	gitIn(t, f.factory, "reset", "--quiet", "--hard", f.previous)
	f.syncPR(t, f.previous, f.now.Add(-time.Hour))
	t.Setenv("DOCTOR_GH_AUTH", "0")
	writeDoctorFile(t, filepath.Join(f.root, "state", "maintenance.json"), fmt.Sprintf(`{"mode":"draining","updatedAt":%q}`, f.now.Add(-10*time.Minute).UTC().Format(time.RFC3339)), 0o600)
	f.logLine(t, "scheduler.log", f.now.Add(-time.Hour), `"msg":"command started","component":"scheduler","event":"start","version":"`+f.previous+`"`)
	for i, err := range []string{"maintenance refused new claim or dispatch", "launch deferred", "recovery evidence changed before launch"} {
		at := f.now.Add(-time.Duration(20-i) * time.Minute)
		f.logLine(t, "launch-OF-2.log", at, `"msg":"command started","event":"start"`)
		f.logLine(t, "launch-OF-2.log", at.Add(time.Second), `"msg":"command exited","event":"exit","exit_code":1,"err":"`+err+`"`)
	}
	probe := f.probe(t, []string{"go", "gh", "git", "orca", "codex"}, inFlightLedger("OF-2", "", "", "进度"))
	probe.projects = func(context.Context) (projects.Result, error) {
		return projects.Result{Eligible: []projects.Project{{Path: f.factory, Prefix: "OF", Name: "orca-factory", Repository: f.repository}, {Path: feature, Prefix: "FX", Name: "feature", Repository: f.repository}}}, nil
	}

	draining := runDoctorProbe(t, probe)

	draining.want(t, 0, map[string]string{"service_path": "ok", "version": "warn", "maintenance": "warn", "checkouts": "warn", "sync_prs": "warn", "ledger": "warn", "launches": "warn", "credentials": "ok"})
	if !strings.Contains(string(draining.out), "feat/x") || !strings.Contains(string(draining.out), "2 条空正文") {
		t.Fatalf("checkouts must name the branch and ledger the empty-body count:\n%s", draining.out)
	}

	writeDoctorFile(t, filepath.Join(f.root, "state", "maintenance.json"), `{"mode":"running"}`, 0o600)
	runDoctorProbe(t, probe).want(t, 0, map[string]string{"version": "warn", "maintenance": "ok"})
}

func TestDriverDoctorReportsUnknownForUnreachableDependencies(t *testing.T) {
	f := newDoctorFixture(t)
	t.Setenv("DOCTOR_GH_PRS", filepath.Join(f.root, "missing.json"))
	t.Setenv("DOCTOR_GH_AUTH", "1")
	f.logLine(t, "scheduler.log", f.now.Add(-time.Hour), `"msg":"command started","component":"scheduler","event":"start","version":"`+f.current+`"`)
	probe := f.probe(t, []string{"go", "gh", "git", "orca", "claude"}, doctorFakeLedger{})
	probe.ledger = func(context.Context, projects.Project) (doctorLedger, error) {
		return nil, errors.New("feishu unreachable")
	}

	unreachable := runDoctorProbe(t, probe)

	unreachable.want(t, 0, map[string]string{"service_path": "ok", "sync_prs": "unknown", "ledger": "unknown", "credentials": "unknown"})
	if len(unreachable.errors) != 3 {
		t.Fatalf("errors = %v, want the three unreadable probes", unreachable.errors)
	}

	probe.serviceEnv = func(context.Context) (map[string]string, error) { return nil, errors.New("systemctl unreachable") }
	probe.projects = func(context.Context) (projects.Result, error) {
		return projects.Result{}, errors.New("orca unreachable")
	}
	runDoctorProbe(t, probe).want(t, 0, map[string]string{"service_path": "unknown", "checkouts": "unknown", "sync_prs": "unknown", "ledger": "unknown", "credentials": "unknown"})
}

// tokenInvalidFixture is a healthy runtime whose gh reports the stored token
// invalid, as after a reboot with the login keyring still locked (OF-138).
func tokenInvalidFixture(t *testing.T) doctorProbe {
	t.Helper()
	f := newDoctorFixture(t)
	writeDoctorFile(t, filepath.Join(f.root, "prs.json"), `[]`, 0o600)
	t.Setenv("DOCTOR_GH_PRS", filepath.Join(f.root, "prs.json"))
	t.Setenv("DOCTOR_GH_AUTH", "1")
	t.Setenv("DOCTOR_GH_AUTH_STDERR", "github.com\n  X Failed to log in to github.com account TEST-PLACEHOLDER-CRED (default)\n  - The token in default is invalid.\n")
	f.logLine(t, "scheduler.log", f.now.Add(-time.Hour), `"msg":"command started","component":"scheduler","event":"start","version":"`+f.current+`"`)
	return f.probe(t, []string{"go", "gh", "git", "orca", "claude"}, inFlightLedger("OF-1", "进度"))
}

func TestDriverDoctorReportsUnreadableCredentials(t *testing.T) {
	result := runDoctorProbe(t, tokenInvalidFixture(t))

	result.want(t, 1, map[string]string{"credentials": "fail"})
	out := string(result.out)
	if !strings.Contains(out, "GitHub 凭证不可读（keyring 可能未解锁）") || !strings.Contains(out, "登录桌面会话解锁 login keyring") {
		t.Fatalf("credentials must name the locked keyring and its fix:\n%s", out)
	}
	if strings.Contains(out, "is invalid") || len(result.errors) != 0 {
		t.Fatalf("an unreadable credential is a finding, not raw gh output or an unreadable probe:\n%s", out)
	}
}

func TestDriverDoctorReportsUnknownCredentialsWhenGitHubUnreachable(t *testing.T) {
	probe := tokenInvalidFixture(t)
	probe.githubReachable = func(context.Context) error { return errors.New("dial tcp: network is unreachable") }

	result := runDoctorProbe(t, probe)

	result.want(t, 0, map[string]string{"credentials": "unknown"})
	if len(result.errors) != 1 || !strings.HasPrefix(result.errors[0], "credentials: ") {
		t.Fatalf("errors = %v, want the unreadable credentials probe", result.errors)
	}
}

func TestDoctorReportsOrcaTerminalCreateFailure(t *testing.T) {
	f := newDoctorFixture(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	writeDoctorFile(t, status.PushPath(), fmt.Sprintf(`{"generatedAt":%q,"orcaRuntime":{"paused":true,"failures":3,"pausedAt":"2026-11-19T10:30:00Z","restarts":2},"tickets":[]}`, f.now.Format(time.RFC3339Nano)), 0600)
	assertDoctorReportsOrcaRuntimeFault(t, runDoctorProbe(t, f.probe(t, []string{"go", "gh", "git", "orca", "claude"}, inFlightLedger("OF-1", "进度"))))

	// An explicit DRIVER_STATUS_FILE selects the snapshot serve writes, not the pushed view.
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(t.TempDir(), "status.json"))
	if err := status.Write(status.Snapshot{GeneratedAt: f.now, OrcaRuntime: status.OrcaRuntime{Paused: true, Failures: 3, PausedAt: time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC), Restarts: 2}}); err != nil {
		t.Fatal(err)
	}
	assertDoctorReportsOrcaRuntimeFault(t, runDoctorProbe(t, f.probe(t, []string{"go", "gh", "git", "orca", "claude"}, inFlightLedger("OF-1", "进度"))))
}

func TestDoctorReportsOrcaUnreachableAfterGrace(t *testing.T) {
	for _, source := range []string{"push", "snapshot"} {
		for _, age := range []time.Duration{10*time.Minute - time.Second, 10 * time.Minute} {
			t.Run(fmt.Sprintf("%s/%s", source, age), func(t *testing.T) {
				f := newDoctorFixture(t)
				t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
				t.Setenv("DRIVER_STATUS_FILE", "")
				path := status.PushPath()
				if source == "snapshot" {
					path = filepath.Join(t.TempDir(), "status.json")
					t.Setenv("DRIVER_STATUS_FILE", path)
				}
				since := f.now.Add(-age)
				writeDoctorFile(t, path, fmt.Sprintf(`{"generatedAt":%q,"orcaRuntime":{"unreachableSince":%q,"restarts":1},"tickets":[]}`, f.now.Format(time.RFC3339Nano), since.Format(time.RFC3339Nano)), 0600)
				result := runDoctorProbe(t, f.probe(t, []string{"go", "gh", "git", "orca", "claude"}, inFlightLedger("OF-1", "进度")))
				want := "ok"
				if age == 10*time.Minute {
					want = "fail"
				}
				if result.statuses["orca_runtime"] != want {
					t.Fatalf("orca_runtime=%s, want %s: %s", result.statuses["orca_runtime"], want, result.out)
				}
				if want == "fail" {
					detail := string(result.details["orca_runtime"])
					for _, value := range []string{`"unreachable_since":"` + since.Format(time.RFC3339Nano) + `"`, "自动重启", "orca-serve"} {
						if !strings.Contains(detail, value) {
							t.Fatalf("detail missing %q: %s", value, detail)
						}
					}
				}
			})
		}
	}
}

func assertDoctorReportsOrcaRuntimeFault(t *testing.T, result doctorResult) {
	t.Helper()
	if result.statuses["orca_runtime"] != "fail" {
		t.Fatalf("runtime failure not reported: %s", result.out)
	}
	detail := string(result.details["orca_runtime"])
	for _, want := range []string{"orca-serve", "profile", "超时", "自动重启", `"paused_at":"2026-11-19T10:30:00Z"`, `"restarts":2`} {
		if !strings.Contains(detail, want) {
			t.Fatalf("missing recovery advice %q: %s", want, detail)
		}
	}
}

func TestOrcaRuntimeCapabilitiesSurviveStatusBackend(t *testing.T) {
	backend := &statusBackend{Backend: &scheduler.Runtime{}}
	if _, ok := any(backend).(interface {
		ProbeOrcaRuntime(context.Context, string) error
	}); !ok {
		t.Error("serve statusBackend hides Orca runtime recovery probe")
	}
	if _, ok := any(backend).(interface {
		NotifyOrcaRuntime(context.Context, scheduler.Ticket) error
	}); !ok {
		t.Error("serve statusBackend hides Orca runtime failure notification")
	}
	if _, ok := any(backend).(interface {
		RestartOrcaService(context.Context) error
		NotifyOrcaRestart(context.Context, scheduler.Ticket, int, error) error
		NotifyOrcaRestartExhausted(context.Context, scheduler.Ticket) error
	}); !ok {
		t.Error("serve statusBackend hides the Orca service restart or its notifications")
	}
	if _, ok := any(backend).(interface{ OrcaTerminalCreates() (int, int) }); !ok {
		t.Error("serve statusBackend hides the Orca terminal create counts")
	}
}

func TestDoctorModels(t *testing.T) {
	now := time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, state, want string }{
		{"healthy", "ok", "ok"}, {"one worker tier unavailable", "unavailable", "warn"}, {"unknown", "unknown", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := map[string]int{}
			var mu sync.Mutex
			p := modelFixtureProbe(now, t.TempDir())
			p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
				mu.Lock()
				calls[ref.Key()]++
				mu.Unlock()
				state := modelhealth.OK
				if ref.Model == "primary" {
					state = tc.state
				}
				return modelhealth.Status{Ref: ref, State: state, Reason: "test", CheckedAt: now, Until: now.Add(time.Hour)}
			}
			eligible := []projects.Project{{Name: "one", Repository: config.Repository{Env: map[string]string{
				"AGENT_TIER_DEFAULT": "codex:primary:high", "AGENT_TIER_STRONG": "claude:backup:high",
				"AGENT_ROLE_REVIEWER": "claude:backup:low", "AGENT_TIER_ESCALATE": "1", "AGENT_TIER_CHAIN": "default strong max", "AGENT_TIER_MAX": "claude:backup:max",
				"AGENT_ROLE_RESOLVER_TOP": "claude:backup:max", "AGENT_ROLE_INTAKE": "claude:backup:high",
			}}}}
			got := p.checkModels(context.Background(), eligible, nil)
			if got.Status != tc.want {
				t.Fatalf("check = %+v", got)
			}
			// The reviewer inherits the service environment; intake runs with only HOME and PATH.
			if calls["codex:primary"] != 1 || calls["claude:backup"] != 2 || len(calls) != 2 {
				t.Fatalf("probe calls = %v", calls)
			}
			rows := got.Details.([]doctorModelStatus)
			found := map[string]bool{}
			for _, row := range rows {
				found[row.Role] = true
				if row.Repository != "one" {
					t.Fatalf("row = %+v", row)
				}
			}
			for _, role := range []string{"worker_default", "worker_strong", "worker_max", "reviewer", "resolver", "resolver_top", "intake"} {
				if !found[role] {
					t.Fatalf("missing %s: %+v", role, rows)
				}
			}
			var out bytes.Buffer
			report := healthReport{Checks: map[string]healthCheck{"models": got}}
			if err := writeHealth(&out, report, false, []string{"models"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "one") || !strings.Contains(out.String(), "codex:primary:high") {
				t.Fatalf("text omits models: %s", out.String())
			}
		})
	}
}

func TestDoctorModelsProbesDuringCooldown(t *testing.T) {
	now := time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC)
	root := t.TempDir()
	r, err := modelhealth.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ref := modelhealth.Ref{Agent: "claude", Model: "primary", Effort: "high"}
	fixture, err := os.ReadFile("../../testdata/recorded/claude-model-limit.txt")
	if err != nil {
		t.Fatal(err)
	}
	limited := modelhealth.Classify(ref, string(fixture), nil, now)
	if err := r.Record(limited); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := modelFixtureProbe(now.Add(time.Minute), root)
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		calls.Add(1)
		return modelhealth.Classify(ref, "ok", nil, p.now)
	}
	eligible := []projects.Project{{Name: "one", Repository: config.Repository{Env: map[string]string{
		"AGENT_TIER_DEFAULT": ref.String(), "AGENT_ROLE_RESOLVER": ref.String(), "AGENT_ROLE_REVIEWER": ref.String(), "AGENT_ROLE_INTAKE": ref.String(),
	}}}}
	got := p.checkModels(context.Background(), eligible, nil)
	if got.Status != "ok" || calls.Load() == 0 {
		t.Fatalf("cooldown = %+v, calls=%d", got, calls.Load())
	}
	reopened, err := modelhealth.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if state := reopened.Status(ref, p.now).State; state != modelhealth.OK {
		t.Fatalf("registry after fresh ok = %s", state)
	}
	if err := os.WriteFile(filepath.Join(root, "model-health.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	calls.Store(0)
	got = p.checkModels(context.Background(), eligible, nil)
	if got.Status != "unknown" || !got.unprobed || calls.Load() != 0 {
		t.Fatalf("corrupt registry = %+v", got)
	}
}

func assertDoctorModel(t *testing.T, rows []doctorModelStatus, repo, role, ref, state, reason string) {
	t.Helper()
	count := 0
	for _, row := range rows {
		if row.Repository == repo && row.Role == role {
			count++
			if row.Status.Ref.String() != ref || row.Status.State != state || row.Status.Reason != reason {
				t.Errorf("%s/%s = %+v, want %s %s/%s", repo, role, row.Status, ref, state, reason)
			}
		}
	}
	if count != 1 {
		t.Errorf("found %d rows for %s/%s, want 1: %+v", count, repo, role, rows)
	}
}

func TestDoctorModelsProbesReviewerEndpoint(t *testing.T) {
	const proxy = "https://proxy.invalid/TEST-PLACEHOLDER-CRED"
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_MODEL_PROXY_URL", proxy)
	t.Setenv("AGENT_INTAKE_PI", "")
	t.Setenv("AGENT_INTAKE_CLAUDE", "/opt/test/claude")
	p, err := newDoctorProbe()
	if err != nil {
		t.Fatal(err)
	}
	p.stateRoot = t.TempDir()
	var mu sync.Mutex
	calls := map[string]int{}
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, endpoint modelhealth.Endpoint) modelhealth.Status {
		mu.Lock()
		defer mu.Unlock()
		if ref.Key() != "claude:m" {
			t.Errorf("unexpected model %s", ref)
		}
		calls[endpoint.BaseURL]++
		if endpoint.BaseURL == proxy {
			return modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: p.now, Until: p.now.Add(time.Hour)}
		}
		return modelhealth.Classify(ref, "ok", nil, p.now)
	}
	project := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
		"AGENT_TIER_DEFAULT": "claude:m:high", "AGENT_ROLE_RESOLVER": "claude:m:low",
		"AGENT_ROLE_REVIEWER": "claude:m:low", "AGENT_ROLE_REVIEWER_SMALL": "claude:m:high", "AGENT_ROLE_INTAKE": "claude:m:low",
	}}}
	got := p.checkModels(context.Background(), []projects.Project{project}, nil)
	if got.Status != "fail" || !reflect.DeepEqual(calls, map[string]int{"": 2, proxy: 1}) {
		t.Errorf("check = %s, calls = %v, want fail with inherited, isolated intake and proxy probes", got.Status, calls)
	}
	rows := got.Details.([]doctorModelStatus)
	assertDoctorModel(t, rows, "one", "worker_default", "claude:m:high", "ok", "probe_ok")
	assertDoctorModel(t, rows, "one", "resolver", "claude:m:low", "ok", "probe_ok")
	assertDoctorModel(t, rows, "one", "reviewer", "claude:m:low", "unavailable", "usage_limit")
	assertDoctorModel(t, rows, "one", "reviewer_small", "claude:m:high", "unavailable", "usage_limit")
	assertDoctorModel(t, rows, "one", "intake", "claude:m:low", "ok", "probe_ok")
	data, err := os.ReadFile(filepath.Join(p.stateRoot, "model-health.json"))
	if err != nil {
		t.Fatal(err)
	}
	var records map[string]modelhealth.Status
	if err := json.Unmarshal(data, &records); err != nil || len(records) != 1 || records["claude:m"].State != "unavailable" {
		t.Errorf("registry = %s, want one unavailable claude:m record, err = %v", data, err)
	}
	report, err := json.Marshal(got)
	if err != nil || bytes.Contains(report, []byte(proxy)) || bytes.Contains(data, []byte(proxy)) {
		t.Fatal("proxy URL in model health report or registry", err)
	}
	p.now = p.now.Add(time.Minute)
	rows = p.checkModels(context.Background(), []projects.Project{project}, nil).Details.([]doctorModelStatus)
	if !reflect.DeepEqual(calls, map[string]int{"": 4, proxy: 2}) {
		t.Errorf("calls during cooldown = %v, want every endpoint probed again", calls)
	}
	assertDoctorModel(t, rows, "one", "worker_default", "claude:m:high", "ok", "probe_ok")
	assertDoctorModel(t, rows, "one", "reviewer", "claude:m:low", "unavailable", "usage_limit")
	p.now = p.now.Add(time.Minute)
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, endpoint modelhealth.Endpoint) modelhealth.Status {
		if endpoint.BaseURL == proxy {
			return modelhealth.Classify(ref, "", context.DeadlineExceeded, p.now)
		}
		return modelhealth.Classify(ref, "ok", nil, p.now)
	}
	rows = p.checkModels(context.Background(), []projects.Project{project}, nil).Details.([]doctorModelStatus)
	assertDoctorModel(t, rows, "one", "reviewer", "claude:m:low", "unavailable", "usage_limit")
	reopened, err := modelhealth.Open(p.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Status(modelhealth.Ref{Agent: "claude", Model: "m"}, p.now); got.State != modelhealth.Unavailable {
		t.Errorf("registry after worker ok and proxy timeout = %+v, want the cooldown kept", got)
	}
}

func TestDoctorModelsRoleEndpoints(t *testing.T) {
	home := t.TempDir()
	writeDoctorFile(t, filepath.Join(home, ".config", "orca-factory", "driver.env"), "AGENT_INTAKE_PI=anthropic/fable:high\n", 0o600)
	t.Setenv("HOME", home)
	t.Setenv("AGENT_MODEL_PROXY_URL", "")
	t.Setenv("AGENT_INTAKE_PI", " ")
	project := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
		"AGENT_TIER_DEFAULT": "codex:worker:high", "AGENT_ROLE_RESOLVER": "codex:resolver:high",
		"AGENT_ROLE_REVIEWER": "claude:reviewer:high", "AGENT_ROLE_SECOND_REVIEWER": "claude:second:high", "AGENT_ROLE_INTAKE": "claude:intake:low",
	}}}
	bin := t.TempDir()
	writeDoctorFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\n", 0o755)
	t.Setenv("PATH", bin)
	for _, tc := range []struct{ name, intakeClaude, intake, reason string }{
		{"absolute", "/opt/intake/claude", "intake|/opt/intake/claude|true", "probe_ok"},
		{"default on PATH", "", "intake||true", "probe_ok"},
		{"relative", "claude", "", "config_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_INTAKE_CLAUDE", tc.intakeClaude)
			p, err := newDoctorProbe()
			if err != nil {
				t.Fatal(err)
			}
			p.stateRoot = t.TempDir()
			var mu sync.Mutex
			var endpoints []string
			p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, endpoint modelhealth.Endpoint) modelhealth.Status {
				mu.Lock()
				defer mu.Unlock()
				endpoints = append(endpoints, fmt.Sprintf("%s|%s|%t", ref.Model, endpoint.Binary, endpoint.Isolated))
				return modelhealth.Classify(ref, "ok", nil, p.now)
			}
			got := p.checkModels(context.Background(), []projects.Project{project}, nil)
			want := []string{"resolver||false", "reviewer||false", "reviewer||true", "second||true", "worker||false"}
			if tc.intake != "" {
				want = append(want, tc.intake)
			}
			slices.Sort(endpoints)
			slices.Sort(want)
			if !slices.Equal(endpoints, want) {
				t.Errorf("endpoints = %v, want %v", endpoints, want)
			}
			rows := got.Details.([]doctorModelStatus)
			intakeState := "ok"
			if tc.reason == "config_invalid" {
				intakeState = "unknown"
			}
			assertDoctorModel(t, rows, "one", "intake", "claude:intake:low", intakeState, tc.reason)
			assertDoctorModel(t, rows, "one", "cross_reviewer", "claude:reviewer:high", "ok", "probe_ok")
		})
	}
}

func TestDoctorModelsReportsUnloadedEnabledRepository(t *testing.T) {
	p := modelFixtureProbe(time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC), t.TempDir())
	rejected := []projects.Rejection{
		{Path: "/repos/broken", Remote: "github.com/o/broken", Reason: projects.ReasonCheckFailed},
		{Path: "/repos/plain", Remote: "github.com/o/plain", Reason: projects.ReasonOnboardable},
		{Path: "/repos/off", Remote: "github.com/o/off", Reason: projects.ReasonCheckFailed},
		{Path: "/repos/stray", Remote: "github.com/o/stray", Reason: projects.ReasonCheckFailed},
	}
	ledger := []issues.LedgerProject{{Remote: "github.com/o/broken", Enabled: true}, {Remote: "github.com/o/off", Enabled: true}, {Remote: "github.com/o/off"}}
	if got := unloadedRepositories(rejected, ledger); len(got) != 1 || got[0].Remote != "github.com/o/broken" {
		t.Errorf("with a readable ledger = %+v, want only the enabled broken repository", got)
	}
	if got := unloadedRepositories(rejected, nil); len(got) != 3 {
		t.Errorf("without a ledger = %+v, want every failed check", got)
	}
	got := p.checkModels(context.Background(), nil, unloadedRepositories(rejected, ledger))
	if got.Status != "fail" {
		t.Errorf("check = %+v, want fail for an enabled repository whose profile does not load", got)
	}
	assertDoctorModel(t, got.Details.([]doctorModelStatus), "github.com/o/broken", "profile", "::", "unknown", "config_invalid")
}

func TestDoctorModelsKeepsResultsWhenRegistryWriteFails(t *testing.T) {
	p := modelFixtureProbe(time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC), t.TempDir())
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		return modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: p.now, Until: p.now.Add(time.Hour)}
	}
	lock, err := flock.TryLock(filepath.Join(p.stateRoot, "model-health.json.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	project := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
		"AGENT_TIER_DEFAULT": "claude:m:high", "AGENT_ROLE_REVIEWER": "claude:m:low",
		"AGENT_ROLE_RESOLVER": "claude:m:high", "AGENT_ROLE_INTAKE": "claude:m:low",
	}}}
	got := p.checkModels(context.Background(), []projects.Project{project}, nil)
	if got.Status != "fail" || !got.unprobed || !strings.Contains(got.Reason, "登记无法写入") {
		t.Fatalf("check = %+v, want fail that also reports the registry write", got)
	}
	assertDoctorModel(t, got.Details.([]doctorModelStatus), "one", "reviewer", "claude:m:low", "unavailable", "usage_limit")
}

func TestDoctorModelsRecordsLongestEndpointCooldown(t *testing.T) {
	const proxy = "https://proxy.invalid"
	for _, proxyLonger := range []bool{true, false} {
		now := time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC)
		p := modelFixtureProbe(now, t.TempDir())
		p.modelSettings = func() doctorModelSettings { return doctorModelSettings{proxyURL: proxy} }
		p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, endpoint modelhealth.Endpoint) modelhealth.Status {
			until := now.Add(30 * time.Minute)
			if (endpoint.BaseURL == proxy) == proxyLonger {
				until = now.Add(48 * time.Hour)
			}
			return modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: now, Until: until}
		}
		project := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
			"AGENT_TIER_DEFAULT": "claude:m:high", "AGENT_ROLE_REVIEWER": "claude:m:low",
			"AGENT_ROLE_RESOLVER": "claude:m:high", "AGENT_ROLE_INTAKE": "claude:m:low",
		}}}
		p.checkModels(context.Background(), []projects.Project{project}, nil)
		r, err := modelhealth.Open(p.stateRoot)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Status(modelhealth.Ref{Agent: "claude", Model: "m"}, now); !got.Until.Equal(now.Add(48 * time.Hour)) {
			t.Errorf("proxyLonger=%v recorded until %v, want the longest cooldown", proxyLonger, got.Until)
		}
	}
}

func TestDoctorModelsWithoutRecording(t *testing.T) {
	p := modelFixtureProbe(time.Now(), t.TempDir())
	p.recordModels = false
	project := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
		"AGENT_TIER_DEFAULT": "claude:m:high", "AGENT_ROLE_REVIEWER": "claude:m:low",
		"AGENT_ROLE_RESOLVER": "claude:m:high", "AGENT_ROLE_INTAKE": "claude:m:low",
	}}}
	got := p.checkModels(context.Background(), []projects.Project{project}, nil)
	if got.Status != "ok" {
		t.Errorf("check = %+v", got)
	}
	rows := got.Details.([]doctorModelStatus)
	assertDoctorModel(t, rows, "one", "worker_default", "claude:m:high", "ok", "probe_ok")
	assertDoctorModel(t, rows, "one", "reviewer", "claude:m:low", "ok", "probe_ok")
	if _, err := os.Stat(filepath.Join(p.stateRoot, "model-health.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat model-health.json = %v, want file absent", err)
	}
}

func TestDoctorModelsInvalidProxyReportsAllChecks(t *testing.T) {
	f := newDoctorFixture(t)
	t.Setenv("HOME", f.root)
	t.Setenv("AGENT_MODEL_PROXY_URL", "")
	t.Setenv("AGENT_INTAKE_PI", "")
	t.Setenv("AGENT_INTAKE_CLAUDE", "/opt/test/claude")
	writeDoctorFile(t, filepath.Join(f.root, ".config/orca-factory/driver.env"), "AGENT_MODEL_PROXY_URL=ftp://x\n", 0600)
	machine, err := newDoctorProbe()
	if err != nil {
		t.Fatalf("constructing doctor with an invalid reviewer proxy: %v", err)
	}
	f.repository.Env["AGENT_ROLE_REVIEWER"] = "claude:review:low"
	f.repository.Env["AGENT_ROLE_REVIEWER_SMALL"] = "claude:small:high"
	probe := f.probe(t, doctorServiceTools, doctorFakeLedger{})
	probe.modelSettings = machine.modelSettings
	probe.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		if ref.Model == "review" || ref.Model == "small" {
			t.Errorf("probed reviewer with invalid proxy: %s", ref)
		}
		return modelhealth.Classify(ref, "ok", nil, f.now)
	}
	result := runDoctorProbe(t, probe)
	result.want(t, 1, map[string]string{"models": "fail"})
	for _, name := range doctorChecks {
		if result.statuses[name] == "" {
			t.Errorf("doctor JSON missing %s", name)
		}
	}
	if bytes.Contains(result.out, []byte("ftp://x")) || !strings.Contains(result.reasons["models"], "执行器无法启动") {
		t.Errorf("expected a redacted executor startup diagnosis: %s", result.out)
	}
	var rows []doctorModelStatus
	if err := json.Unmarshal(result.details["models"], &rows); err != nil {
		t.Fatal(err)
	}
	assertDoctorModel(t, rows, "orca-factory", "reviewer", "claude:review:low", "unknown", "model_proxy_invalid")
	assertDoctorModel(t, rows, "orca-factory", "reviewer_small", "claude:small:high", "unknown", "model_proxy_invalid")
	assertDoctorModel(t, rows, "orca-factory", "worker_default", "codex:m:high", "ok", "probe_ok")
	assertDoctorModel(t, rows, "orca-factory", "intake", "claude:m:high", "unknown", "model_relay_invalid")
}

func TestDoctorModelsConcurrent(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	entered := 0
	var batchDeadline time.Time
	ready := make(chan struct{})
	p := modelFixtureProbe(now, t.TempDir())
	p.modelProbe = func(ctx context.Context, ref modelhealth.Ref, dir string, _ modelhealth.Endpoint) modelhealth.Status {
		if dir != "/first" {
			t.Errorf("probe directory = %q, want first repository", dir)
		}
		mu.Lock()
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) > modelhealth.ProbeTimeout {
			t.Error("probe batch missing its time limit")
		}
		if entered == 0 {
			batchDeadline = deadline
		} else if deadline != batchDeadline {
			t.Error("probes in a batch must share a deadline")
		}
		entered++
		if entered == 4 {
			close(ready)
		}
		mu.Unlock()
		select {
		case <-ready:
			return modelhealth.Classify(ref, "ok", nil, now)
		case <-time.After(2 * time.Second):
			return modelhealth.Status{Ref: ref, State: modelhealth.Unknown, Reason: "serial", CheckedAt: now}
		}
	}
	repo := config.Repository{Env: map[string]string{
		"AGENT_TIER_DEFAULT": "codex:a:high", "AGENT_ROLE_REVIEWER": "claude:b:low", "AGENT_ROLE_RESOLVER": "codex:c:high", "AGENT_ROLE_INTAKE": "claude:b:low",
	}}
	got := p.checkModels(context.Background(), []projects.Project{{Name: "first", Path: "/first", Repository: repo}, {Name: "second", Path: "/second", Repository: repo}}, nil)
	if got.Status != "ok" || entered != 4 {
		t.Fatalf("check = %+v, calls = %d", got, entered)
	}
	rows := got.Details.([]doctorModelStatus)
	if len(rows) != 8 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, row := range rows {
		if row.Status.State != "ok" {
			t.Fatalf("row = %+v", row)
		}
	}
}

func TestDoctorRunPersistsModelCooldown(t *testing.T) {
	f := newDoctorFixture(t)
	f.repository.Env["AGENT_TIER_DEFAULT"] = "claude:m:high"
	f.repository.Env["AGENT_ROLE_REVIEWER"] = "claude:m:high"
	f.repository.Env["AGENT_ROLE_RESOLVER"] = "claude:m:low"
	p := f.probe(t, doctorServiceTools, doctorFakeLedger{})
	var calls atomic.Int32
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		calls.Add(1)
		return modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: f.now, Until: f.now.Add(time.Hour)}
	}
	got := p.run(context.Background()).Checks["models"]
	if got.Status != "fail" || calls.Load() != 2 {
		t.Fatalf("first run = %+v, probes = %d, want the shared endpoint and isolated intake", got, calls.Load())
	}
	for _, row := range got.Details.([]doctorModelStatus) {
		if row.Status.State != "unavailable" {
			t.Fatalf("fresh probe missing from row: %+v", row)
		}
	}
	if _, err := os.Stat(filepath.Join(p.stateRoot, "model-health.json")); err != nil {
		t.Error(err)
	}
	calls.Store(0)
	p.now = f.now.Add(time.Minute)
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		calls.Add(1)
		return modelhealth.Classify(ref, "", context.DeadlineExceeded, p.now)
	}
	got = p.run(context.Background()).Checks["models"]
	if got.Status != "fail" || calls.Load() == 0 {
		t.Errorf("second run = %+v, probes during cooldown = %d, want the cooldown to keep failing", got, calls.Load())
	}
	for _, row := range got.Details.([]doctorModelStatus) {
		if row.Status.State != "unavailable" || row.Status.Reason != "usage_limit" {
			t.Errorf("inconclusive probe dropped the cooldown: %+v", row)
		}
	}
	r, err := modelhealth.Open(p.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Status(modelhealth.Ref{Agent: "claude", Model: "m"}, p.now); got.State != modelhealth.Unavailable {
		t.Errorf("registry after timeout = %+v, want the cooldown kept", got)
	}
}

func TestDoctorModelsReachableWorkerTiers(t *testing.T) {
	for _, tc := range []struct{ escalate, chain, ref, reason string }{
		{"1", "default", "codex:default:low", "usage_limit"},
		{"0", "default max", "codex:default:low", "usage_limit"},
		{"1", "max default", "codex:default:low", "usage_limit"},
		{"1", "default default", "::", "config_invalid"},
		{"0", "default default", "::", "config_invalid"},
		{"1", "max", "::", "config_invalid"},
	} {
		t.Run(tc.escalate+"_"+tc.chain, func(t *testing.T) {
			now := time.Now()
			p := modelFixtureProbe(now, t.TempDir())
			p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
				if ref.Model == "max" {
					t.Errorf("unexpected probe of %s", ref.Model)
				}
				if ref.Model == "default" {
					return modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", Until: now.Add(time.Hour), CheckedAt: now}
				}
				return modelhealth.Classify(ref, "ok", nil, now)
			}
			project := projects.Project{Repository: config.Repository{Env: map[string]string{
				"AGENT_TIER_ESCALATE": tc.escalate, "AGENT_TIER_CHAIN": tc.chain,
				"AGENT_TIER_DEFAULT": "codex:default:low", "AGENT_TIER_MAX": "codex:max:high", "AGENT_ROLE_REVIEWER": "claude:m:low", "AGENT_ROLE_INTAKE": "claude:m:low",
			}}}
			got := p.checkModels(context.Background(), []projects.Project{project}, nil)
			if got.Status != "fail" {
				t.Errorf("check = %+v", got)
			}
			state := "unavailable"
			if tc.reason == "config_invalid" {
				state = "unknown"
			}
			rows, _ := got.Details.([]doctorModelStatus)
			assertDoctorModel(t, rows, "", "worker_default", tc.ref, state, tc.reason)
			assertDoctorModel(t, rows, "", "reviewer", "claude:m:low", "ok", "probe_ok")
			for _, row := range rows {
				if row.Role == "worker_max" {
					t.Fatal("listed unreachable worker_max")
				}
			}
		})
	}
}

func TestDoctorModelsWorkerCorrectionLimit(t *testing.T) {
	now := time.Now()
	p := modelFixtureProbe(now, t.TempDir())
	env := map[string]string{
		"AGENT_TIER_ESCALATE": "1", "AGENT_TIER_CHAIN": "default a b c d",
		"AGENT_ROLE_REVIEWER": "claude:m:low", "AGENT_ROLE_RESOLVER": "claude:m:low", "AGENT_ROLE_INTAKE": "claude:m:low",
	}
	for _, tier := range []string{"default", "a", "b", "c", "d"} {
		env["AGENT_TIER_"+strings.ToUpper(tier)] = modelhealth.Ref{Agent: "codex", Model: tier, Effort: "high"}.String()
	}
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		if ref.Agent != "codex" {
			return modelhealth.Classify(ref, "ok", nil, now)
		}
		if ref.Model == "d" {
			t.Errorf("unexpected probe beyond the last correction: %s", ref)
		}
		return modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: now, Until: now.Add(time.Hour)}
	}
	got := p.checkModels(context.Background(), []projects.Project{{Name: "one", Repository: config.Repository{Env: env}}}, nil)
	if got.Status != "fail" {
		t.Errorf("check = %s, want fail with all reachable workers cooling down", got.Status)
	}
	rows := got.Details.([]doctorModelStatus)
	for _, tier := range []string{"default", "a", "b", "c"} {
		assertDoctorModel(t, rows, "one", "worker_"+tier, "codex:"+tier+":high", "unavailable", "usage_limit")
	}
	for _, row := range rows {
		if row.Role == "worker_d" {
			t.Error("listed worker_d beyond the last correction")
		}
	}
}

func TestDoctorModelsPiIntake(t *testing.T) {
	for _, tc := range []struct{ name, repository, process, file, claude, ref, status, reason string }{
		{"repository wins", " pi:anthropic/m:high ", "pi:other/process:low", "", "claude:x:high", "pi:anthropic/m:high", "unknown", "unsupported_agent"},
		{"process wins", "", " pi:anthropic/m:high ", "pi:other/file:low", "claude:x:high", "pi:anthropic/m:high", "unknown", "unsupported_agent"},
		{"machine file only", " ", "", "pi:anthropic/m:high", "", "::", "fail", "not_configured"},
		{"machine file with claude tier", "", "", "pi:anthropic/m:high", "claude:x:high", "pi:anthropic/m:high", "unknown", "unsupported_agent"},
		{"invalid repository tier", "pi:missing-provider", "", "", "claude:x:high", "pi:missing-provider:", "fail", "config_invalid"},
		{"non-claude intake", "", "", "", "codex:x:high", "codex:x:high", "fail", "config_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("AGENT_MODEL_PROXY_URL", "")
			t.Setenv("AGENT_INTAKE_PI", tc.process)
			t.Setenv("AGENT_INTAKE_CLAUDE", "/opt/test/claude")
			writeDoctorFile(t, filepath.Join(home, ".config/orca-factory/driver.env"), "AGENT_INTAKE_PI="+tc.file+"\n", 0600)
			p, err := newDoctorProbe()
			if err != nil {
				t.Fatal(err)
			}
			p.stateRoot = t.TempDir()
			p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
				if ref.Agent == "pi" || ref.Model == "x" {
					t.Errorf("unexpected intake probe %s", ref)
				}
				return modelhealth.Classify(ref, "ok", nil, p.now)
			}
			project := projects.Project{Repository: config.Repository{Env: map[string]string{
				"AGENT_INTAKE_PI": tc.repository, "AGENT_ROLE_INTAKE": tc.claude,
				"AGENT_TIER_DEFAULT": "codex:m:high", "AGENT_ROLE_REVIEWER": "codex:m:high",
			}}}
			got := p.checkModels(context.Background(), []projects.Project{project}, nil)
			if got.Status != tc.status {
				t.Errorf("check = %+v", got)
			}
			rows, _ := got.Details.([]doctorModelStatus)
			assertDoctorModel(t, rows, "", "intake", tc.ref, "unknown", tc.reason)
			assertDoctorModel(t, rows, "", "reviewer", "codex:m:high", "ok", "probe_ok")
			assertDoctorModel(t, rows, "", "worker_default", "codex:m:high", "ok", "probe_ok")
			assertDoctorModel(t, rows, "", "resolver", "codex::", "ok", "probe_ok")
		})
	}
}

func TestDoctorModelsInvalidResolverTiers(t *testing.T) {
	for _, tc := range []struct{ key, role, value string }{
		{"AGENT_ROLE_RESOLVER", "resolver", "pi:provider/model:low"},
		{"AGENT_ROLE_RESOLVER", "resolver", "claude:a:high:extra"},
		{"AGENT_ROLE_RESOLVER_TOP", "resolver_top", "claude:m:"},
		{"AGENT_ROLE_RESOLVER_TOP", "resolver_top", "codex::high"},
	} {
		t.Run(tc.role+"/"+tc.value, func(t *testing.T) {
			now := time.Now()
			p := modelFixtureProbe(now, t.TempDir())
			p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
				if ref.String() == tc.value {
					t.Errorf("probed invalid %s: %s", tc.role, ref)
				}
				return modelhealth.Classify(ref, "ok", nil, now)
			}
			env := map[string]string{"AGENT_TIER_DEFAULT": "codex:worker:low", "AGENT_ROLE_REVIEWER": "codex:worker:low", "AGENT_ROLE_INTAKE": "claude:intake:low", tc.key: tc.value}
			got := p.checkModels(context.Background(), []projects.Project{{Name: "A", Repository: config.Repository{Env: env}}}, nil)
			if got.Status != "fail" {
				t.Errorf("check = %+v", got)
			}
			rows, _ := got.Details.([]doctorModelStatus)
			assertDoctorModel(t, rows, "A", tc.role, tc.value, "unknown", "config_invalid")
			assertDoctorModel(t, rows, "A", "worker_default", "codex:worker:low", "ok", "probe_ok")
		})
	}
}

func TestDoctorModelsIsolatesRepositoryConfigErrors(t *testing.T) {
	now := time.Now()
	p := modelFixtureProbe(now, t.TempDir())
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		if ref.Model == "cooling" {
			return modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: now, Until: now.Add(time.Hour)}
		}
		return modelhealth.Classify(ref, "ok", nil, now)
	}
	ref := modelhealth.Ref{Agent: "claude", Model: "cooling", Effort: "low"}
	got := p.checkModels(context.Background(), []projects.Project{
		{Name: "A", Repository: config.Repository{Env: map[string]string{
			"AGENT_TIER_DEFAULT": "codex:w:low", "AGENT_ROLE_REVIEWER": "codex:w:low",
			"AGENT_ROLE_INTAKE": "claude:m:high", "AGENT_INTAKE_PI": "pi:broken",
		}}},
		{Name: "B", Repository: config.Repository{Env: map[string]string{"AGENT_TIER_DEFAULT": "codex:w:low", "AGENT_ROLE_REVIEWER": ref.String(), "AGENT_ROLE_INTAKE": "claude:m:high"}}},
	}, nil)
	if got.Status != "fail" {
		t.Errorf("check = %+v", got)
	}
	rows, _ := got.Details.([]doctorModelStatus)
	assertDoctorModel(t, rows, "A", "intake", "pi:broken:", "unknown", "config_invalid")
	assertDoctorModel(t, rows, "A", "reviewer", "codex:w:low", "ok", "probe_ok")
	assertDoctorModel(t, rows, "B", "reviewer", "claude:cooling:low", "unavailable", "usage_limit")
}

func TestDoctorModelsUnreadablePiSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_INTAKE_PI", "")
	t.Setenv("AGENT_MODEL_PROXY_URL", "https://proxy.invalid")
	if err := os.MkdirAll(filepath.Join(home, ".config/orca-factory/driver.env"), 0700); err != nil {
		t.Fatal(err)
	}
	p, err := newDoctorProbe()
	if err != nil {
		t.Fatal(err)
	}
	p.stateRoot = t.TempDir()
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		if ref.Agent != "codex" {
			t.Errorf("unexpected probe: %s", ref)
		}
		return modelhealth.Classify(ref, "ok", nil, p.now)
	}
	env := map[string]string{"AGENT_ROLE_INTAKE": "claude:m:low", "AGENT_TIER_DEFAULT": "codex:w:low", "AGENT_ROLE_REVIEWER": "codex:w:low"}
	got := p.checkModels(context.Background(), []projects.Project{
		{Name: "A", Repository: config.Repository{Env: env}}, {Name: "B", Repository: config.Repository{Env: env}},
	}, nil)
	if got.Status != "fail" {
		t.Errorf("check = %+v", got)
	}
	rows, _ := got.Details.([]doctorModelStatus)
	for _, repo := range []string{"A", "B"} {
		assertDoctorModel(t, rows, repo, "intake", "claude:m:low", "unknown", "config_invalid")
		assertDoctorModel(t, rows, repo, "reviewer", "codex:w:low", "ok", "probe_ok")
	}
}

func TestDoctorModelsValidatesWorkerTierValues(t *testing.T) {
	for _, tc := range []struct{ chain, primary, max, role, ref string }{
		{"default", "codex:incomplete", "", "worker_default", "codex:incomplete:"},
		{"default max", "codex:m:low", "codex:incomplete", "worker_max", "codex:incomplete:"},
		{"max default", "codex:m:low", "codex:incomplete", "worker_max", "codex:incomplete:"},
		{"default a b c max", "codex:m:low", "codex:incomplete", "worker_max", "codex:incomplete:"},
	} {
		t.Run(tc.chain, func(t *testing.T) {
			now := time.Now()
			p := modelFixtureProbe(now, t.TempDir())
			p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
				if ref.Model == "incomplete" {
					t.Errorf("probed invalid tier: %s", ref)
				}
				return modelhealth.Classify(ref, "ok", nil, now)
			}
			env := map[string]string{"AGENT_TIER_ESCALATE": "1", "AGENT_TIER_CHAIN": tc.chain,
				"AGENT_TIER_DEFAULT": tc.primary, "AGENT_TIER_MAX": tc.max, "AGENT_ROLE_REVIEWER": "codex:m:low",
				"AGENT_TIER_A": "codex:m:low", "AGENT_TIER_B": "codex:m:low", "AGENT_TIER_C": "codex:m:low", "AGENT_ROLE_INTAKE": "claude:m:low"}
			got := p.checkModels(context.Background(), []projects.Project{{Name: "A", Repository: config.Repository{Env: env}}}, nil)
			if got.Status != "fail" {
				t.Errorf("check = %+v", got)
			}
			rows, _ := got.Details.([]doctorModelStatus)
			assertDoctorModel(t, rows, "A", tc.role, tc.ref, "unknown", "config_invalid")
			assertDoctorModel(t, rows, "A", "reviewer", "codex:m:low", "ok", "probe_ok")
			if tc.role != "worker_default" {
				assertDoctorModel(t, rows, "A", "worker_default", "codex:m:low", "ok", "probe_ok")
			}
		})
	}
}

func TestDoctorModelsMissingRequiredRoles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		missing []string
	}{
		{"empty", nil, []string{"worker_default", "reviewer", "intake"}},
		{"strong without default", map[string]string{"AGENT_TIER_ESCALATE": "1", "AGENT_TIER_STRONG": "claude:backup:high", "AGENT_ROLE_REVIEWER": "codex:m:high"}, []string{"worker_default"}},
		{"small without reviewer", map[string]string{"AGENT_TIER_DEFAULT": "codex:m:high", "AGENT_ROLE_REVIEWER_SMALL": "claude:small:high"}, []string{"reviewer", "intake"}},
		{"only intake missing", map[string]string{"AGENT_TIER_DEFAULT": "codex:m:high", "AGENT_ROLE_REVIEWER": "codex:m:low"}, []string{"intake"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			p := modelFixtureProbe(now, t.TempDir())
			got := p.checkModels(context.Background(), []projects.Project{{Name: "project", Repository: config.Repository{Env: tc.env}}}, nil)
			if got.Status != "fail" {
				t.Fatalf("check = %+v", got)
			}
			var missing []string
			for _, row := range got.Details.([]doctorModelStatus) {
				if row.Status.Reason == "not_configured" {
					if row.Repository != "project" || row.Status.Ref != (modelhealth.Ref{}) || row.Status.State != "unknown" {
						t.Fatalf("missing role row = %+v", row)
					}
					missing = append(missing, row.Role)
				}
			}
			if !reflect.DeepEqual(missing, tc.missing) {
				t.Fatalf("missing = %v, want %v", missing, tc.missing)
			}
		})
	}
}

func TestDoctorModelsDryRunMatchesRecordedVerdict(t *testing.T) {
	const proxy = "https://proxy.invalid"
	now := time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC)
	project := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
		"AGENT_TIER_ESCALATE": "1", "AGENT_TIER_CHAIN": "default strong",
		"AGENT_TIER_DEFAULT": "claude:m:high", "AGENT_TIER_STRONG": "codex:backup:high",
		"AGENT_ROLE_REVIEWER": "claude:m:low", "AGENT_ROLE_RESOLVER": "codex:backup:high", "AGENT_ROLE_INTAKE": "claude:intake:low",
	}}}
	verdicts := map[bool]healthCheck{}
	for _, record := range []bool{true, false} {
		p := modelFixtureProbe(now, t.TempDir())
		p.recordModels = record
		p.modelSettings = func() doctorModelSettings { return doctorModelSettings{proxyURL: proxy} }
		p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, endpoint modelhealth.Endpoint) modelhealth.Status {
			switch {
			case ref.Model != "m":
				return modelhealth.Classify(ref, "ok", nil, now)
			case endpoint.Isolated:
				t.Errorf("unexpected isolated probe of %s", ref)
			case endpoint.BaseURL == proxy:
				return modelhealth.Classify(ref, "", context.DeadlineExceeded, now)
			}
			return modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: now, Until: now.Add(time.Hour)}
		}
		verdicts[record] = p.checkModels(context.Background(), []projects.Project{project}, nil)
	}
	if verdicts[true].Status != "warn" || verdicts[false].Status != "warn" {
		t.Errorf("watch = %s, dry-run = %s, want warn: the strong worker is still available", verdicts[true].Status, verdicts[false].Status)
	}
	if !reflect.DeepEqual(verdicts[true].Details, verdicts[false].Details) {
		t.Errorf("watch = %s %+v, dry-run = %s %+v", verdicts[true].Status, verdicts[true].Details, verdicts[false].Status, verdicts[false].Details)
	}
	assertDoctorModel(t, verdicts[true].Details.([]doctorModelStatus), "one", "reviewer", "claude:m:low", "unknown", "timeout")
}

func TestDoctorModelsInvalidDriverEnvWithProcessIntake(t *testing.T) {
	home := t.TempDir()
	writeDoctorFile(t, filepath.Join(home, ".config/orca-factory/driver.env"), "AGENT_INTAKE_PI='unterminated\n", 0600)
	t.Setenv("HOME", home)
	t.Setenv("AGENT_MODEL_PROXY_URL", "https://proxy.invalid")
	t.Setenv("AGENT_INTAKE_PI", " ")
	t.Setenv("AGENT_INTAKE_CLAUDE", "/opt/test/claude")
	p, err := newDoctorProbe()
	if err != nil {
		t.Fatal(err)
	}
	p.stateRoot = t.TempDir()
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		return modelhealth.Classify(ref, "ok", nil, p.now)
	}
	project := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
		"AGENT_TIER_DEFAULT": "codex:w:high", "AGENT_ROLE_REVIEWER": "codex:w:low", "AGENT_ROLE_RESOLVER": "codex:w:high", "AGENT_ROLE_INTAKE": "claude:i:low",
	}}}
	got := p.checkModels(context.Background(), []projects.Project{project}, nil)
	assertDoctorModel(t, got.Details.([]doctorModelStatus), "one", "intake", "claude:i:low", "unknown", "config_invalid")
}

func TestDoctorModelsPathChangeDuringProbe(t *testing.T) {
	bin := t.TempDir()
	cli := filepath.Join(bin, "claude")
	writeDoctorFile(t, cli, "#!/bin/sh\n", 0o755)
	t.Setenv("PATH", bin)
	p := modelFixtureProbe(time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC), t.TempDir())
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		if ref.Model == "solo" {
			if err := os.Remove(cli); err != nil {
				t.Error(err)
			}
		}
		return modelhealth.Classify(ref, "ok", nil, p.now)
	}
	// Only the reviewer resolves claude on PATH, so a key recomputed after its
	// probe misses the one result it recorded.
	project := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
		"AGENT_TIER_DEFAULT": "codex:w:high", "AGENT_ROLE_REVIEWER": "claude:solo:low", "AGENT_ROLE_RESOLVER": "codex:w:high", "AGENT_ROLE_INTAKE": "claude:i:low",
	}}}
	got := p.checkModels(context.Background(), []projects.Project{project}, nil)
	if got.Status != "ok" {
		t.Errorf("check = %+v", got)
	}
	assertDoctorModel(t, got.Details.([]doctorModelStatus), "one", "reviewer", "claude:solo:low", "ok", "probe_ok")
}

// TestDoctorModelsUnloadedRecordedLedger replays the recorded 项目 table row
// (feishu/bot-project-records.json) through unloadedRepositories.
func TestDoctorModelsUnloadedRecordedLedger(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", "feishu", "bot-project-records.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recording struct {
		Response struct {
			Data struct {
				Fields []string `json:"fields"`
				Data   [][]any  `json:"data"`
			} `json:"data"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &recording); err != nil {
		t.Fatal(err)
	}
	var ledger []issues.LedgerProject
	for _, row := range recording.Response.Data.Data {
		at := func(name string) any { return row[slices.Index(recording.Response.Data.Fields, name)] }
		ledger = append(ledger, issues.LedgerProject{Name: at("名称").(string), Enabled: at("启用").(bool), Remote: at("仓库远端").(string)})
	}
	const remote = "github.com/vmxmy/orca-factory"
	rejected := []projects.Rejection{{Path: "/repos/orca-factory", Remote: remote, Reason: projects.ReasonCheckFailed}}
	if got := unloadedRepositories(rejected, ledger); len(got) != 1 || got[0].Remote != remote {
		t.Errorf("recorded enabled row = %+v, want the repository reported", got)
	}
	disabled := append(slices.Clone(ledger), issues.LedgerProject{Name: "orca-factory", Remote: remote})
	if got := unloadedRepositories(rejected, disabled); len(got) != 0 {
		t.Errorf("a later disabled row = %+v, want it to win as in projects.Discover", got)
	}
}

func TestDoctorModelsProbeKeyResolvesPath(t *testing.T) {
	bin := t.TempDir()
	writeDoctorFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\n", 0o755)
	t.Setenv("PATH", bin)
	ref := modelhealth.Ref{Agent: "claude", Model: "m"}
	onPath := doctorModelRole{ref: ref, endpoint: modelhealth.Endpoint{Isolated: true}}
	explicit := doctorModelRole{ref: ref, endpoint: modelhealth.Endpoint{Binary: filepath.Join(bin, "claude"), Isolated: true}}
	if onPath.probeKey() != explicit.probeKey() {
		t.Error("the PATH claude and its absolute path probe the same endpoint twice")
	}
}

// Legacy *_CMD reviewers are arbitrary shell; doctor reports them without
// guessing the program, model or endpoint the shell would choose.
func TestDoctorModelsLegacyCommandUnprobed(t *testing.T) {
	p := modelFixtureProbe(time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC), t.TempDir())
	p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
		if ref.Model != "w" && ref.Model != "i" {
			t.Errorf("probed %s from a legacy command", ref)
		}
		return modelhealth.Classify(ref, "ok", nil, p.now)
	}
	project := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
		"AGENT_TIER_DEFAULT": "codex:w:high", "AGENT_ROLE_RESOLVER": "codex:w:high", "AGENT_ROLE_INTAKE": "claude:i:low",
		"AGENT_ROLE_REVIEWER_CMD":        "env AUTH='Bearer TEST-PLACEHOLDER-CRED' /opt/custom/claude -p --model m",
		"AGENT_ROLE_REVIEWER_CMD_SMALL":  "claude -p --model \"${REVIEW_MODEL:-sonnet}\"",
		"AGENT_ROLE_SECOND_REVIEWER_CMD": "./tools/claude -p --model m",
	}}}
	got := p.checkModels(context.Background(), []projects.Project{project}, nil)
	if got.Status != "unknown" {
		t.Errorf("check = %s, want unknown", got.Status)
	}
	rows := got.Details.([]doctorModelStatus)
	for _, role := range []string{"reviewer", "reviewer_small", "second_reviewer", "cross_reviewer"} {
		assertDoctorModel(t, rows, "one", role, "::", "unknown", "legacy_command")
	}
	if report, _ := json.Marshal(got); bytes.Contains(report, []byte("TEST-PLACEHOLDER-CRED")) || bytes.Contains(report, []byte("REVIEW_MODEL")) {
		t.Fatal("command text in report")
	}
}

func TestDoctorModelsLegacyCommandMatchingStructuredValue(t *testing.T) {
	now := time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC)
	generated, err := config.GenerateReviewerCommand("claude:legacy:low")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"AGENT_TIER_DEFAULT": "codex:w:high", "AGENT_ROLE_RESOLVER": "codex:w:high", "AGENT_ROLE_INTAKE": "claude:i:low"}
	for _, tc := range []struct {
		name, command, ref, state, reason, status string
		probes                                    int32
	}{
		{"generated", generated, "claude:legacy:low", "unavailable", "usage_limit", "fail", 1},
		{"blank", "   ", "::", "unknown", "not_configured", "fail", 0},
		{"quoted model", strings.Replace(generated, "--model legacy", `--model "legacy"`, 1), "::", "unknown", "legacy_command", "unknown", 0},
		{"variable model", strings.Replace(generated, "--model legacy", "--model $REVIEW_MODEL", 1), "::", "unknown", "legacy_command", "unknown", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := modelFixtureProbe(now.Add(time.Minute), t.TempDir())
			registry, err := modelhealth.Open(p.stateRoot)
			if err != nil {
				t.Fatal(err)
			}
			cooling := modelhealth.Status{Ref: modelhealth.Ref{Agent: "claude", Model: "legacy"}, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: now, Until: now.Add(time.Hour)}
			if err := registry.Record(cooling); err != nil {
				t.Fatal(err)
			}
			var probes atomic.Int32
			p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
				switch {
				case ref.Key() == "claude:legacy":
					probes.Add(1)
					return modelhealth.Classify(ref, "", context.DeadlineExceeded, p.now)
				case ref.Agent == "claude" && ref.Model != "i":
					t.Errorf("unexpected probe of %s", ref)
				}
				return modelhealth.Classify(ref, "ok", nil, p.now)
			}
			project := projects.Project{Name: "one", Repository: config.Repository{Env: maps.Clone(env)}}
			project.Repository.Env["AGENT_ROLE_REVIEWER_CMD"] = tc.command
			got := p.checkModels(context.Background(), []projects.Project{project}, nil)
			if got.Status != tc.status || probes.Load() != tc.probes {
				t.Errorf("check = %s after %d reviewer probes, want %s after %d", got.Status, probes.Load(), tc.status, tc.probes)
			}
			assertDoctorModel(t, got.Details.([]doctorModelStatus), "one", "reviewer", tc.ref, tc.state, tc.reason)
		})
	}
}

func TestDoctorWarnsOnHostMemoryHold(t *testing.T) {
	f := newDoctorFixture(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	for _, tc := range []struct {
		hostMemory, status, reason, source string
		want                               []string
	}{
		{`{"paused":true,"since":"2026-11-19T10:00:00Z","reading":{"available":5368709120,"someAvg60":3,"source":"/orca.slice"}}`, "warn", "已暂停认领", "/orca.slice", []string{"5368709120", "2026-11-19T10:00:00Z", "memory.peak"}},
		{`{"paused":true,"since":"2026-11-19T10:00:00Z"}`, "warn", "已暂停认领", "host", []string{"2026-11-19T10:00:00Z"}},
		{`{"paused":false,"error":"read /proc/pressure/memory: no such file"}`, "warn", "读不到主机内存读数，调度器不按内存暂停：read /proc/pressure/memory: no such file", "", []string{"/proc/pressure/memory"}},
		{`{"paused":false,"reading":{"available":32212254720,"someAvg60":0}}`, "ok", "未记录主机内存暂停", "host", nil},
		{`{"paused":false,"reading":{"available":3221225472,"someAvg60":30,"source":"host"},"error":"read /proc/meminfo: denied"}`, "warn", "读不到主机内存读数，调度器不按内存暂停：read /proc/meminfo: denied", "host", []string{"3221225472", `"error"`, "/proc/meminfo"}},
		{`{"paused":false,"reading":{"available":32212254720,"someAvg60":0,"source":"host"},"cgroupError":"read memory.stat: denied"}`, "warn", "cgroup 读取失败，按主机与本轮读到的层级判断：read memory.stat: denied", "host", []string{`"cgroup_error"`, "memory.stat"}},
		{`{"paused":true,"since":"2026-11-19T10:00:00Z","reading":{"available":3221225472,"source":"host"},"cgroupError":"read memory.stat: denied"}`, "warn", "主机内存吃紧，已暂停认领与新建终端；读不到 cgroup 内存读数：read memory.stat: denied", "host", []string{"2026-11-19T10:00:00Z", `"cgroup_error"`, "memory.stat"}},
	} {
		writeDoctorFile(t, status.PushPath(), fmt.Sprintf(`{"generatedAt":%q,"hostMemory":%s,"tickets":[]}`, f.now.Format(time.RFC3339Nano), tc.hostMemory), 0600)
		result := runDoctorProbe(t, f.probe(t, []string{"go", "gh", "git", "orca", "claude"}, inFlightLedger("OF-1", "进度")))
		if result.statuses["host_memory"] != tc.status {
			t.Errorf("host_memory = %q, want %q for %s", result.statuses["host_memory"], tc.status, tc.hostMemory)
		}
		if got := result.reasons["host_memory"]; !strings.Contains(got, tc.reason) {
			t.Errorf("host_memory reason = %q, want %q for %s", got, tc.reason, tc.hostMemory)
		}
		detail := result.details["host_memory"]
		var fields struct{ Source string }
		if err := json.Unmarshal(detail, &fields); err != nil {
			t.Fatal(err)
		}
		if fields.Source != tc.source {
			t.Errorf("host_memory source = %q, want %q for %s", fields.Source, tc.source, tc.hostMemory)
		}
		for _, want := range tc.want {
			if !bytes.Contains(detail, []byte(want)) {
				t.Errorf("host_memory detail lacks %q: %s", want, detail)
			}
		}
	}
}

func TestHostMemoryNotificationThroughStatusBackend(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/feishu/bot-message-send.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording struct {
		Request struct {
			Method, Path string
			Body         struct {
				ReceiveID string `json:"receive_id"`
				MsgType   string `json:"msg_type"`
			}
		}
		Response struct{ Data json.RawMessage }
	}
	if err := json.Unmarshal(raw, &recording); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "test", "expire": 7200})
			return
		}
		calls++
		var body struct {
			ReceiveID string `json:"receive_id"`
			MsgType   string `json:"msg_type"`
			Content   string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if r.Method != recording.Request.Method || r.URL.Path != recording.Request.Path || r.URL.Query().Get("receive_id_type") != "chat_id" || body.ReceiveID != recording.Request.Body.ReceiveID || body.MsgType != recording.Request.Body.MsgType {
			t.Errorf("notification request = %s %s %+v", r.Method, r.URL, body)
		}
		for _, want := range []string{"15m0s", "3221225472", "30.00", "/orca.slice"} {
			if !strings.Contains(body.Content, want) {
				t.Errorf("message lacks %q: %s", want, body.Content)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": recording.Response.Data})
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "test-app")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "test-secret")
	t.Setenv("AGENT_FEISHU_NOTIFY_CHAT", recording.Request.Body.ReceiveID)
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	backend := &statusBackend{Backend: &scheduler.Runtime{Now: func() time.Time { return now }}}
	memory := status.HostMemory{Paused: true, Since: now.Add(-15 * time.Minute), Reading: hostmem.Reading{Available: 3 << 30, SomeAvg60: 30, Source: "/orca.slice"}}
	if err := backend.NotifyHostMemory(context.Background(), memory); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("notification requests = %d, want 1", calls)
	}
}

func TestModelRelayDoctor(t *testing.T) {
	const token = "fake-doctor-relay-sentinel"
	checkOutput := func(t *testing.T, report healthReport) {
		t.Helper()
		for _, asJSON := range []bool{true, false} {
			var output bytes.Buffer
			if err := writeHealth(&output, report, asJSON, doctorChecks); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), token) {
				t.Fatal("credential in doctor output")
			}
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_MODEL_PROXY_URL", "https://relay.invalid")
	t.Setenv("AGENT_MODEL_PROXY_TOKEN", token)
	t.Setenv("AGENT_INTAKE_PI", "")
	t.Setenv("AGENT_INTAKE_CLAUDE", "/opt/test/claude")
	project := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
		"AGENT_TIER_DEFAULT": "claude:worker:high", "AGENT_ROLE_RESOLVER": "claude:resolver:high",
		"AGENT_ROLE_REVIEWER": "claude:review:high", "AGENT_ROLE_REVIEWER_SMALL": "claude:small:high",
		"AGENT_ROLE_SECOND_REVIEWER": "claude:second:high", "AGENT_ROLE_INTAKE": "claude:intake:high",
	}}}
	for _, url := range []string{"https://relay.invalid", "", "ftp://" + token} {
		t.Run(url, func(t *testing.T) {
			t.Setenv("AGENT_MODEL_PROXY_URL", url)
			p, err := newDoctorProbe()
			if err != nil {
				t.Fatal("doctor constructor failed")
			}
			p.stateRoot = t.TempDir()
			settings := p.modelSettings()
			for _, role := range settings.modelRoles(project) {
				isolated := role.name == "intake" || role.name == "second_reviewer" || role.name == "cross_reviewer"
				if url == "https://relay.invalid" && isolated {
					if role.endpoint.BaseURL != url || role.endpoint.AuthToken != token {
						t.Errorf("relay missing from %s", role.name)
					}
					without := role
					without.endpoint.AuthToken = ""
					if without.probeKey() == role.probeKey() {
						t.Error("probe key ignores credentials")
					}
				} else if role.endpoint.AuthToken != "" {
					t.Errorf("credential reached %s", role.name)
				}
				if role.name == "reviewer" || role.name == "reviewer_small" {
					if role.endpoint.BaseURL != settings.proxyURL {
						t.Error("reviewer URL behavior changed")
					}
				}
			}
			p.modelProbe = func(_ context.Context, ref modelhealth.Ref, _ string, _ modelhealth.Endpoint) modelhealth.Status {
				return modelhealth.Classify(ref, "ok", nil, p.now)
			}
			got := p.checkModels(context.Background(), []projects.Project{project}, nil)
			if url != "https://relay.invalid" && got.Status != "fail" {
				t.Fatalf("invalid relay status=%s", got.Status)
			}
			checkOutput(t, healthReport{Checks: map[string]healthCheck{"models": got}})
		})
	}
	for _, unavailable := range []string{"discovery", "registry"} {
		t.Run(unavailable, func(t *testing.T) {
			f := newDoctorFixture(t)
			p := f.probe(t, nil, doctorFakeLedger{})
			var discoveryErr error
			if unavailable == "discovery" {
				discoveryErr = errors.New("discovery failed")
			} else {
				writeDoctorFile(t, filepath.Join(p.stateRoot, "model-health.json"), "{", 0600)
			}
			p.projects = func(context.Context) (projects.Result, error) { return projects.Result{}, discoveryErr }
			for _, tc := range []struct{ url, status string }{{"https://relay.invalid", healthUnknown}, {"", healthFail}} {
				t.Setenv("AGENT_MODEL_PROXY_URL", tc.url)
				doctor, err := newDoctorProbe()
				if err != nil {
					t.Fatal(err)
				}
				p.modelSettings = doctor.modelSettings
				report := p.run(context.Background())
				got := report.Checks["models"]
				if got.Status != tc.status {
					t.Errorf("models status = %s, want %s", got.Status, tc.status)
				}
				if tc.status == healthFail && got.Reason != "模型中转站配置无效，网关与交叉审查无法启动" {
					t.Error("doctor did not identify invalid relay configuration")
				}
				checkOutput(t, report)
			}
		})
	}
	p, err := newDoctorProbe()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ tier, wantURL, wantToken string }{
		{tier: "claude::high", wantURL: "https://relay.invalid", wantToken: token},
		{tier: "claude:m;env:high"},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			project.Repository.Env["AGENT_ROLE_REVIEWER"] = tc.tier
			project.Repository.Env["AGENT_ROLE_SECOND_REVIEWER"] = tc.tier
			roles := p.modelSettings().modelRoles(project)
			for _, name := range []string{"second_reviewer", "cross_reviewer"} {
				i := slices.IndexFunc(roles, func(r doctorModelRole) bool { return r.name == name })
				if i < 0 {
					t.Fatalf("missing %s", name)
				}
				endpoint := roles[i].endpoint
				if !endpoint.Isolated || endpoint.BaseURL != tc.wantURL || endpoint.AuthToken != tc.wantToken {
					t.Errorf("unexpected relay endpoint for %s", name)
				}
			}
		})
	}
	t.Run("legacy command equal to default model", func(t *testing.T) {
		generated, err := config.GenerateReviewerCommand("claude::high")
		if err != nil {
			t.Fatal(err)
		}
		legacy := projects.Project{Name: "one", Repository: config.Repository{Env: map[string]string{
			"AGENT_ROLE_REVIEWER_CMD": generated, "AGENT_ROLE_SECOND_REVIEWER_CMD": generated,
		}}}
		roles := p.modelSettings().modelRoles(legacy)
		for _, name := range []string{"second_reviewer", "cross_reviewer"} {
			i := slices.IndexFunc(roles, func(r doctorModelRole) bool { return r.name == name })
			if i < 0 || roles[i].ref.Agent != "claude" {
				t.Fatalf("legacy %s not probed as claude", name)
			}
			if roles[i].endpoint.AuthToken != "" || roles[i].endpoint.BaseURL != "" {
				t.Errorf("relay reached legacy %s", name)
			}
		}
	})
	project.Repository.Env["AGENT_ROLE_REVIEWER"] = "codex:review:high"
	project.Repository.Env["AGENT_ROLE_SECOND_REVIEWER"] = "codex:second:high"
	project.Repository.Env["AGENT_INTAKE_PI"] = "anthropic/fable:high"
	for _, role := range p.modelSettings().modelRoles(project) {
		if role.ref.Agent != "claude" && (role.endpoint.BaseURL != "" || role.endpoint.AuthToken != "") {
			t.Errorf("relay reached %s", role.name)
		}
	}
}
