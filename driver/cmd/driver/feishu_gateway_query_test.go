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
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
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

// recordedTicket replays issues-read-OF-427.json: the ledger task as
// `agent-graph.sh issues read --id OF-427 --json` returned it, plus the
// owner and phase that read derived from the same comments.
func recordedTicket(t *testing.T) (issues.Task, string, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", "issues-read-OF-427.json"))
	if err != nil {
		t.Fatal(err)
	}
	var read struct {
		Result struct {
			Issue struct {
				issues.Task
				Owner string `json:"owner"`
				Phase string `json:"phase"`
			} `json:"issue"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &read); err != nil {
		t.Fatal(err)
	}
	issue := read.Result.Issue
	if issue.Metadata.Identifier != "OF-427" || len(issue.Comments.Nodes) == 0 || issue.Owner == "" {
		t.Fatalf("recorded ticket did not decode: %+v", issue.Metadata)
	}
	return issue.Task, issue.Owner, issue.Phase
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
	recorded, _, _ := recordedTicket(t)
	store := &evidenceStore{tasks: []issues.Task{recorded, labeledTask("OF-12", "Todo")},
		comments: map[string][]task.Comment{recorded.ID: recorded.Comments.Nodes}}
	r := newProjectRuntime("/usr/bin/true", t.TempDir(), config.Tuning{})
	r.readers = defaultEvidenceReaders()
	r.readers.logRoot = stateRoot
	r.readers.now = func() time.Time { return now }
	if env == nil {
		env = map[string]string{"AGENT_GH_REPO": "vmxmy/orca-factory"}
	}
	r.readers.profile = func(string) (map[string]string, time.Time, error) { return env, recordedStatusTime, nil }
	r.sources["OF"] = newQuerySource(store, "/home/laoxu/github/orca-factory", "github.com/vmxmy/orca-factory", env)
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
	t.Run("recorded ticket keeps labels and derives the recorded ownership", func(t *testing.T) {
		r, _, _ := evidenceRuntime(t, recordedStatusTime, nil)
		_, owner, phase := recordedTicket(t)
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryTicket, Ticket: "OF-427"})
		evidenceData(t, e, &detail)
		if e.Outcome != intake.OutcomeOK || !slices.Equal(detail.Labels, []string{"auto-failed", "auto-impl"}) || detail.State != "In Progress" || detail.Owner != owner || detail.Phase != phase || !detail.CommentsComplete {
			t.Fatalf("evidence=%+v detail=%+v recorded owner=%s phase=%s", e, detail, owner, phase)
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
	for _, want := range []string{`\"labels\":[\"auto-failed\",\"auto-impl\"]`, "不属于当前会话绑定的项目 OF", `"op": "status"`} {
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
			if got.Identifier != "OF-427" || !slices.Equal(got.Labels, []string{"auto-failed", "auto-impl"}) || got.Owner != tc.owner || got.Phase != tc.phase {
				t.Fatalf("ticket=%+v", got)
			}
		})
	}
}

