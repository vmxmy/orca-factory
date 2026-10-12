package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/executor"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type linearGuardTransport struct {
	t    *testing.T
	next http.RoundTripper
}

func (g linearGuardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Host, "linear.app") {
		g.t.Fatalf("GitHub pipeline reached Linear endpoint %s", r.URL)
	}
	return g.next.RoundTrip(r)
}

type e2eExecutorOrca struct {
	*recoveredMergeOrca
	worktree          string
	runRequests       []string
	workers, released int
}

func (o *e2eExecutorOrca) RunCreate(_ context.Context, request, _ string) (orca.Receipt, error) {
	o.runRequests = append(o.runRequests, request)
	return orca.Receipt{"runId": json.RawMessage(`"run_1"`)}, nil
}
func (o *e2eExecutorOrca) WorkerStart(context.Context, string, orca.StartOptions) (orca.Receipt, error) {
	o.workers++
	path, err := json.Marshal(o.worktree)
	if err != nil {
		return nil, err
	}
	return orca.Receipt{"taskId": json.RawMessage(`"task_1"`), "dispatchId": json.RawMessage(`"dispatch_1"`), "worktreePath": path}, nil
}
func (o *e2eExecutorOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	return []orca.Task{{ID: "task_1", RunID: "run_1", DispatchID: "dispatch_1", Status: "completed", Result: json.RawMessage(`{"provenance":"worker_report","outcome":"succeeded"}`)}}, nil
}
func (o *e2eExecutorOrca) WorkerRelease(_ context.Context, _, dispatch string) (orca.Receipt, error) {
	if dispatch != "dispatch_1" {
		o.t.Fatalf("executor released unknown dispatch %q", dispatch)
	}
	o.released++
	return orca.Receipt{}, nil
}
func (o *e2eExecutorOrca) Workers(context.Context, orca.ListOptions) ([]orca.Worker, error) {
	return nil, nil
}
func (o *e2eExecutorOrca) TerminalList(context.Context) ([]orca.Terminal, error) { return nil, nil }

type e2eExecutorTools struct{ *recoveredMergeTools }

func (x e2eExecutorTools) ChangedFiles(context.Context, string, string, string) ([]string, error) {
	return []string{"feature"}, nil
}

type e2eSchedulerOrca struct {
	*fakeOrca
	start        func(context.Context, string) error
	releases     []string
	worktreeSets []string
	dispatched   bool
}

func (o *e2eSchedulerOrca) TerminalCreate(ctx context.Context, opts orca.TerminalOptions) (orca.Receipt, error) {
	o.launches = append(o.launches, opts)
	identifier := strings.TrimPrefix(opts.Title, "driver exec ")
	if err := o.start(ctx, identifier); err != nil {
		return nil, err
	}
	o.dispatched = true
	return orca.Receipt{"terminal": json.RawMessage(`{"handle":"term_new"}`)}, nil
}
func (o *e2eSchedulerOrca) WorkerList(_ context.Context, opts orca.ListOptions) (orca.Receipt, error) {
	workers := "[]"
	if o.dispatched && opts.Run == "run_1" {
		workers = `[{"dispatchId":"dispatch_1","dispatchStatus":"completed","terminalState":"exited"}]`
	}
	return receipt(fmt.Sprintf(`{"workers":%s,"scope":{"run":%q},"page":{"hasMore":false}}`, workers, opts.Run)), nil
}
func (o *e2eSchedulerOrca) WorkerRelease(_ context.Context, _, dispatch string) (orca.Receipt, error) {
	o.releases = append(o.releases, dispatch)
	return orca.Receipt{}, nil
}
func (o *e2eSchedulerOrca) WorktreeSet(_ context.Context, path, _, status, _ string) (orca.Receipt, error) {
	o.worktreeSets = append(o.worktreeSets, path+" "+status)
	return orca.Receipt{}, nil
}

