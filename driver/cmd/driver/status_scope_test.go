package main

import (
	"context"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/task"
	"testing"
)

type statusScopeStore struct{ statusDependencyStore }

func (s *statusScopeStore) Description(context.Context, string) (string, error) {
	return "## SCOPE\nsrc/*", nil
}
func (s *statusScopeStore) ListComments(_ context.Context, id string) ([]task.Comment, error) {
	if id == "owned" {
		return []task.Comment{{Body: "阶段: Agent处置\n归属: 处置Agent term=active run=run attempt=1"}}, nil
	}
	return nil, nil
}
func TestStatusFreshIncludesOwnedScopeWait(t *testing.T) {
	root := task.Issue{ID: "owned", Identifier: "WAW-530", Project: task.NamedID{ID: "p"}, State: task.NamedID{Name: "In Progress"}}
	child := root
	child.ID = "candidate"
	child.Identifier = "WAW-532"
	child.State.Name = "Todo"
	child.Labels.Nodes = []task.NamedID{{Name: "ready-for-agent"}}
	store := &statusScopeStore{statusDependencyStore{rows: []task.Issue{root, child}}}
	repo := config.Repository{Path: t.TempDir(), Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p"}}
	view, err := freshStatusSnapshot(context.Background(), repo, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Tickets) != 2 {
		t.Fatalf("view=%+v", view)
	}
	waits := view.Tickets[1].ScopeWaits
	if len(waits) != 1 || waits[0].Identifier != "WAW-530" || waits[0].CandidatePath != "src/*" || waits[0].OccupiedPath != "src/*" || waits[0].Source != "declared" || waits[0].Rule != "equal" {
		t.Fatalf("fresh lacks conservative fallback scope: %+v", view.Tickets[1])
	}
}
