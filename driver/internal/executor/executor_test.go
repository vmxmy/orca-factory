package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type fakeOrca struct {
	workerRequests []string
	runRequests    []string
	used           []string
	start          orca.StartOptions
	objective      string
	releases       int
	boardUpdates   []string
	releaseErr     error
}

func (f *fakeOrca) RunCreate(_ context.Context, request, objective string) (orca.Receipt, error) {
	f.runRequests = append(f.runRequests, request)
	f.objective = objective
	return orca.Receipt{"runId": json.RawMessage(`"run_one"`)}, nil
}
func (f *fakeOrca) RunUse(_ context.Context, _, run string) (orca.Receipt, error) {
	f.used = append(f.used, run)
	return orca.Receipt{"runId": json.RawMessage(`"` + run + `"`)}, nil
}
func (f *fakeOrca) WorkerStart(_ context.Context, request string, o orca.StartOptions) (orca.Receipt, error) {
	f.workerRequests = append(f.workerRequests, request)
	f.start = o
	return orca.Receipt{"taskId": json.RawMessage(`"task_one"`), "dispatchId": json.RawMessage(`"ctx_one"`), "worktreePath": json.RawMessage(`"/worktrees/ZII-861-executor"`)}, nil
}
func (f *fakeOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	return []orca.Task{{ID: "task_one", RunID: "run_one", DispatchID: "ctx_one", Result: json.RawMessage(`{"provenance":"worker_report","outcome":"succeeded"}`)}}, nil
}
func (f *fakeOrca) WorkerStop(context.Context, string, string) (orca.Receipt, error) {
	return nil, errors.New("unexpected worker stop")
}
func (f *fakeOrca) WorkerRelease(_ context.Context, _, dispatch string) (orca.Receipt, error) {
	if dispatch != "ctx_one" {
		panic("wrong dispatch")
	}
	f.releases++
	return orca.Receipt{}, f.releaseErr
}

// fakeIssues serves the fixture ticket issue_one/ZII-861 as a managed open record.
type fakeIssues struct {
	issues.Store
	comments []string
}

func (f *fakeIssues) Repository() string { return "acme/demo" }
func (f *fakeIssues) Read(_ context.Context, id string) (issues.Task, error) {
	issue := task.Issue{ID: id, Identifier: "ZII-861"}
	return issues.Task{Issue: issue, URL: "https://github.com/acme/demo/issues/1", Complete: true, NativeState: "open", Metadata: issues.Metadata{Complete: true}}, nil
}

func (f *fakeIssues) ListComments(context.Context, string) ([]task.Comment, error) {
	var comments []task.Comment
	for _, body := range f.comments {
		comments = append(comments, task.Comment{Body: body})
	}
	return comments, nil
}

func (f *fakeIssues) CreateComment(_ context.Context, id, body string) (task.Comment, error) {
	if id != "issue_one" {
		panic("wrong issue")
	}
	f.comments = append(f.comments, body)
	return task.Comment{}, nil
}
func (f *fakeIssues) UpdateIssue(context.Context, string, string, []string, []string) (task.Issue, error) {
	return task.Issue{Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "auto-impl"}}}}, nil
}

type fakeGitHub struct {
	created  []github.CreatePRRequest
	existing *github.PullRequest
}

func (f *fakeGitHub) FindOpenPR(_ context.Context, owner, repo, head string) (*github.PullRequest, error) {
	if owner != "acme" || repo != "demo" || head != "agent/ZII-861/executor" {
		panic("wrong PR lookup")
	}
	return f.existing, nil
}
func (f *fakeGitHub) CreatePR(_ context.Context, owner, repo string, r github.CreatePRRequest) (github.PullRequest, error) {
	if owner != "acme" || repo != "demo" {
		panic("wrong repo")
	}
	f.created = append(f.created, r)
	return github.PullRequest{Number: 42, HTMLURL: "https://example.test/pr/42"}, nil
}

type fakeTools struct {
	head                   string
	gateCalls, reviewCalls int
	reviewBase             string
}

