package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/executor"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type recoveredMergeOrca struct {
	executor.Orca
	t *testing.T
}

func (o *recoveredMergeOrca) RunUse(context.Context, string, string) (orca.Receipt, error) {
	return orca.Receipt{}, nil
}
func (o *recoveredMergeOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	return []orca.Task{{ID: "task_1", RunID: "run_1", Status: "completed", Result: json.RawMessage(`{"provenance":"worker_report","outcome":"succeeded"}`)}}, nil
}
func (o *recoveredMergeOrca) WorktreeSet(context.Context, orca.BoardCard) (orca.Receipt, error) {
	return orca.Receipt{}, nil
}
func (o *recoveredMergeOrca) WorkerRelease(context.Context, string, string) (orca.Receipt, error) {
	o.t.Fatal("invented dispatch authorized release")
	return nil, nil
}

type recoveredMergeTools struct {
	executor.Tools
	head, base string
}

func (x *recoveredMergeTools) Head(context.Context, string, string) (string, error) {
	return x.head, nil
}
func (x *recoveredMergeTools) Baseline(context.Context, string) (string, error) { return x.base, nil }
func (x *recoveredMergeTools) MergeBase(context.Context, string, string) (string, error) {
	return x.base, nil
}
func (x *recoveredMergeTools) Gate(context.Context, string, string, string) (string, error) {
	return "UNIT executor\nVERDICT green\nSTEP all\nREASON passed\nEVIDENCE\nSCOPE feature\nLANE 1", nil
}
func (x *recoveredMergeTools) Review(context.Context, string, string, string, *float64) (string, string, error) {
	return `{"verdict":"green","step":"review"}`, "default", nil
}

type recoveredMergePR struct {
	pr      github.PullRequest
	created bool
}

func (g *recoveredMergePR) FindOpenPR(context.Context, string, string, string) (*github.PullRequest, error) {
	if !g.created {
		return nil, nil
	}
	return &g.pr, nil
}
func (g *recoveredMergePR) CreatePR(context.Context, string, string, github.CreatePRRequest) (github.PullRequest, error) {
	g.created = true
	return g.pr, nil
}

type recoveredMergeLinear struct{ *mergeLinear }

func (l *recoveredMergeLinear) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (linear.Issue, error) {
	v, err := l.mergeLinear.UpdateIssue(ctx, id, state, add, remove)
	v.Labels.Nodes = []linear.NamedID{{Name: "auto-impl"}}
	return v, err
}

func TestRecoveredTaskWithoutDispatchReachesSchedulerMerge(t *testing.T) {
	f := newMergeFixture(t)
	f.linear.rows = nil
	root := t.TempDir()
	dir := filepath.Join(root, "r", "ENG-1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cp := map[string]any{"Run": "run_1", "Task": "task_1", "Worktree": f.config.Repositories[0].Path, "Baseline": f.main}
	b, bMarshalErr := json.Marshal(cp)
	if bMarshalErr != nil {
		t.Fatal(bMarshalErr)
	}
	if err := os.WriteFile(filepath.Join(dir, "executor.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	var pr github.PullRequest
	b, bMarshalErr = json.Marshal(f.prBody)
	if bMarshalErr != nil {
		t.Fatal(bMarshalErr)
	}
	if err := json.Unmarshal(b, &pr); err != nil {
		t.Fatal(err)
	}
	pr.HTMLURL = "https://example.test/pr/1"
	l := &recoveredMergeLinear{f.linear}
	e := &executor.Executor{Orca: &recoveredMergeOrca{t: t}, Linear: l, GitHub: &recoveredMergePR{pr: pr}, Tools: &recoveredMergeTools{head: f.head, base: f.main}, Config: executor.Config{Repo: f.config.Repositories[0].Path, Owner: "o", Repository: "r", Unit: "executor", Terminal: "term_new", Root: root}, Now: time.Now}
	ticket := executor.Ticket{ID: "id", Identifier: "ENG-1", Title: "test", Todo: "implement", Check: "true", Scope: "feature", Lane: 1}
	result, err := e.Start(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if result.DispatchID != "" || result.RunID != "run_1" {
		t.Fatalf("invented/lost association: %+v", result)
	}
	// Existing-PR recovery must retain the frozen task-only association too.
	// The first start handed ownership to the merge lane, so only a merging
	// executor may re-enter.
	e.Config.Merging = true
	result, err = e.Start(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if result.DispatchID != "" {
		t.Fatal("resume invented dispatch")
	}
	f.runtime.Clients[f.config.Repositories[0].Path] = l
	if err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.puts != 1 || f.linear.issue.State.Name != "Done" {
		t.Fatalf("valid task handoff stranded: puts=%d state=%s", f.puts, f.linear.issue.State.Name)
	}
}
