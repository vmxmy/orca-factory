package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type doctorFakeLedger struct {
	tasks    []issues.Task
	comments map[string][]task.Comment
}

func (l doctorFakeLedger) List(context.Context) ([]issues.Task, error) { return l.tasks, nil }
func (l doctorFakeLedger) ListComments(_ context.Context, id string) ([]task.Comment, error) {
	return l.comments[id], nil
}

type doctorFixture struct {
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
	gh := "#!/bin/sh\ncase \"$1 $2\" in\n\"pr list\") cat \"$DOCTOR_GH_PRS\" ;;\n\"auth status\") exit \"$DOCTOR_GH_AUTH\" ;;\n*) exit 9 ;;\nesac\n"
	writeDoctorFile(t, filepath.Join(bin, "gh"), gh, 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := filepath.Join(root, "state")
	t.Setenv("AGENT_DRIVER_STATE_ROOT", state)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return doctorFixture{root: root, factory: factory, previous: previous, current: current, now: now}
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

func (f doctorFixture) probe(t *testing.T, serviceTools []string, ledger doctorFakeLedger) doctorProbe {
	t.Helper()
	serviceBin := filepath.Join(f.root, "service-bin")
	for _, tool := range serviceTools {
		writeDoctorFile(t, filepath.Join(serviceBin, tool), "#!/bin/sh\n", 0o755)
	}
	return doctorProbe{
		now:         f.now,
		stateRoot:   filepath.Join(f.root, "state"),
		factoryRoot: f.factory,
		serviceEnv: func(context.Context) (map[string]string, error) {
			return map[string]string{"PATH": serviceBin, "AGENT_FEISHU_APP_ID": "TEST-PLACEHOLDER-CRED", "AGENT_FEISHU_APP_SECRET": "TEST-PLACEHOLDER-CRED"}, nil
		},
		projects: func(context.Context) ([]projects.Project, error) {
			return []projects.Project{{Path: f.factory, Prefix: "OF", Name: "orca-factory"}}, nil
		},
		ledger: func(context.Context, projects.Project) (doctorLedger, error) { return ledger, nil },
	}
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
	result := doctorResult{code: code, statuses: map[string]string{}, errors: report.Errors, details: map[string]json.RawMessage{}, out: out.Bytes()}
	for name, check := range report.Checks {
		result.statuses[name] = check.Status
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

	result.want(t, 0, map[string]string{"service_path": "ok", "version": "ok", "maintenance": "ok", "checkouts": "ok", "sync_prs": "ok", "ledger": "ok", "launches": "ok", "credentials": "ok"})
	if len(result.errors) != 0 {
		t.Fatalf("errors = %v, want none", result.errors)
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
	probe.projects = func(context.Context) ([]projects.Project, error) {
		return []projects.Project{{Path: f.factory, Prefix: "OF", Name: "orca-factory"}, {Path: feature, Prefix: "FX", Name: "feature"}}, nil
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
	probe.projects = func(context.Context) ([]projects.Project, error) { return nil, errors.New("orca unreachable") }
	runDoctorProbe(t, probe).want(t, 0, map[string]string{"service_path": "unknown", "checkouts": "unknown", "sync_prs": "unknown", "ledger": "unknown", "credentials": "unknown"})
}
