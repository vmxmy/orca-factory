package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// evidenceStore fakes the issues.Store reads the ticket query uses.
type evidenceStore struct {
	issues.Store
	tasks       []issues.Task
	comments    map[string][]task.Comment
	readErr     error
	commentsErr error
	reads       []string
}

func (s *evidenceStore) Repository() string { return "vmxmy/orca-factory" }

func (s *evidenceStore) List(context.Context) ([]issues.Task, error) { return s.tasks, nil }

func (s *evidenceStore) Read(_ context.Context, id string) (issues.Task, error) {
	s.reads = append(s.reads, id)
	if s.readErr != nil {
		return issues.Task{}, s.readErr
	}
	for _, t := range s.tasks {
		if t.Metadata.Identifier == id {
			return t, nil
		}
	}
	return issues.Task{}, errors.New("task not found")
}

func (s *evidenceStore) ListComments(_ context.Context, id string) ([]task.Comment, error) {
	return s.comments[id], s.commentsErr
}

func labeledTask(identifier, state string, labels ...string) issues.Task {
	t := managedTask("uuid-"+identifier, identifier, "票 "+identifier, state, true)
	t.URL = "https://ledger.example/" + identifier
	for _, l := range labels {
		t.Labels.Nodes = append(t.Labels.Nodes, task.NamedID{ID: l, Name: l})
	}
	return t
}

// recordedStatusTime is generatedAt of driver-status-json-stale.json.
var recordedStatusTime = time.Date(2026, 11, 9, 10, 59, 37, 443068794, time.FixedZone("CST", 8*3600))

// evidenceRuntime wires one project "OF" at the recorded repository path,
// with the real status, maintenance and log readers pointed at temp roots.
func evidenceRuntime(t *testing.T, now time.Time, env map[string]string) (*projectRuntime, *evidenceStore, string) {
	t.Helper()
	runtimeDir, stateRoot := t.TempDir(), t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("DRIVER_STATUS_FILE", "")
	t.Setenv("AGENT_DRIVER_STATE_ROOT", stateRoot)
	copyRecorded(t, "driver-status-json-stale.json", filepath.Join(runtimeDir, "orca-factory", "status.json"))
	copyRecorded(t, "driver-maintenance-status.json", filepath.Join(stateRoot, "maintenance.json"))
	store := &evidenceStore{tasks: []issues.Task{labeledTask("OF-427", "In Progress", "auto-impl", "auto-failed"), labeledTask("OF-12", "Todo")},
		comments: map[string][]task.Comment{"uuid-OF-427": {{Body: "Agent处置: executor failed twice\n证据: scheduler inspection\n阶段: Agent处置\n归属: 处置Agent term=- run=- attempt=2 scheduler-attempt=2 since=2026-11-07T03:40:58Z"}}}}
	r := newProjectRuntime("/usr/bin/true", t.TempDir(), config.Tuning{})
	r.readers = defaultEvidenceReaders()
	r.readers.logRoot = stateRoot
	r.readers.now = func() time.Time { return now }
	if env == nil {
		env = map[string]string{"AGENT_GH_REPO": "vmxmy/orca-factory"}
	}
	r.sources["OF"] = newQuerySource(store, "/home/laoxu/github/orca-factory", env)
	return r, store, stateRoot
}

func copyRecorded(t *testing.T, name, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func evidenceData(t *testing.T, e intake.Evidence, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(e.Data), v); err != nil {
		t.Fatalf("evidence data %q: %v", e.Data, err)
	}
}

var ofProject = intake.Project{Prefix: "OF"}

