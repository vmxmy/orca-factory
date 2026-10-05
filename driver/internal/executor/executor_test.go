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
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type fakeOrca struct {
	start      orca.StartOptions
	objective  string
	releases   int
	releaseErr error
}

func (f *fakeOrca) RunCreate(_ context.Context, _, objective string) (orca.Receipt, error) {
	f.objective = objective
	return orca.Receipt{"runId": json.RawMessage(`"run_one"`)}, nil
}
func (f *fakeOrca) WorkerStart(_ context.Context, _ string, o orca.StartOptions) (orca.Receipt, error) {
	f.start = o
	return orca.Receipt{"taskId": json.RawMessage(`"task_one"`), "dispatchId": json.RawMessage(`"ctx_one"`)}, nil
}
func (f *fakeOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	return []orca.Task{{ID: "task_one", RunID: "run_one", DispatchID: "ctx_one", Result: json.RawMessage(`{"provenance":"worker_report","outcome":"succeeded"}`)}}, nil
}
func (f *fakeOrca) WorkerRelease(_ context.Context, _, dispatch string) (orca.Receipt, error) {
	if dispatch != "ctx_one" {
		panic("wrong dispatch")
	}
	f.releases++
	return orca.Receipt{}, f.releaseErr
}

type fakeLinear struct{ comments []string }

func (f *fakeLinear) CreateComment(_ context.Context, id, body string) (linear.Comment, error) {
	if id != "issue_one" {
		panic("wrong issue")
	}
	f.comments = append(f.comments, body)
	return linear.Comment{}, nil
}
func (f *fakeLinear) UpdateIssue(context.Context, string, string, []string, []string) (linear.Issue, error) {
	return linear.Issue{Labels: linear.Connection[linear.NamedID]{Nodes: []linear.NamedID{{Name: "auto-impl"}}}}, nil
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
}

func (*fakeTools) Baseline(context.Context, string) (string, error)       { return "base-sha", nil }
func (f *fakeTools) Head(context.Context, string, string) (string, error) { return f.head, nil }
func (f *fakeTools) Gate(_ context.Context, repo, head, spec string) (string, error) {
	f.gateCalls++
	b, e := os.ReadFile(spec)
	if e != nil || !strings.Contains(string(b), "CHECK: go test ./unit") {
		panic("missing gate spec")
	}
	return "UNIT      executor\nVERDICT   green\nSTEP      all\nREASON    all gates passed\nEVIDENCE  \nSCOPE     unit/*\nLANE      2\n", nil
}
func (f *fakeTools) Review(context.Context, string, string, string) (string, error) {
	f.reviewCalls++
	return "review notes\n{\"verdict\":\"green\",\"step\":\"review\"}", nil
}
func fixture(t *testing.T, head string) (*Executor, *fakeOrca, *fakeLinear, *fakeGitHub, *fakeTools, Ticket) {
	t.Helper()
	o, l, g, tools := &fakeOrca{}, &fakeLinear{}, &fakeGitHub{}, &fakeTools{head: head}
	e := &Executor{Orca: o, Linear: l, GitHub: g, Tools: tools, Config: Config{Repo: "/repo/demo", Owner: "acme", Repository: "demo", Terminal: "term_exec", Unit: "executor", Root: t.TempDir()}, Now: func() time.Time { return time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC) }}
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
	want := []string{"阶段: worker 运行中\n归属: 执行器 term=term_exec run=run_one attempt=1 since=2026-10-05T06:00:00Z", "阶段: 分支检查", "阶段: gate", "阶段: 审查", "阶段: 开 PR", "PR 已开: https://example.test/pr/42\n阶段: 排队合并\n归属: 合并通道 term=term_exec run=run_one attempt=1 since=2026-10-05T06:00:00Z"}
	if !reflect.DeepEqual(l.comments, want) {
		t.Fatalf("comments:\n%q\nwant:\n%q", l.comments, want)
	}
	if !reflect.DeepEqual(g.created, []github.CreatePRRequest{{Head: "agent/ZII-861/executor", Base: "main", Title: "ZII-861 执行器", Body: "Fixes ZII-861\n\n实现主路径"}}) {
		t.Fatalf("PR input: %+v", g.created)
	}
	if o.releases != 1 || tools.gateCalls != 1 || tools.reviewCalls != 1 {
		t.Fatalf("release=%d gate=%d review=%d", o.releases, tools.gateCalls, tools.reviewCalls)
	}
	if o.objective != "ZII-861 执行器" || o.start.Run != "run_one" || o.start.Worktree != "new-top-level" || o.start.Repo != "path:/repo/demo" {
		t.Fatalf("dispatch: %+v %s", o.start, o.objective)
	}
	b, err := os.ReadFile(filepath.Join(e.Config.Root, "demo", "ZII-861", "brief.md"))
	if err != nil || !strings.Contains(string(b), "实现主路径") || !strings.Contains(o.start.Spec, "agent/ZII-861/executor") {
		t.Fatalf("brief: %s %v", b, err)
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
			if result.State.Owner != machine.OwnerHuman || result.State.Phase != machine.PhaseHuman {
				t.Fatalf("state: %+v", result.State)
			}
			if len(g.created) != 0 || tools.gateCalls != 0 || tools.reviewCalls != 0 || o.releases != 1 {
				t.Fatalf("PR=%v gate=%d review=%d release=%d", g.created, tools.gateCalls, tools.reviewCalls, o.releases)
			}
			want := []string{"阶段: worker 运行中\n归属: 执行器 term=term_exec run=run_one attempt=1 since=2026-10-05T06:00:00Z", "阶段: 分支检查", "转人: worker reported success but branch did not advance\n证据: 分支检查\n需要你: 检查 worker 分支并重新排队\n阶段: 转人\n归属: 人 term=term_exec run=run_one attempt=1 since=2026-10-05T06:00:00Z"}
			if !reflect.DeepEqual(l.comments, want) {
				t.Fatalf("comments: %q", l.comments)
			}
		})
	}
}