// TestIntakeQueryLogsAttribution reads the recorded shared scheduler shapes:
// a decision record names the checkout path as repo, a merge-lane record
// names AGENT_GH_REPO as repository. Neither an unset AGENT_GH_REPO nor a
// repository-name log directory another bound project shares may attribute
// a record to this project.
func TestIntakeQueryLogsAttribution(t *testing.T) {
	decisionTime := time.Date(2026, 11, 9, 9, 50, 0, 0, time.FixedZone("CST", 8*3600))
	mergeTime := time.Date(2026, 11, 5, 13, 20, 0, 0, time.FixedZone("CST", 8*3600))
	shared := func(t *testing.T, root string, synthetic ...string) {
		t.Helper()
		var lines []string
		for _, name := range []string{"driver-journal-scheduler-decision.jsonl", "driver-logs-scheduler-repository.jsonl"} {
			b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", name))
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, strings.Split(strings.TrimSpace(string(b)), "\n")...)
		}
		writeLines(t, filepath.Join(root, "scheduler.log"), append(lines, synthetic...)...)
	}
	var data struct {
		Records []map[string]any `json:"records"`
	}
	t.Run("decision records by checkout path", func(t *testing.T) {
		r, _, root := evidenceRuntime(t, decisionTime, nil)
		// Synthetic: the same ticket decided for another checkout.
		shared(t, root, `{"time":"2026-11-09T09:45:20+08:00","msg":"scheduler decision","event":"launch","ticket":"OF-425","repo":"/home/laoxu/github/other"}`)
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryLogs, Ticket: "OF-425", Window: 30 * time.Minute})
		evidenceData(t, e, &data)
		if e.Outcome != intake.OutcomeOK || len(data.Records) != 2 || data.Records[0]["event"] != "heartbeat" || data.Records[1]["event"] != "launch" {
			t.Fatalf("evidence=%+v", e)
		}
	})
	t.Run("merge-lane records by repository", func(t *testing.T) {
		r, _, root := evidenceRuntime(t, mergeTime, nil)
		shared(t, root)
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryLogs, Ticket: "OF-384", Window: 30 * time.Minute})
		evidenceData(t, e, &data)
		if e.Outcome != intake.OutcomeOK || len(data.Records) != 1 || data.Records[0]["result"] != "green" {
			t.Fatalf("evidence=%+v", e)
		}
	})
	t.Run("unset AGENT_GH_REPO attributes nothing by repository", func(t *testing.T) {
		r, _, root := evidenceRuntime(t, mergeTime, map[string]string{})
		// Synthetic: a record with an empty repository.
		shared(t, root, `{"time":"2026-11-05T13:10:00+08:00","msg":"blank","ticket":"OF-384","repository":""}`)
		e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryLogs, Ticket: "OF-384", Window: 30 * time.Minute})
		if e.Outcome != intake.OutcomeEmpty || strings.Contains(e.Data, "merge gate finished") || strings.Contains(e.Data, "blank") {
			t.Fatalf("evidence=%+v", e)
		}
	})
	t.Run("log directory a disabled ledger project shares is not read", func(t *testing.T) {
		r, _, root := evidenceRuntime(t, time.Date(2026, 11, 7, 11, 40, 0, 0, time.FixedZone("CST", 8*3600)), nil)
		copyRecorded(t, "driver-logs-executor-auth-failure.jsonl", filepath.Join(root, "orca-factory", "OF-427", "executor.log"))
		own := issues.LedgerProject{Name: "orca-factory", Remote: "github.com/vmxmy/orca-factory", Prefix: "OF", Enabled: true}
		resolveIntakeProjects(context.Background(), projects.Result{}, []issues.LedgerProject{own}, r)
		logs := intake.QueryRequest{Op: intake.QueryLogs, Ticket: "OF-427", Window: time.Hour}
		if e := r.query(context.Background(), ofProject, logs); e.Outcome != intake.OutcomeOK || !strings.Contains(e.Data, "no oauth token") {
			t.Fatalf("evidence=%+v", e)
		}
		// Synthetic: a disabled ledger row whose repository is also named
		// orca-factory; the gateway restarted, so no source was ever resolved for it.
		other := issues.LedgerProject{Name: "fork", Remote: "github.com/acme/orca-factory", Prefix: "ZY", Enabled: false}
		resolveIntakeProjects(context.Background(), projects.Result{}, []issues.LedgerProject{own, other}, r)
		if e := r.query(context.Background(), ofProject, logs); e.Outcome != intake.OutcomeFailed || !strings.Contains(e.Error, "被多个项目共用") || strings.Contains(e.Data, "no oauth token") {
			t.Fatalf("evidence=%+v", e)
		}
	})
}

// TestIntakeQueryLogOwnersRecorded derives log directory owners from the
// recorded 项目 table row (feishu/bot-project-records.json) and the recorded
// `orca-ide repo list --json` (orca-repo-list.json) through Discover: each
// production directory has exactly its own repository, so executor logs stay
// readable; a recorded remote reappearing under a second owner is shared.
func TestIntakeQueryLogOwnersRecorded(t *testing.T) {
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
	var rows []issues.LedgerProject
	for _, row := range recording.Response.Data.Data {
		at := func(name string) any { return row[slices.Index(recording.Response.Data.Fields, name)] }
		rows = append(rows, issues.LedgerProject{Name: at("名称").(string), Enabled: at("启用").(bool), Remote: at("仓库远端").(string), Prefix: at("编号前缀").(string)})
	}
	fixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "recorded", "orca-repo-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	writeLines(t, script, "#!/bin/sh", "cat '"+fixture+"'")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	recorded, err := (&orca.Client{Path: script}).RepoList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var repos []orca.Repo
	for _, r := range recorded {
		path := filepath.Join(root, filepath.Base(r.Path))
		if r.CanonicalKey == "github.com/vmxmy/orca-factory" {
			writeLines(t, filepath.Join(path, "scripts", "agent-gates", "agent-graph.env"), "AGENT_ISSUE_BACKEND=feishu", "AGENT_GH_REPO=vmxmy/orca-factory")
		} else if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		repos = append(repos, orca.Repo{Path: path, CanonicalKey: r.CanonicalKey})
	}
	pass := func(context.Context, string) ([]string, error) { return nil, nil }
	result := projects.Discover(context.Background(), repos, rows, pass, nil)
	if len(rows) != 1 || len(result.Eligible) != 1 || len(result.Rejected) != len(recorded)-1 {
		t.Fatalf("rows=%+v result=%+v", rows, result)
	}
	owners := logDirOwners(result, rows)
	for _, r := range recorded {
		key := filepath.Base(r.Path)
		if len(owners[key]) != 1 || !owners[key][r.CanonicalKey] {
			t.Fatalf("owners[%s]=%v, want only %s", key, owners[key], r.CanonicalKey)
		}
	}
	// Synthetic: a second checkout of another owner's orca-factory.
	fork := projects.Rejection{Path: "/home/laoxu/github/fork/orca-factory", Remote: "github.com/acme/orca-factory", Reason: projects.ReasonOnboardable}
	shared := logDirOwners(projects.Result{Eligible: result.Eligible, Rejected: append(slices.Clone(result.Rejected), fork)}, rows)
	if len(shared["orca-factory"]) != 2 {
		t.Fatalf("owners=%v", shared["orca-factory"])
	}
}

