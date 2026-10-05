package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

const followupBody = "## 后续工作\n- 标题：修复检查\n  证据：unit/a.go:12；检查缺失\n  要做：补充检查\n  CHECK：go test ./unit\n  SCOPE：unit/a.go\n  estimate：2\n"

type followupStore struct {
	issues  []FollowupIssue
	created []FollowupCreate
	related [][2]string
	fail    string
	now     time.Time
}

func (f *followupStore) FollowupProject(context.Context, string) (string, error) {
	if f.fail == "project" {
		return "", errors.New("secret")
	}
	return "project_one", nil
}
func (f *followupStore) ListFollowupIssues(context.Context) ([]FollowupIssue, error) {
	if f.fail == "list" {
		return nil, errors.New("secret")
	}
	return f.issues, nil
}
func (f *followupStore) CreateFollowup(_ context.Context, r FollowupCreate) (string, error) {
	if f.fail == "create" {
		return "", errors.New("secret")
	}
	f.created = append(f.created, r)
	id := fmt.Sprintf("new_%d", len(f.created))
	f.issues = append(f.issues, FollowupIssue{ID: id, Project: r.Project, Body: r.Body, Scope: r.Scope, StateType: "unstarted", CreatedAt: f.now, Machine: r.Machine})
	return id, nil
}
func (f *followupStore) RelateFollowup(_ context.Context, a, b string) error {
	if f.fail == "related" {
		return errors.New("secret")
	}
	f.related = append(f.related, [2]string{a, b})
	return nil
}

type followupChecks struct {
	reason string
	err    error
	calls  int
	globs  []string
}

func (f *followupChecks) ValidateFollowupScope(_ context.Context, _, _ string, g []string) (string, error) {
	f.globs = g
	return f.reason, f.err
}
func (f *followupChecks) FollowupCheckFails(ctx context.Context, _, _ string) (bool, error) {
	f.calls++
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Minute {
		panic("missing deadline")
	}
	return f.reason != "check passes", f.err
}
func followupFixture(t *testing.T) (*Executor, *followupStore, *followupChecks, *fakeLinear, Ticket) {
	t.Helper()
	e, _, l, _, _, ticket := fixture(t, "new-sha")
	s := &followupStore{now: e.Now()}
	c := &followupChecks{}
	e.Followups = s
	e.FollowupTools = c
	e.Config.Machine = "host_one"
	e.Config.Lane3Globs = []string{"secret/*"}
	return e, s, c, l, ticket
}

type followupOrca struct {
	fakeOrca
	body string
}

func (f *followupOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	b, err := json.Marshal(map[string]string{"provenance": "worker_report", "outcome": "succeeded", "body": f.body})
	if err != nil {
		return nil, err
	}
	return []orca.Task{{ID: "task_one", RunID: "run_one", DispatchID: "ctx_one", Result: b}}, nil
}