func TestMainPathReviewerStdin(t *testing.T) {
	repo := t.TempDir()
	c := Commands{ReviewerCommand: "reviewer --quiet"}
	calls := 0
	c.Run = func(_ context.Context, s runx.Spec) ([]byte, error) {
		calls++
		if s.Timeout <= 0 || s.Dir != repo {
			t.Fatalf("unbounded or misplaced command: %+v", s)
		}
		if s.Path == "git" {
			return []byte("diff --git a/unit/a b/unit/a\n+$(do-not-execute)\n"), nil
		}
		if s.Path != "/bin/sh" || len(s.Args) != 6 {
			t.Fatalf("review invocation: %+v", s)
		}
		if s.Args[3] != `reviewer --quiet "$1"` {
			t.Fatalf("review instruction is not passed to command: %q", s.Args[3])
		}
		b, err := os.ReadFile(s.Args[4])
		if err != nil || string(b) != "diff --git a/unit/a b/unit/a\n+$(do-not-execute)\n" {
			t.Fatalf("stdin = %q, %v", b, err)
		}
		if !strings.Contains(s.Args[5], "End stdout with JSON") {
			t.Fatalf("reviewer lacks verdict contract")
		}
		return []byte(`{"verdict":"green","step":"review"}`), nil
	}
	out, err := c.Review(context.Background(), repo, "base-sha", "new-sha")
	if err != nil || out != `{"verdict":"green","step":"review"}` || calls != 2 {
		t.Fatalf("review=%q calls=%d err=%v", out, calls, err)
	}
	files, err := os.ReadDir(repo)
	if err != nil || len(files) != 0 {
		t.Fatalf("stdin file leaked: %v, %v", files, err)
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

func TestMainPathReleaseFailure(t *testing.T) {
	e, o, _, g, tools, ticket := fixture(t, "new-sha")
	o.releaseErr = errors.New("secret release detail")
	_, err := e.Execute(context.Background(), ticket)
	if err == nil || strings.Contains(err.Error(), "secret") || o.releases != 1 || len(g.created) != 0 || tools.gateCalls != 0 {
		t.Fatalf("release must not be replayed after uncertainty: err=%v calls=%d PR=%v gate=%d", err, o.releases, g.created, tools.gateCalls)
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

func (*fakeOrca) TerminalList(context.Context) ([]orca.Terminal, error) { return nil, nil }
