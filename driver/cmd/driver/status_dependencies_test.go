package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestStatusTextExplainsDependencyBlock(t *testing.T) {
	t.Setenv("DRIVER_STATUS_FILE", t.TempDir()+"/status.json")
	if err := status.Write(status.Snapshot{GeneratedAt: time.Now(), Tickets: []status.Ticket{{Identifier: "WAW-523", Stage: "candidate", BlockedReason: "WAW-522: missing ready-for-agent"}}}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if code := showStatus(nil); code != 0 {
			t.Fatalf("code=%d", code)
		}
	})
	if !strings.Contains(string(out), "WAW-523") || !strings.Contains(string(out), "missing ready-for-agent") {
		t.Fatalf("hidden blocker not explained: %s", out)
	}
}

func TestStatusFreshNeverFallsBackToCachedGreen(t *testing.T) {
	t.Setenv("DRIVER_STATUS_FILE", t.TempDir()+"/status.json")
	if err := status.Write(status.Snapshot{GeneratedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	code := 0
	out := captureStdout(t, func() { code = showStatus([]string{"--fresh", "--repo", t.TempDir(), "--json"}) })
	if code != 1 || !strings.Contains(string(out), `"state":"unknown"`) {
		t.Fatalf("fresh read failure must be unknown, code=%d out=%s", code, out)
	}
}

type statusDependencyStore struct {
	issues.Store
	rows      []task.Issue
	readError error
}

func (s statusDependencyStore) Snapshot(context.Context, []string, time.Time) ([]task.Issue, error) {
	return s.rows, nil
}
func (s statusDependencyStore) ListComments(context.Context, string) ([]task.Comment, error) {
	return nil, s.readError
}

func TestStatusFreshReadsOwnershipAndPreservesErrors(t *testing.T) {
	repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p", "AGENT_SCOPE_AVOID": "0"}}
	parent := task.Issue{ID: "parent", Identifier: "WAW-522", Title: "hidden root", Project: task.NamedID{ID: "p"}, State: task.NamedID{Name: "Todo"}}
	child := parent
	child.ID, child.Identifier = "child", "WAW-523"
	child.Labels.Nodes = []task.NamedID{{Name: "ready-for-agent"}}
	relation := task.InverseRelation{Type: "blocks"}
	relation.Issue.ID, relation.Issue.Identifier, relation.Issue.State.Type = "parent", "WAW-522", "unstarted"
	child.InverseRelations.Nodes = []task.InverseRelation{relation}
	store := statusDependencyStore{rows: []task.Issue{parent, child}}
	view, err := freshStatusSnapshot(context.Background(), repo, &store)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Tickets) != 1 || len(view.Tickets[0].Dependencies) != 1 || view.Tickets[0].Dependencies[0].Status != "stalled" {
		t.Fatalf("view=%+v", view)
	}
	store.readError = errors.New("ownership read failed")
	view, err = freshStatusSnapshot(context.Background(), repo, &store)
	if err == nil || !strings.Contains(err.Error(), "ownership read failed") || view.State == "ok" {
		t.Fatalf("error suppressed: %v view=%+v", err, view)
	}
}