func (*fakeTools) Baseline(context.Context, string) (string, error)       { return "base-sha", nil }
func (f *fakeTools) Head(context.Context, string, string) (string, error) { return f.head, nil }
func (*fakeTools) ChangedFiles(context.Context, string, string, string) ([]string, error) {
	return []string{"unit/change.go"}, nil
}
func (f *fakeTools) Gate(_ context.Context, repo, head, spec string) (string, error) {
	f.gateCalls++
	b, e := os.ReadFile(spec)
	if e != nil || !strings.Contains(string(b), "CHECK: go test ./unit") {
		panic("missing gate spec")
	}
	return "UNIT      executor\nVERDICT   green\nSTEP      all\nREASON    all gates passed\nEVIDENCE  \nSCOPE     unit/*\nLANE      2\n", nil
}
func (*fakeTools) MergeBase(context.Context, string, string) (string, error) {
	return "merge-base-sha", nil
}
func (f *fakeTools) Review(_ context.Context, _, base, _ string, _ *float64) (string, string, error) {
	f.reviewBase = base
	f.reviewCalls++
	return "review notes\n{\"verdict\":\"green\",\"step\":\"review\"}", "default", nil
}
func fixture(t *testing.T, head string) (*Executor, *fakeOrca, *fakeIssues, *fakeGitHub, *fakeTools, Ticket) {
	t.Helper()
	o, l, g, tools := &fakeOrca{}, &fakeIssues{}, &fakeGitHub{}, &fakeTools{head: head}
	repo := t.TempDir()
	for _, role := range []string{"driver-worker.md", "correction.md", "reviewer.md"} {
		writeRole(t, repo, role, "role "+role+"\n")
	}
	e := &Executor{Orca: o, Issues: l, GitHub: g, Tools: tools, Config: Config{Repo: repo, Owner: "acme", Repository: "demo", Terminal: "term_exec", Unit: "executor", Root: t.TempDir()}, Now: func() time.Time { return time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC) }}
	ticket := Ticket{ID: "issue_one", Identifier: "ZII-861", Title: "执行器", Todo: "实现主路径", Check: "go test ./unit", Scope: "unit/*", Lane: 2}
	return e, o, l, g, tools, ticket
}
func TestMainPath(t *testing.T) {
	e, o, l, g, tools, ticket := fixture(t, "new-sha")
	result, err := e.Execute(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Owner != machine.OwnerMerge || result.State.Phase != machine.PhaseMergeQueue || result.PR.Number != 42 {
		t.Fatalf("result: %+v", result)
	}
	want := []string{"阶段: worker 运行中\n归属: 执行器 term=term_exec run=run_one attempt=1 since=2026-10-05T06:00:00Z", "阶段: 分支检查", "阶段: gate", "阶段: 审查", "阶段: 开 PR\n审查命令档位: default", "PR 已开: https://example.test/pr/42\n阶段: 排队合并\n归属: 合并通道 term=term_exec run=run_one attempt=1 since=2026-10-05T06:00:00Z"}
	legacyComments := append([]string(nil), l.comments...)
	for i, body := range legacyComments {
		legacyComments[i], _, _ = strings.Cut(body, "\nmerge-handoff-v1: ")
	}
	if !reflect.DeepEqual(legacyComments, want) {
		t.Fatalf("comments:\n%q\nwant:\n%q", l.comments, want)
	}
	if !reflect.DeepEqual(g.created, []github.CreatePRRequest{{Head: "agent/ZII-861/executor", Base: "main", Title: "ZII-861 执行器", Body: "关联票: https://github.com/acme/demo/issues/1\n\nCanonical identifier: ZII-861\nCanonical ID: issue_one\n\n实现主路径"}}) {
		t.Fatalf("PR input: %+v", g.created)
	}
	if o.releases != 1 || tools.gateCalls != 1 || tools.reviewCalls != 1 {
		t.Fatalf("release=%d gate=%d review=%d", o.releases, tools.gateCalls, tools.reviewCalls)
	}
	if tools.reviewBase != "merge-base-sha" {
		t.Fatalf("review base=%q; review must diff against the head's merge-base with current main, not the claim baseline", tools.reviewBase)
	}
	board := []string{"driver-worker driver: worker 运行中\nissue: ZII-861", "driver-branch-check driver: 分支检查\nissue: ZII-861", "driver-gate driver: gate\nissue: ZII-861", "driver-review driver: 审查\nissue: ZII-861", "driver-open-pr driver: 开 PR\nissue: ZII-861", "driver-merge-queue driver: 排队合并\nissue: ZII-861"}
	if !reflect.DeepEqual(o.boardUpdates, board) {
		t.Fatalf("board updates=%q", o.boardUpdates)
	}
	if o.objective != "ZII-861 执行器" || o.start.Run != "run_one" || o.start.Worktree != "new-top-level" || o.start.Repo != "path:"+e.Config.Repo {
		t.Fatalf("dispatch: %+v %s", o.start, o.objective)
	}
	b, err := os.ReadFile(filepath.Join(e.Config.Root, "demo", "ZII-861", "brief.md"))
	if err != nil || !strings.Contains(string(b), "实现主路径") || !strings.Contains(o.start.Spec, "agent/ZII-861/executor") {
		t.Fatalf("brief: %s %v", b, err)
	}
}

// observedWorktreeOrca matches real Orca: worker-start omits the worktree,
// which first appears in the fleet projection.
type observedWorktreeOrca struct {
	*fakeOrca
	taskReads int
}

func (o *observedWorktreeOrca) WorkerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	receipt, err := o.fakeOrca.WorkerStart(ctx, request, opts)
	delete(receipt, "worktreePath")
	return receipt, err
}
func (o *observedWorktreeOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	o.taskReads++
	if o.taskReads == 1 {
		return []orca.Task{{ID: "task_one", RunID: "run_one", DispatchID: "ctx_one"}}, nil
	}
	return o.fakeOrca.TaskList(ctx, run)
}
func (o *observedWorktreeOrca) Workers(context.Context, orca.ListOptions) ([]orca.Worker, error) {
	w := orca.Worker{DispatchID: "ctx_one"}
	w.Resource.WorktreeID = "repo::/worktrees/ZII-861-executor"
	return []orca.Worker{w}, nil
}
func TestBoardShowsWorkerOnceWorktreeObserved(t *testing.T) {
	e, o, _, _, _, ticket := fixture(t, "new-sha")
	e.Orca = &observedWorktreeOrca{fakeOrca: o}
	e.Sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(o.boardUpdates) == 0 || o.boardUpdates[0] != "driver-worker driver: worker 运行中\nissue: ZII-861" {
		t.Fatalf("board updates=%q", o.boardUpdates)
	}
}
func TestFakeSuccess(t *testing.T) {
	for _, head := range []string{"base-sha", ""} {
		t.Run(head, func(t *testing.T) {
			e, o, l, g, tools, ticket := fixture(t, head)
			result, err := e.Execute(context.Background(), ticket)
			if err != nil {
				t.Fatal(err)
			}
			if result.State.Owner != machine.OwnerResolver || result.State.Phase != machine.PhaseResolver {
				t.Fatalf("state: %+v", result.State)
			}
			if len(g.created) != 0 || tools.gateCalls != 0 || tools.reviewCalls != 0 || o.releases != 1 {
				t.Fatalf("PR=%v gate=%d review=%d release=%d", g.created, tools.gateCalls, tools.reviewCalls, o.releases)
			}
			want := []string{"阶段: worker 运行中\n归属: 执行器 term=term_exec run=run_one attempt=1 since=2026-10-05T06:00:00Z", "阶段: 分支检查", "Agent处置: worker reported success but branch did not advance\n证据: 分支检查\n处置要求: 检查 worker 分支并重新排队\n阶段: Agent处置\n归属: 处置Agent term=term_exec run=run_one attempt=1 since=2026-10-05T06:00:00Z"}
			if !reflect.DeepEqual(l.comments, want) {
				t.Fatalf("comments: %q", l.comments)
			}
		})
	}
}