func TestIntakeQueryStatusEvidence(t *testing.T) {
	var data struct {
		Tickets []statusTicketEvidence `json:"tickets"`
	}
	t.Run("fresh snapshot keeps only this project", func(t *testing.T) {
		r, _, _ := evidenceRuntime(t, recordedStatusTime.Add(30*time.Second), nil)
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryStatus})
		if e.Outcome != intake.OutcomeOK || e.Stale || !e.Complete || !e.SourceGeneratedAt.Equal(recordedStatusTime) {
			t.Fatalf("evidence=%+v", e)
		}
		evidenceData(t, e, &data)
		var ids []string
		for _, ticket := range data.Tickets {
			ids = append(ids, ticket.Identifier+"/"+ticket.Owner+"/"+ticket.Phase)
		}
		if !slices.Contains(ids, "OF-427/处置Agent/Agent处置") || strings.Contains(e.Data, "ZY-") || strings.Contains(e.Data, "zhiyun") {
			t.Fatalf("tickets=%v data=%s", ids, e.Data)
		}
		if !strings.Contains(e.Data, "不证明 worker 进程存活") {
			t.Fatalf("data lacks the liveness caveat: %s", e.Data)
		}
	})
	t.Run("old snapshot is stale and keeps its own generation time", func(t *testing.T) {
		r, _, _ := evidenceRuntime(t, recordedStatusTime.Add(121*time.Second), nil)
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryStatus})
		if !e.Stale || !e.SourceGeneratedAt.Equal(recordedStatusTime) || e.CollectedAt.Equal(e.SourceGeneratedAt) {
			t.Fatalf("evidence=%+v", e)
		}
	})
	t.Run("missing snapshot fails instead of reporting empty", func(t *testing.T) {
		r, _, _ := evidenceRuntime(t, recordedStatusTime, nil)
		if err := os.Remove(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "orca-factory", "status.json")); err != nil {
			t.Fatal(err)
		}
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryStatus})
		if e.Outcome != intake.OutcomeFailed || !strings.Contains(e.Error, "存活均未知") {
			t.Fatalf("evidence=%+v", e)
		}
	})
}

