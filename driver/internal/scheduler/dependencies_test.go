package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func dependencyIssue(id string, ready bool, blockers ...string) task.Issue {
	issue := task.Issue{ID: id, Identifier: "WAW-" + id, Title: "task " + id, Project: task.NamedID{ID: "p"}, State: task.NamedID{Name: "Todo"}}
	if ready {
		issue.Labels.Nodes = []task.NamedID{{Name: "ready-for-agent"}}
	}
	for _, id := range blockers {
		relation := task.InverseRelation{Type: "blocks"}
		relation.Issue.ID, relation.Issue.Identifier, relation.Issue.State.Type = id, "WAW-"+id, "unstarted"
		issue.InverseRelations.Nodes = append(issue.InverseRelations.Nodes, relation)
	}
	return issue
}

func dependencyRows(t *testing.T, input []task.Issue) ([]Ticket, []map[string]any) {
	t.Helper()
	store := &fakeIssues{issues: input}
	repo := config.Repository{Path: "/fixture/wecom-agent-workbench", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p", "AGENT_SCOPE_AVOID": "0"}}
	runtime := &Runtime{Clients: map[string]issues.Store{repo.Path: store}}
	tickets, err := runtime.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(ObservationRows(tickets, []config.Repository{repo}, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err = json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if store.updates != 0 || len(store.comments) != 0 {
		t.Fatal("read-only snapshot mutated ledger")
	}
	return tickets, rows
}

func TestDependenciesExplainHiddenRootAndCompletion(t *testing.T) {
	parent, child, leaf := dependencyIssue("522", false), dependencyIssue("523", true, "522"), dependencyIssue("529", true, "523")
	parent.Title, child.Title, leaf.Title = "hidden prerequisite", "ready implementation", "downstream verification"
	tickets, rows := dependencyRows(t, []task.Issue{parent, child, leaf})
	if len(tickets) != 2 || !tickets[0].Candidate || !tickets[0].Blocked || !tickets[1].Blocked {
		t.Fatalf("claim policy changed: %+v", tickets)
	}
	for _, row := range rows {
		deps, ok := row["dependencies"].([]any)
		if !ok || len(deps) != 1 {
			t.Fatalf("hidden root diagnostics missing: %s", mustDependencyJSON(row))
		}
		root := deps[0].(map[string]any)
		if root["identifier"] != "WAW-522" || root["title"] != "hidden prerequisite" || root["state"] != "Todo" || root["owner"] != "无" || root["candidate"] != false || root["status"] != "stalled" || root["reason"] != "missing ready-for-agent" || mustDependencyJSON(root["labels"]) != "[]" || mustDependencyJSON(root["affected"]) != `["WAW-523","WAW-529"]` {
			t.Fatalf("root=%s", mustDependencyJSON(root))
		}
	}
	var generated struct{ Tickets []map[string]any }
	if err := json.Unmarshal(recorded(t, "driver-status-json-dependencies.json"), &generated); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, generated.Tickets) {
		t.Fatalf("stored driver output differs from runtime: %s", mustDependencyJSON(rows))
	}
	parent.State.Name = "Done"
	child.InverseRelations.Nodes[0].Issue.State.Type = "completed"
	tickets, rows = dependencyRows(t, []task.Issue{parent, child, leaf})
	if tickets[0].Blocked {
		t.Fatal("completed parent still blocks child")
	}
	if _, ok := rows[0]["dependencies"]; ok {
		t.Fatalf("completed parent retained: %s", mustDependencyJSON(rows[0]))
	}
	if !strings.Contains(rows[1]["blockedReason"].(string), "WAW-523") {
		t.Fatal("leaf must still wait for child")
	}
}

func TestDependenciesBoundCyclesAndUnknownEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []task.Issue
		want  string
	}{
		{"missing", []task.Issue{dependencyIssue("523", true, "522")}, "dependency missing from snapshot"},
		{"cycle", []task.Issue{dependencyIssue("522", false, "523"), dependencyIssue("523", true, "522")}, "dependency cycle"},
		{"unknown state", []task.Issue{func() task.Issue { i := dependencyIssue("522", true); i.State.Name = ""; return i }(), dependencyIssue("523", true, "522")}, "unknown state"},
		{"incomplete", []task.Issue{func() task.Issue {
			i := dependencyIssue("522", false)
			i.InverseRelations.PageInfo.HasNextPage = true
			return i
		}(), dependencyIssue("523", true, "522")}, "dependency evidence incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rows := dependencyRows(t, tc.input)
			data := mustDependencyJSON(rows)
			if !strings.Contains(data, tc.want) {
				t.Fatalf("want %q: %s", tc.want, data)
			}
		})
	}
}

func mustDependencyJSON(value any) string { data, _ := json.Marshal(value); return string(data) }