func TestMainPathReviewerStdin(t *testing.T) {
	repo := t.TempDir()
	c := Commands{ReviewerCommand: "reviewer --quiet"}
	calls, stdinPath := 0, ""
	c.Run = func(_ context.Context, s runx.Spec) ([]byte, error) {
		calls++
		if s.Timeout <= 0 || (s.Path == "git") != (s.Dir == repo) {
			t.Fatalf("unbounded or misplaced command: %+v", s)
		}
		if s.Path == "git" {
			return []byte("diff --git a/unit/a b/unit/a\n+$(do-not-execute)\n"), nil
		}
		if s.Path != "/bin/sh" || len(s.Args) != 5 {
			t.Fatalf("review invocation: %+v", s)
		}
		if s.Args[3] != `reviewer --quiet` {
			t.Fatalf("review command is not passed unchanged: %q", s.Args[3])
		}
		stdinPath = s.Args[4]
		b, err := os.ReadFile(stdinPath)
		if err != nil || !strings.Contains(string(b), "diff --git a/unit/a b/unit/a\n+$(do-not-execute)\n") || !strings.Contains(string(b), "REVIEWER ROLE") {
			t.Fatalf("stdin = %q, %v", b, err)
		}
		return []byte(`{"verdict":"green","step":"review"}`), nil
	}
	c.Run = withReviewerRole(t, c.Run)
	out, tier, err := c.Review(context.Background(), repo, "base-sha", "new-sha", nil)
	if err != nil || tier != "default" || out != `{"verdict":"green","step":"review"}` || calls != 4 {
		t.Fatalf("review=%q calls=%d err=%v", out, calls, err)
	}
	files, err := os.ReadDir(repo)
	if _, statErr := os.Stat(stdinPath); err != nil || len(files) != 0 || !os.IsNotExist(statErr) {
		t.Fatalf("stdin file leaked: %v, %v, %v", files, err, statErr)
	}
}
func TestMainPathReusesPR(t *testing.T) {
	e, o, _, g, _, ticket := fixture(t, "new-sha")
	g.existing = &github.PullRequest{Number: 7, HTMLURL: "https://example.test/pr/7"}
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.PR.Number != 7 || len(g.created) != 0 || o.releases != 1 {
		t.Fatalf("result=%+v created=%v releases=%d err=%v", result, g.created, o.releases, err)
	}
}

