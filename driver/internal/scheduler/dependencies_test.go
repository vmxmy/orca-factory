package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
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
	repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p", "AGENT_SCOPE_AVOID": "0"}}
	runtime := &Runtime{Clients: map[string]issues.Store{"/repo": store}}
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
		if root["identifier"] != "WAW-522" || root["title"] != "task 522" || root["state"] != "Todo" || root["owner"] != "无" || root["candidate"] != false || root["status"] != "stalled" || root["reason"] != "missing ready-for-agent" || mustDependencyJSON(root["labels"]) != "[]" || mustDependencyJSON(root["affected"]) != `["WAW-523","WAW-529"]` {
			t.Fatalf("root=%s", mustDependencyJSON(root))
		}
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