// TestIntakeQueryConfigFollowsProfile rereads agent-graph.env at query time
// and refreshes the query source on every discovery round, so a changed
// limit is reported with the file's own modification time.
func TestIntakeQueryConfigFollowsProfile(t *testing.T) {
	repo := t.TempDir()
	profile := filepath.Join(repo, "scripts", "agent-gates", "agent-graph.env")
	write := func(limit string, modified time.Time) map[string]string {
		env := map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "vmxmy/orca-factory", "AGENT_ROLE_INTAKE": "claude:claude-opus-5-5:high", "AGENT_MAX_IN_FLIGHT": limit}
		var b strings.Builder
		for _, k := range []string{"AGENT_ISSUE_BACKEND", "AGENT_GH_REPO", "AGENT_ROLE_INTAKE", "AGENT_MAX_IN_FLIGHT"} {
			fmt.Fprintf(&b, "%s=%s\n", k, env[k])
		}
		writeLines(t, profile, strings.TrimSpace(b.String()))
		if err := os.Chtimes(profile, modified, modified); err != nil {
			t.Fatal(err)
		}
		return env
	}
	t.Setenv("AGENT_DRIVER_STATE_ROOT", t.TempDir())
	r := newProjectRuntime("/usr/bin/true", t.TempDir(), config.Tuning{})
	first := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	project := projects.Project{Path: repo, Remote: "github.com/vmxmy/orca-factory", Prefix: "OF", Name: "orca-factory", Repository: config.Repository{Env: write("2", first)}}
	if _, err := r.resolve(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	second := first.Add(time.Hour)
	write("5", second)
	e := r.query(context.Background(), ofProject, intake.QueryRequest{Op: intake.QueryConfig})
	if !strings.Contains(e.Data, `"AGENT_MAX_IN_FLIGHT":"5（agent-graph.env）"`) || !e.SourceGeneratedAt.Equal(second) {
		t.Fatalf("evidence=%+v", e)
	}
	project.Repository.Env = map[string]string{"AGENT_GH_REPO": "vmxmy/renamed"}
	if _, err := r.resolve(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	if got := r.sources["OF"]; got.ghRepo != "vmxmy/renamed" || got.logKey != "renamed" {
		t.Fatalf("source=%+v, want the current profile", got)
	}
}

// TestIntakeQueryAgentWiring runs projectRuntime.agent, the Turn dispatcher
// the gateway uses, over a built Runner whose claude replays the recorded
// intake-query-v1 reply: the gateway answers that request from the bound
// project and resumes the session, and only the final answer comes back.
func TestIntakeQueryAgentWiring(t *testing.T) {
	r, store, _ := evidenceRuntime(t, recordedStatusTime.Add(10*time.Second), nil)
	dir := t.TempDir()
	recorded, err := filepath.Abs(filepath.Join("..", "..", "testdata", "recorded", "claude-print-json-intake-query.json"))
	if err != nil {
		t.Fatal(err)
	}
	final := "OF-427 带 auto-failed，只由处置 Agent 处理，不进普通领票；快照不证明 worker 存活。"
	finalJSON, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": final})
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(dir, "final.json"), string(finalJSON))
	claude := filepath.Join(dir, "claude")
	writeLines(t, claude, "#!/bin/sh", "d=$(mktemp -d "+dir+"/call.XXXXXX)", "cat > $d/stdin",
		"if grep -q 'intake-query-v1 查询结果' $d/stdin; then exec cat "+filepath.Join(dir, "final.json")+"; else exec cat "+recorded+"; fi")
	bwrap := filepath.Join(dir, "bwrap")
	writeLines(t, bwrap, "#!/bin/sh", "while [ \"$1\" != -- ]; do shift; done", "shift", "exec \"$@\"")
	for _, path := range []string{claude, bwrap} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.built["OF"] = &intake.Runner{Claude: claude, Bwrap: bwrap, Checkout: t.TempDir(), Timeout: 10 * time.Second}
	answer, err := r.agent(context.Background(), intake.Turn{Session: "s-427", Resume: true, Prompt: "[需求人] OF-427 为什么一直没被 driver 领取？", Project: ofProject})
	if err != nil || answer != final {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
	if !slices.Equal(store.reads, []string{"OF-427"}) {
		t.Fatalf("ledger reads=%v", store.reads)
	}
	calls, err := filepath.Glob(filepath.Join(dir, "call.*", "stdin"))
	if err != nil || len(calls) != 2 {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
	var evidence string
	for _, call := range calls {
		b, err := os.ReadFile(call)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "intake-query-v1 查询结果") {
			evidence = string(b)
		}
	}
	for _, want := range []string{`"op": "ticket"`, `"op": "status"`, `"op": "logs"`, `"op": "maintenance"`, `\"labels\":[\"auto-failed\",\"auto-impl\"]`, "已用 4/4 次"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("evidence prompt lacks %q:\n%s", want, evidence)
		}
	}
}