// The release error is the real OF-22 receipt (ok:true, exit 1) replayed
// through the Orca client, so the warning shows what production logs show.
func TestMainPathReleaseUnknownWarnsState(t *testing.T) {
	e, o, _, g, tools, ticket := fixture(t, "new-sha")
	fixturePath, err := filepath.Abs("../../testdata/recorded/orca-worker-release-unknown.json")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat '"+fixturePath+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, o.releaseErr = (&orca.Client{Path: script}).WorkerRelease(context.Background(), orca.RequestID("ZII-861", "executor", "release", 1), "ctx_one")
	var warnings strings.Builder
	e.Stderr = &warnings
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || o.releases != 1 || len(g.created) != 1 || tools.gateCalls != 1 {
		t.Fatalf("release failure must not fail or replay: err=%v owner=%s calls=%d PR=%v gate=%d", err, result.State.Owner, o.releases, g.created, tools.gateCalls)
	}
	if !strings.Contains(warnings.String(), "worker release failed for dispatch ctx_one: orca command exited with state release_unknown") {
		t.Fatalf("warnings=%q", warnings.String())
	}
}

func TestMainPathTicketFields(t *testing.T) {
	for _, description := range []string{
		"## 要做\n实现主路径\n**CHECK:** `go test ./unit`\n**SCOPE:** `unit/*`",
		"## 要做\n实现主路径\n## CHECK\n```sh\ngo test ./unit\n```\n## SCOPE\nunit/*",
	} {
		ticket, err := ParseTicket("issue_one", "ZII-861", "执行器", description)
		want := Ticket{ID: "issue_one", Identifier: "ZII-861", Title: "执行器", Todo: "实现主路径", Check: "go test ./unit", Scope: "unit/*", Lane: 2}
		if err != nil || ticket != want {
			t.Fatalf("ticket=%+v err=%v", ticket, err)
		}
	}
}

func (f *fakeOrca) WorktreeSet(_ context.Context, card orca.BoardCard) (orca.Receipt, error) {
	f.boardUpdates = append(f.boardUpdates, card.Status+" "+card.Comment)
	return orca.Receipt{}, nil
}

func (*fakeOrca) TerminalList(context.Context) ([]orca.Terminal, error) { return nil, nil }

func (*fakeOrca) TerminalSend(context.Context, string, string) (orca.Receipt, error) {
	return nil, nil
}

func (f *fakeOrca) Workers(context.Context, orca.ListOptions) ([]orca.Worker, error) {
	return nil, nil
}
