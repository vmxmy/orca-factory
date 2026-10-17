package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
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
// past previous, a gh stand-in on PATH, and an empty driver state root.
func newDoctorFixture(t *testing.T) doctorFixture {
	t.Helper()
	root := t.TempDir()
	origin, factory := filepath.Join(root, "origin.git"), filepath.Join(root, "factory")
	gitIn(t, root, "init", "--quiet", "--bare", "-b", "main", origin)
	gitIn(t, root, "clone", "--quiet", origin, factory)
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
	return doctorFixture{root: root, factory: factory, previous: previous, current: current, now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
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

func runDoctorProbe(t *testing.T, probe doctorProbe) (int, map[string]string, []byte) {
	t.Helper()
	var out bytes.Buffer
	code := probe.main(context.Background(), []string{"--json"}, &out)
	var report struct {
		Checks map[string]struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"checks"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("doctor --json is not JSON: %v\n%s", err, out.String())
	}
	if report.Errors == nil {
		t.Fatalf("errors must be an array:\n%s", out.String())
	}
	statuses := map[string]string{}
	for name, check := range report.Checks {
		statuses[name] = check.Status
	}
	if strings.Contains(out.String(), "TEST-PLACEHOLDER-CRED") {
		t.Fatalf("doctor output leaks a credential value:\n%s", out.String())
	}
	return code, statuses, out.Bytes()
}

func TestDriverDoctorReportsHealthyRuntime(t *testing.T) {
	f := newDoctorFixture(t)
	writeDoctorFile(t, filepath.Join(f.root, "prs.json"), `[]`, 0o600)
	t.Setenv("DOCTOR_GH_PRS", filepath.Join(f.root, "prs.json"))
	t.Setenv("DOCTOR_GH_AUTH", "0")
	f.logLine(t, "scheduler.log", f.now.Add(-time.Hour), `"msg":"command started","component":"scheduler","event":"start","version":"`+f.current+`"`)
	f.logLine(t, "launch-OF-1.log", f.now.Add(-10*time.Minute), `"msg":"command started","event":"start","version":"`+f.current+`"`)
	f.logLine(t, "launch-OF-1.log", f.now.Add(-9*time.Minute), `"msg":"command exited","event":"exit","exit_code":0`)
	inFlight := issues.Task{Issue: task.Issue{ID: "OF-1", Identifier: "OF-1", State: task.NamedID{ID: "started", Name: "In Progress"}}}
	ledger := doctorFakeLedger{tasks: []issues.Task{inFlight}, comments: map[string][]task.Comment{"OF-1": {{ID: "c1", Body: "进度"}}}}

	code, statuses, out := runDoctorProbe(t, f.probe(t, []string{"go", "gh", "git", "orca", "claude"}, ledger))

	want := map[string]string{"service_path": "ok", "version": "ok", "maintenance": "ok", "checkouts": "ok", "sync_prs": "ok", "ledger": "ok", "launches": "ok", "credentials": "ok"}
	if !reflect.DeepEqual(statuses, want) || code != 0 {
		t.Fatalf("exit %d statuses %v, want exit 0 %v\n%s", code, statuses, want, out)
	}
}

func TestDriverDoctorReportsFailingRuntime(t *testing.T) {
	f := newDoctorFixture(t)
	gitIn(t, f.factory, "reset", "--quiet", "--hard", f.previous)
	writeDoctorFile(t, filepath.Join(f.root, "prs.json"), `[{"number":7,"headRefName":"`+coreSyncBranchPrefix+f.previous+`","url":"https://example.invalid/pr/7"}]`, 0o600)
	t.Setenv("DOCTOR_GH_PRS", filepath.Join(f.root, "prs.json"))
	t.Setenv("DOCTOR_GH_AUTH", "1")
	writeDoctorFile(t, filepath.Join(f.root, "state", "maintenance.json"), `{"mode":"frozen","updatedAt":"2026-10-17T11:00:00Z"}`, 0o600)
	f.logLine(t, "scheduler.log", f.now.Add(-time.Hour), `"msg":"command started","component":"scheduler","event":"start","version":"`+f.previous+`"`)
	for i := 0; i < 3; i++ {
		at := f.now.Add(-time.Duration(20-i) * time.Minute)
		f.logLine(t, "launch-ZCP-63.log", at, `"msg":"command started","event":"start"`)
		f.logLine(t, "launch-ZCP-63.log", at.Add(time.Second), `"msg":"command exited","event":"exit","exit_code":1`)
	}
	inFlight := issues.Task{Issue: task.Issue{ID: "ZCP-63", Identifier: "ZCP-63", State: task.NamedID{ID: "started", Name: "In Progress"}}}
	ledger := doctorFakeLedger{tasks: []issues.Task{inFlight}, comments: map[string][]task.Comment{"ZCP-63": {{ID: "c1", Body: ""}, {ID: "c2", Body: "x"}}}}
	probe := f.probe(t, []string{"git", "orca"}, ledger)
	probe.serviceEnv = func(context.Context) (map[string]string, error) {
		return map[string]string{"PATH": filepath.Join(f.root, "service-bin"), "AGENT_FEISHU_APP_ID": "TEST-PLACEHOLDER-CRED"}, nil
	}

	code, statuses, out := runDoctorProbe(t, probe)

	want := map[string]string{"service_path": "fail", "version": "fail", "maintenance": "fail", "checkouts": "fail", "sync_prs": "fail", "ledger": "fail", "launches": "fail", "credentials": "fail"}
	if !reflect.DeepEqual(statuses, want) || code != 1 {
		t.Fatalf("exit %d statuses %v, want exit 1 %v\n%s", code, statuses, want, out)
	}
}