func TestGitHubIsolatedPipelineEndToEnd(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	ctx := context.Background()
	f := newMergeFixture(t)
	repo := f.config.Repositories[0]
	gitMergeTest(t, repo.Path, "checkout", "main")
	g := githubSafetyFixture(t, f)
	previous := http.DefaultTransport
	http.DefaultTransport = linearGuardTransport{t: t, next: previous}
	t.Cleanup(func() { http.DefaultTransport = previous })

	meta, err := json.Marshal(issues.Metadata{ID: safetyID, Identifier: "ENG-1", Project: "o/r", State: "Todo", Complete: true, Blockers: []string{}, Related: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.comments = nil
	g.row["title"] = "feature"
	g.row["html_url"] = "https://github.example/o/r/issues/42"
	g.row["body"] = "## 要做\n实现 feature\n\nCHECK: true\nSCOPE: feature\n\n<!-- orca-task-v1: " + string(meta) + " -->"
	g.row["labels"] = []any{map[string]any{"name": "ready-for-agent"}}
	g.mu.Unlock()

	client := f.runtime.Clients[repo.Path].(*issues.GitHub)
	if err = f.runtime.initialize(ctx, f.config, func(context.Context, string) (LinearClient, error) {
		t.Fatal("GitHub pipeline initialized a Linear client")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	f.runtime.Clients[repo.Path] = client
	f.runtime.DriverPath = "/opt/driver/bin/driver"

	var pr github.PullRequest
	raw, err := json.Marshal(f.prBody)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &pr); err != nil {
		t.Fatal(err)
	}
	pr.HTMLURL = "https://example.test/pr/1"
	prs := &recoveredMergePR{pr: pr}
	execOrca := &e2eExecutorOrca{recoveredMergeOrca: &recoveredMergeOrca{t: t}, worktree: repo.Path}
	var results []executor.Result
	schedOrca := &e2eSchedulerOrca{fakeOrca: &fakeOrca{automations: receipt(`{"automations":[]}`)}}
	schedOrca.start = func(ctx context.Context, identifier string) error {
		task, readErr := client.Read(ctx, identifier)
		if readErr != nil {
			return readErr
		}
		ticket, parseErr := executor.ParseTicket(task.ID, task.Identifier, task.Title, task.Description)
		if parseErr != nil {
			return parseErr
		}
		e := &executor.Executor{Orca: execOrca, Linear: client, GitHub: prs, Tools: e2eExecutorTools{&recoveredMergeTools{head: f.head, base: f.main}}, Config: executor.Config{Repo: repo.Path, Owner: "o", Repository: "r", Unit: "executor", Terminal: "term_new", Root: t.TempDir(), PollInterval: time.Millisecond}, Now: time.Now}
		result, startErr := e.Start(ctx, ticket)
		results = append(results, result)
		return startErr
	}
	f.runtime.Orca = schedOrca
	s := &Scheduler{Config: f.config, Backend: f.runtime}

	if err = s.Tick(ctx); err != nil {
		t.Fatalf("claim and launch tick: %v", err)
	}
	if len(schedOrca.launches) != 1 || !strings.Contains(schedOrca.launches[0].Command, " exec 'ENG-1'") {
		t.Fatalf("launch: %+v", schedOrca.launches)
	}
	if len(results) != 1 || results[0].State.Owner != machine.OwnerMerge || results[0].State.Phase != machine.PhaseMergeQueue {
		t.Fatalf("executor did not hand off to merge lane: %+v", results)
	}
	runIdentities := map[string]bool{}
	for _, request := range execOrca.runRequests {
		runIdentities[request] = true
	}
	if len(runIdentities) != 1 || execOrca.workers != 1 || execOrca.released != 1 || !prs.created || f.puts != 0 {
		t.Fatalf("first tick: runs=%v workers=%d released=%d pr=%v squash=%d", execOrca.runRequests, execOrca.workers, execOrca.released, prs.created, f.puts)
	}
	history, err := client.ListComments(ctx, safetyID)
	if err != nil {
		t.Fatal(err)
	}
	frozen := Ticket{ID: safetyID, Identifier: "ENG-1"}
	readMergeHandoff(&frozen, history)
	if frozen.Handoff == nil || frozen.Handoff.Head != f.head || frozen.Handoff.Baseline != f.main || frozen.Handoff.PR != 1 || frozen.MergeProblem != "" {
		t.Fatalf("frozen handoff: %+v problem=%q", frozen.Handoff, frozen.MergeProblem)
	}

	if err = s.Tick(ctx); err != nil {
		t.Fatalf("merge tick: %v", err)
	}
	final, err := client.Read(ctx, safetyID)
	if err != nil {
		t.Fatal(err)
	}
	if f.puts != 1 || !f.merged || final.State.Name != "Done" || final.Identifier != "ENG-1" {
		t.Fatalf("merge: squash=%d merged=%v final=%+v", f.puts, f.merged, final)
	}
	history, err = client.ListComments(ctx, safetyID)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := InspectOwnership(history)
	if err != nil || ownership.Owner != machine.OwnerNone {
		t.Fatalf("ownership not released: %+v %v", ownership, err)
	}
	if strings.Join(schedOrca.releases, ",") != "dispatch_1" || len(schedOrca.worktreeSets) != 1 || schedOrca.worktreeSets[0] != repo.Path+" completed" {
		t.Fatalf("cleanup: releases=%v worktree=%v", schedOrca.releases, schedOrca.worktreeSets)
	}
	if branch := gitMergeTest(t, repo.Path, "ls-remote", "--heads", "origin", "refs/heads/agent/ENG-1/executor"); branch != "" {
		t.Fatalf("remote branch survived cleanup: %s", branch)
	}

	if err = s.Tick(ctx); err != nil {
		t.Fatalf("idle tick: %v", err)
	}
	if len(schedOrca.launches) != 1 || execOrca.workers != 1 || f.puts != 1 {
		t.Fatalf("replayed: launches=%d workers=%d squash=%d", len(schedOrca.launches), execOrca.workers, f.puts)
	}
}