func TestFollowupDrafts(t *testing.T) {
	t.Run("real main checkout and glob semantics", testFollowupCommands)
	t.Run("parse section and fields", func(t *testing.T) {
		got := parseFollowupDrafts("- 标题：ignore\n" + followupBody + "## Other\n- 标题：ignore\n")
		want := []followupDraft{{Title: "修复检查", Evidence: "unit/a.go:12；检查缺失", Todo: "补充检查", Check: "go test ./unit", Scope: "unit/a.go", Estimate: "2"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("drafts: %#v", got)
		}
	})
	t.Run("ticket contents and relation", func(t *testing.T) {
		e, s, c, l, ticket := followupFixture(t)
		e.processFollowups(context.Background(), ticket, followupBody)
		want := []FollowupCreate{{Title: "修复检查", Body: "来源: ZII-861 的后续工作\n\n## 现状/证据\nunit/a.go:12；检查缺失\n\n## 要做\n补充检查\n\n## 验收\nCHECK: go test ./unit\n\n## SCOPE\nunit/a.go\n\n<!-- followup-machine: host_one -->", Scope: "unit/a.go", Project: "project_one", State: "Todo", Labels: []string{"ready-for-agent"}, Estimate: 2, Machine: "host_one"}}
		if !reflect.DeepEqual(s.created, want) || !reflect.DeepEqual(s.related, [][2]string{{"issue_one", "new_1"}}) || len(l.comments) != 0 || c.calls != 1 || !reflect.DeepEqual(c.globs, []string{"secret/*"}) {
			t.Fatalf("created=%+v related=%v comments=%v checks=%+v", s.created, s.related, l.comments, c)
		}
	})
	for _, tc := range []struct{ name, old, new, reason string }{
		{"missing field", "  要做：补充检查\n", "", "字段"}, {"bad evidence", "unit/a.go:12；检查缺失", "没有证据", "证据"}, {"bad estimate", "estimate：2", "estimate：4", "estimate"}, {"fraction estimate", "estimate：2", "estimate：1.5", "estimate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s, _, l, ticket := followupFixture(t)
			e.processFollowups(context.Background(), ticket, strings.Replace(followupBody, tc.old, tc.new, 1))
			if len(s.created) != 0 || len(l.comments) != 1 || !strings.Contains(l.comments[0], tc.reason) {
				t.Fatalf("created=%v comments=%v", s.created, l.comments)
			}
		})
	}
	for _, reason := range []string{"SCOPE 不存在", "SCOPE 命中 lane 3"} {
		t.Run(reason, func(t *testing.T) {
			e, s, c, l, ticket := followupFixture(t)
			c.reason = reason
			e.processFollowups(context.Background(), ticket, followupBody)
			if len(s.created) != 0 || len(l.comments) != 1 || c.calls != 0 {
				t.Fatalf("created=%v comments=%v", s.created, l.comments)
			}
		})
	}
	for _, tc := range []struct {
		name string
		err  error
	}{{"passes", nil}, {"timeout", context.DeadlineExceeded}, {"launch failure", errors.New("launch")}} {
		t.Run("check "+tc.name, func(t *testing.T) {
			e, s, _, l, ticket := followupFixture(t)
			e.FollowupTools = &checkResult{err: tc.err}
			e.processFollowups(context.Background(), ticket, followupBody)
			if len(s.created) != 0 || len(l.comments) != 1 {
				t.Fatalf("created=%v comments=%v", s.created, l.comments)
			}
		})
	}
	for _, state := range []string{"started", "completed", "canceled"} {
		t.Run("duplicate "+state, func(t *testing.T) {
			e, s, _, l, ticket := followupFixture(t)
			s.issues = []FollowupIssue{{Project: "project_one", Scope: "unit/a.go", StateType: state}}
			e.processFollowups(context.Background(), ticket, followupBody)
			want := 1
			if state == "started" {
				want = 0
			}
			if len(s.created) != want || len(l.comments) != 1-want {
				t.Fatalf("created=%v comments=%v", s.created, l.comments)
			}
		})
	}
	t.Run("source cap survives restart", func(t *testing.T) {
		e, s, _, l, ticket := followupFixture(t)
		s.issues = []FollowupIssue{{Body: "来源: ZII-861 的后续工作\n", StateType: "completed"}, {Body: "来源: ZII-861 的后续工作\n", StateType: "canceled"}}
		e.processFollowups(context.Background(), ticket, followupBody)
		if len(s.created) != 0 || len(l.comments) != 1 || !strings.Contains(l.comments[0], "2") {
			t.Fatalf("%v %v", s.created, l.comments)
		}
	})
	t.Run("source cap in report", func(t *testing.T) {
		e, s, _, l, ticket := followupFixture(t)
		body := followupBody + strings.ReplaceAll(strings.TrimPrefix(followupBody, "## 后续工作\n"), "unit/a.go", "unit/b.go") + strings.ReplaceAll(strings.TrimPrefix(followupBody, "## 后续工作\n"), "unit/a.go", "unit/c.go")
		e.processFollowups(context.Background(), ticket, body)
		if len(s.created) != 2 || len(l.comments) != 1 {
			t.Fatalf("%v %v", s.created, l.comments)
		}
	})
	t.Run("daily cap from Linear", func(t *testing.T) {
		e, s, _, l, ticket := followupFixture(t)
		for i := 0; i < 10; i++ {
			s.issues = append(s.issues, FollowupIssue{Body: "来源: ZII-999 的后续工作", CreatedAt: e.Now(), Machine: "host_one"})
		}
		e.processFollowups(context.Background(), ticket, followupBody)
		if len(s.created) != 0 || len(l.comments) != 1 || !strings.Contains(l.comments[0], "10") {
			t.Fatalf("%v %v", s.created, l.comments)
		}
	})
	t.Run("other days machines and unmarked tickets excluded", func(t *testing.T) {
		e, s, _, _, ticket := followupFixture(t)
		for i := 0; i < 10; i++ {
			s.issues = append(s.issues, FollowupIssue{Body: "来源: ZII-999 的后续工作", CreatedAt: e.Now().Add(-24 * time.Hour), Machine: "host_one"}, FollowupIssue{Body: "来源: ZII-999 的后续工作", CreatedAt: e.Now(), Machine: "host_two"}, FollowupIssue{Body: "ordinary", CreatedAt: e.Now(), Machine: "host_one"})
		}
		e.processFollowups(context.Background(), ticket, followupBody)
		if len(s.created) != 1 {
			t.Fatal(s.created)
		}
	})
	for _, stage := range []string{"project", "list", "create", "related"} {
		t.Run("failure does not change state "+stage, func(t *testing.T) {
			e, s, _, l, ticket := followupFixture(t)
			s.fail = stage
			e.Orca = &followupOrca{body: followupBody}
			result, err := e.Execute(context.Background(), ticket)
			if err != nil || result.State.Owner != machine.OwnerMerge {
				t.Fatalf("%+v %v", result, err)
			}
			found := 0
			for _, c := range l.comments {
				if strings.Contains(c, "后续工作") {
					found++
				}
				if strings.Contains(c, "secret") {
					t.Fatal("leaked error")
				}
			}
			if found != 1 {
				t.Fatal(l.comments)
			}
		})
	}
	t.Run("optional dependencies", func(t *testing.T) {
		e, s, _, _, ticket := followupFixture(t)
		e.FollowupTools = nil
		e.processFollowups(context.Background(), ticket, followupBody)
		if len(s.created) != 0 {
			t.Fatal(s.created)
		}
	})
}