func TestIntakeQueryTicketEvidence(t *testing.T) {
	var detail ticketDetail
	t.Run("live read with labels and ledger ownership", func(t *testing.T) {
		r, _, _ := evidenceRuntime(t, recordedStatusTime, nil)
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryTicket, Ticket: "OF-427"})
		evidenceData(t, e, &detail)
		if e.Outcome != intake.OutcomeOK || !slices.Equal(detail.Labels, []string{"auto-impl", "auto-failed"}) || detail.Owner != "处置Agent" || detail.Phase != "Agent处置" || !detail.CommentsComplete {
			t.Fatalf("evidence=%+v detail=%+v", e, detail)
		}
	})
	t.Run("no ownership events is unknown", func(t *testing.T) {
		r, _, _ := evidenceRuntime(t, recordedStatusTime, nil)
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryTicket, Ticket: "OF-12"})
		evidenceData(t, e, &detail)
		if detail.Owner != "未知" || detail.Phase != "未知" {
			t.Fatalf("detail=%+v", detail)
		}
	})
	t.Run("comment failure is partial", func(t *testing.T) {
		r, store, _ := evidenceRuntime(t, recordedStatusTime, nil)
		store.commentsErr = errors.New("rate limited ghp_secretvalue")
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryTicket, Ticket: "OF-427"})
		if e.Outcome != intake.OutcomePartial || e.Complete || strings.Contains(e.Error, "ghp_secretvalue") {
			t.Fatalf("evidence=%+v", e)
		}
	})
	t.Run("ledger failure fails", func(t *testing.T) {
		r, store, _ := evidenceRuntime(t, recordedStatusTime, nil)
		store.readErr = errors.New("ledger down")
		if e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryTicket, Ticket: "OF-427"}); e.Outcome != intake.OutcomeFailed || e.Data != "" {
			t.Fatalf("evidence=%+v", e)
		}
	})
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIntakeQueryLogsEvidence(t *testing.T) {
	var data struct {
		Records      []map[string]any `json:"records"`
		OmittedOlder int              `json:"omittedOlder"`
	}
	firstRun := time.Date(2026, 11, 7, 11, 40, 0, 0, time.FixedZone("CST", 8*3600))
	t.Run("project-scoped executor and attributed scheduler records", func(t *testing.T) {
		r, _, root := evidenceRuntime(t, firstRun, nil)
		copyRecorded(t, "driver-logs-executor-auth-failure.jsonl", filepath.Join(root, "orca-factory", "OF-427", "executor.log"))
		// Synthetic: the same ticket number in another project, and shared
		// scheduler records for another or no repository.
		writeLines(t, filepath.Join(root, "other", "OF-427", "executor.log"), `{"time":"2026-11-07T11:30:00+08:00","msg":"other project record","ticket":"OF-427"}`)
		writeLines(t, filepath.Join(root, "scheduler.log"),
			`{"time":"2026-11-07T11:29:00+08:00","msg":"merge gate finished","component":"scheduler","ticket":"OF-427","repository":"vmxmy/orca-factory","result":"green"}`,
			`{"time":"2026-11-07T11:29:01+08:00","msg":"foreign repository","component":"scheduler","ticket":"OF-427","repository":"vmxmy/other"}`,
			`{"time":"2026-11-07T11:29:02+08:00","msg":"unattributed","component":"scheduler","ticket":"OF-427"}`)
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryLogs, Ticket: "OF-427", Window: 30 * time.Minute})
		evidenceData(t, e, &data)
		if e.Outcome != intake.OutcomeOK || !e.Complete || len(data.Records) != 8 {
			t.Fatalf("evidence=%+v records=%d", e, len(data.Records))
		}
		for _, banned := range []string{"other project record", "foreign repository", "unattributed", "rev-parse"} {
			if strings.Contains(e.Data, banned) {
				t.Fatalf("data leaks %q: %s", banned, e.Data)
			}
		}
		if !strings.Contains(e.Data, "no oauth token found for github.com") || !strings.Contains(e.Data, "global dependency failure") {
			t.Fatalf("data lacks the recorded failure: %s", e.Data)
		}
	})
	t.Run("caps records, keeps newest in stable order, redacts", func(t *testing.T) {
		r, _, root := evidenceRuntime(t, firstRun, nil)
		var lines []string
		for i := range 250 {
			lines = append(lines, fmt.Sprintf(`{"time":"2026-11-07T11:20:%02d+08:00","msg":"tick %03d","stderr":"token ghp_secretvalue%d"}`, i/5, i, i))
		}
		writeLines(t, filepath.Join(root, "orca-factory", "OF-427", "executor.log"), lines...)
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryLogs, Ticket: "OF-427", Window: time.Hour})
		evidenceData(t, e, &data)
		if len(data.Records) != intake.MaxLogRecords || data.OmittedOlder != 50 || e.Complete || data.Records[0]["msg"] != "tick 050" || data.Records[199]["msg"] != "tick 249" {
			t.Fatalf("records=%d omitted=%d complete=%v first=%v", len(data.Records), data.OmittedOlder, e.Complete, data.Records[0])
		}
		if strings.Contains(e.Data, "ghp_secretvalue") {
			t.Fatal("credential-shaped text reached the evidence")
		}
	})
	t.Run("window excludes old records and no logs is empty", func(t *testing.T) {
		r, _, root := evidenceRuntime(t, firstRun.Add(2*time.Hour), nil)
		copyRecorded(t, "driver-logs-executor-auth-failure.jsonl", filepath.Join(root, "orca-factory", "OF-427", "executor.log"))
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryLogs, Ticket: "OF-427", Window: time.Minute})
		if e.Outcome != intake.OutcomeEmpty || !e.Complete {
			t.Fatalf("evidence=%+v", e)
		}
	})
	t.Run("unreadable log fails", func(t *testing.T) {
		r, _, root := evidenceRuntime(t, firstRun, nil)
		path := filepath.Join(root, "orca-factory", "OF-427", "executor.log")
		copyRecorded(t, "driver-logs-executor-auth-failure.jsonl", path)
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		if e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryLogs, Ticket: "OF-427", Window: time.Hour}); e.Outcome != intake.OutcomeFailed {
			t.Fatalf("evidence=%+v", e)
		}
	})
}