func TestDependenciesBoundWideAndDeepGraphs(t *testing.T) {
	t.Run("wide missing roots", func(t *testing.T) {
		ids := make([]string, 1100)
		for index := range ids {
			ids[index] = fmt.Sprint(index)
		}
		_, rows := dependencyRows(t, []task.Issue{dependencyIssue("child", true, ids...)})
		deps := rows[0]["dependencies"].([]any)
		if len(deps) > 1001 || !strings.Contains(mustDependencyJSON(deps), "dependency traversal limit reached") {
			t.Fatalf("unbounded dependencies: count=%d", len(deps))
		}
	})
	t.Run("deep", func(t *testing.T) {
		input := []task.Issue{dependencyIssue("0", true, "1")}
		for index := 1; index < 70; index++ {
			input = append(input, dependencyIssue(fmt.Sprint(index), false, fmt.Sprint(index+1)))
		}
		_, rows := dependencyRows(t, input)
		if !strings.Contains(mustDependencyJSON(rows), "dependency traversal limit reached") {
			t.Fatal("deep dependency chain did not report bound")
		}
	})
}

func TestDependenciesReadyRootWaitsForAdmission(t *testing.T) {
	_, rows := dependencyRows(t, []task.Issue{dependencyIssue("522", true), dependencyIssue("523", true, "522")})
	root := rows[1]["dependencies"].([]any)[0].(map[string]any)
	if root["candidate"] != true || root["status"] != "waiting" || root["reason"] != "automatic candidate; admission checks still apply" {
		t.Fatalf("root=%s", mustDependencyJSON(root))
	}
}

func TestDependenciesOwnedRootIsNormalWaiting(t *testing.T) {
	root, child := dependencyIssue("522", false), dependencyIssue("523", true, "522")
	tickets := []Ticket{{ID: child.ID, Identifier: child.Identifier, Repo: "/repo", Blocked: true}}
	comments := map[string][]task.Comment{root.ID: {{Body: "阶段: worker 运行中\n归属: 执行器 term=worker run=run attempt=1"}}}
	dependencyDiagnostics([]task.Issue{root, child}, comments, tickets)
	dep := tickets[0].Dependencies[0]
	if dep.Status != "waiting" || dep.Owner != "执行器" || dep.Reason != "owned by 执行器" {
		t.Fatalf("normal owned root=%+v", dep)
	}
}

func TestDependenciesUnregisteredExecutorIsNotConfirmedWaiting(t *testing.T) {
	root, child := dependencyIssue("522", false), dependencyIssue("523", true, "522")
	tickets := []Ticket{{ID: child.ID, Identifier: child.Identifier, Repo: "/repo", Blocked: true}}
	comments := map[string][]task.Comment{root.ID: {{Body: "阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=1"}}}
	dependencyDiagnostics([]task.Issue{root, child}, comments, tickets)
	dep := tickets[0].Dependencies[0]
	if dep.Status != "unknown" || dep.Reason != "executor start unconfirmed: terminal/run not registered" {
		t.Fatalf("placeholder ownership reported as confirmed waiting: %+v", dep)
	}
	owner, err := InspectOwnership(comments[root.ID])
	if err != nil {
		t.Fatal(err)
	}
	row := ObservationRow(owner, nil, time.Now())
	data := mustDependencyJSON(row)
	if !strings.Contains(data, `"executionState":"unregistered"`) || row.BlockedReason != "executor start unconfirmed: terminal/run not registered" {
		t.Fatalf("owner row hides unconfirmed startup: %s", data)
	}
}

func TestDependenciesCycleKeepsKnownOwnership(t *testing.T) {
	root, child := dependencyIssue("522", false, "523"), dependencyIssue("523", true, "522")
	tickets := []Ticket{{ID: child.ID, Identifier: child.Identifier, Repo: "/repo", Blocked: true}}
	comments := map[string][]task.Comment{child.ID: {{Body: "阶段: worker 运行中\n归属: 执行器 term=worker run=run attempt=1"}}}
	dependencyDiagnostics([]task.Issue{root, child}, comments, tickets)
	dep := tickets[0].Dependencies[0]
	if dep.Status != "cycle" || dep.Owner != "执行器" || dep.State != "Todo" || !dep.Candidate {
		t.Fatalf("cycle overwrote known evidence: %+v", dep)
	}
}

func TestDependenciesClaimPushKeepsStartupUnconfirmed(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	s := setup(&fakeBackend{})
	s.pushTicket(Ticket{Identifier: "WAW-530", Repo: "a"}, Action{Kind: Claim, Phase: machine.PhaseWorker})
	data, err := os.ReadFile(status.PushPath())
	if err != nil {
		t.Fatal(err)
	}
	var view status.View
	if err = json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Tickets) != 1 || view.Tickets[0].ExecutionState != "unregistered" {
		t.Fatalf("claim push pretends executor registered: %s", data)
	}
}