type checkResult struct {
	followupChecks
	err error
}

func (f *checkResult) FollowupCheckFails(context.Context, string, string) (bool, error) {
	return false, f.err
}

func testFollowupCommands(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, b, err)
		}
		return string(b)
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	for _, path := range []string{"unit/a.go", "secret/deep/key.txt", "safe.txt"} {
		name := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("main"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "main")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "safe.txt"), []byte("worker"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &Commands{}
	for _, tc := range []struct {
		scope  string
		globs  []string
		reject bool
	}{
		{"unit/a.go/new.go", nil, true}, {"unit/a.go", nil, false}, {"unit/new.go", nil, false}, {"missing/new.go", nil, true}, {"../safe.txt", nil, true},
		{"secret/deep/key.txt", []string{"secret/*"}, true}, {"secret/deep/key.txt", []string{"secret/[a-z]*/k?y.txt"}, true},
		{"unit/*.go", []string{"secret/*"}, false}, {"secret/*", []string{"*/key.txt"}, true}, {"secret", []string{"*/key.txt"}, true},
	} {
		t.Run(tc.scope+strings.Join(tc.globs, ","), func(t *testing.T) {
			reason, err := c.ValidateFollowupScope(context.Background(), repo, tc.scope, tc.globs)
			if err != nil || (reason != "") != tc.reject {
				t.Fatalf("reason=%q err=%v", reason, err)
			}
		})
	}
	for _, tc := range []struct {
		check  string
		failed bool
	}{{"test $(cat safe.txt) = main", false}, {"test $(cat safe.txt) = worker", true}, {"exit 3", true}} {
		failed, err := c.FollowupCheckFails(context.Background(), repo, tc.check)
		if err != nil || failed != tc.failed {
			t.Fatalf("check %q failed=%v err=%v", tc.check, failed, err)
		}
		dirs, err := os.ReadDir(filepath.Join(repo, ".omc", "tmp"))
		if err != nil || len(dirs) != 0 {
			t.Fatalf("leftover dirs=%v err=%v", dirs, err)
		}
		if strings.Count(git("worktree", "list", "--porcelain"), "worktree ") != 1 {
			t.Fatal("worktree registration leaked")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	failed, err := c.FollowupCheckFails(ctx, repo, "sleep 5")
	if failed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: failed=%v err=%v", failed, err)
	}
	if strings.Count(git("worktree", "list", "--porcelain"), "worktree ") != 1 {
		t.Fatal("timeout leaked worktree")
	}
}