func TestIntakeQueryConfigEvidence(t *testing.T) {
	env := map[string]string{"AGENT_GH_REPO": "vmxmy/orca-factory", "AGENT_MAX_IN_FLIGHT": "2", "AGENT_TIER_STRONG": "claude:claude-opus-5-5:high",
		"AGENT_SMOKE_CMD": "curl -H 'Authorization: Bearer smoke-secret' https://example", "AGENT_FEISHU_APP_SECRET": "app-secret-value"}
	r, _, _ := evidenceRuntime(t, recordedStatusTime, env)
	e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryConfig})
	if e.Outcome != intake.OutcomeOK {
		t.Fatalf("evidence=%+v", e)
	}
	for _, want := range []string{`"AGENT_MAX_IN_FLIGHT":"2（agent-graph.env）"`, "claude-opus-5-5", `"maintenanceMode":"draining"`} {
		if !strings.Contains(e.Data, want) {
			t.Fatalf("data lacks %q: %s", want, e.Data)
		}
	}
	for _, banned := range []string{"smoke-secret", "app-secret-value", "AGENT_SMOKE_CMD", "AGENT_FEISHU_APP_SECRET"} {
		if strings.Contains(e.Data, banned) {
			t.Fatalf("data leaks %q: %s", banned, e.Data)
		}
	}
	m := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryMaintenance})
	if m.Outcome != intake.OutcomeOK || !strings.Contains(m.Data, `"mode":"draining"`) || m.SourceGeneratedAt.IsZero() {
		t.Fatalf("maintenance=%+v", m)
	}
}

// TestIntakeQueryProductionLoop drives the production Querier through the
// intake loop: queries resolve only against the session's bound project, a
// foreign prefix never reaches any ledger, and an unwired project fails.
func TestIntakeQueryProductionLoop(t *testing.T) {
	r, store, _ := evidenceRuntime(t, recordedStatusTime.Add(10*time.Second), nil)
	var prompts []string
	answers := []string{
		"先查一下\n```intake-query-v1\n{\"queries\":[{\"op\":\"ticket\",\"ticket\":\"OF-427\"},{\"op\":\"ticket\",\"ticket\":\"ZY-427\"},{\"op\":\"status\"}]}\n```",
		"OF-427 带 auto-failed，归属 处置Agent，不会被普通领票。",
	}
	turn := func(_ context.Context, t intake.Turn) (string, error) {
		prompts = append(prompts, t.Prompt)
		answer := answers[0]
		answers = answers[1:]
		return answer, nil
	}
	answer, err := intake.RunQueries(context.Background(), turn, r.query, intake.Turn{Session: "s1", Prompt: "OF-427 为什么没领", Project: ofProject})
	if err != nil || answer != "OF-427 带 auto-failed，归属 处置Agent，不会被普通领票。" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
	if !slices.Equal(store.reads, []string{"OF-427"}) {
		t.Fatalf("ledger reads=%v, want only the bound project's ticket", store.reads)
	}
	evidence := prompts[1]
	for _, want := range []string{`\"labels\":[\"auto-impl\",\"auto-failed\"]`, "不属于当前会话绑定的项目 OF", `"op": "status"`} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("evidence prompt lacks %q:\n%s", want, evidence)
		}
	}
	unwired := r.query(context.Background(), intake.Project{Prefix: "ZY"}, intake.QueryRequest{Op: intake.QueryStatus})
	if unwired.Outcome != intake.OutcomeFailed {
		t.Fatalf("unwired=%+v", unwired)
	}
}

func TestIntakeTicketsRuntimeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		age          time.Duration
		owner, phase string
	}{
		{"fresh status fills owner and phase", 30 * time.Second, "处置Agent", "Agent处置"},
		{"stale status leaves them unknown", 121 * time.Second, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, store, _ := evidenceRuntime(t, recordedStatusTime.Add(tc.age), nil)
			tickets, err := intakeTickets(store, "/home/laoxu/github/orca-factory/", r.readers)(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := tickets[0]
			if got.Identifier != "OF-427" || !slices.Equal(got.Labels, []string{"auto-impl", "auto-failed"}) || got.Owner != tc.owner || got.Phase != tc.phase {
				t.Fatalf("ticket=%+v", got)
			}
		})
	}
}
