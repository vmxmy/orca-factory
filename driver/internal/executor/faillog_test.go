package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type failLogIssues struct {
	fakeIssues
	readErr error
	reads   int
}

func (l *failLogIssues) ListComments(context.Context, string) ([]task.Comment, error) {
	l.reads++
	var comments []task.Comment
	for _, body := range l.comments {
		comments = append(comments, task.Comment{Body: body})
	}
	return comments, l.readErr
}

type failLogTools struct {
	*recoveryTools
	heads                []string
	files                []string
	diffs                [][2]string
	diffErr              error
	gateStep, reviewFile string
}

func (f *failLogTools) Head(ctx context.Context, repo, branch string) (string, error) {
	if len(f.heads) > 0 {
		head := f.heads[0]
		f.heads = f.heads[1:]
		return head, nil
	}
	return f.recoveryTools.Head(ctx, repo, branch)
}
func (f *failLogTools) ChangedFiles(_ context.Context, _, base, head string) ([]string, error) {
	f.diffs = append(f.diffs, [2]string{base, head})
	return f.files, f.diffErr
}
func (f *failLogTools) Gate(ctx context.Context, repo, head, spec string) (string, error) {
	red := f.gateReds > 0
	output, err := f.recoveryTools.Gate(ctx, repo, head, spec)
	if red {
		return "UNIT executor\nVERDICT red\nSTEP " + f.gateStep + "\nREASON external-secret\n", err
	}
	return output, err
}
func (f *failLogTools) Review(ctx context.Context, repo, base, head string, estimate *float64) (string, string, error) {
	red := f.reviewReds > 0
	output, tier, err := f.recoveryTools.Review(ctx, repo, base, head, estimate)
	if red {
		return `review notes\n{"verdict":"red","step":"review","reason":"external-secret","evidence":[{"file":"` + f.reviewFile + `","line":381,"note":"external-secret"}]}`, tier, err
	}
	return output, tier, err
}
func failLogToolFixture(e *Executor, tools *recoveryTools) *failLogTools {
	tools.head = "abc1234567890"
	f := &failLogTools{recoveryTools: tools, files: []string{"driver/internal/executor/executor.go", "driver/internal/executor/faillog.go"}, gateStep: "check_shell", reviewFile: "driver/internal/executor/executor.go"}
	e.Tools = f
	return f
}

type failLogRecovery struct {
	recoveryService
	specs []string
}

func (r *failLogRecovery) ChildTask(ctx context.Context, run, parent, spec, request string) (string, error) {
	r.specs = append(r.specs, spec)
	return r.recoveryService.ChildTask(ctx, run, parent, spec, request)
}
func failLogLines(body string) []string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "FAIL: ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestFailLogStageComments(t *testing.T) {
	for _, tc := range []struct {
		cause    string
		comments int
	}{{"worker failed", 8}, {"gate red", 10}, {"reviewer red", 11}} {
		t.Run(tc.cause, func(t *testing.T) {
			e, o, tools, _, _, ticket := recoveryFixture(t)
			e.Config.Escalate = "0"
			f := failLogToolFixture(e, tools)
			l := &failLogIssues{}
			e.Issues = l
			switch tc.cause {
			case "worker failed":
				o.failFirst = true
			case "gate red":
				tools.gateReds = 1
			case "reviewer red":
				tools.reviewReds = 1
			}
			result, err := e.Execute(context.Background(), ticket)
			if err != nil || result.State.Owner != machine.OwnerMerge {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if len(l.comments) != tc.comments {
				t.Fatalf("CreateComment calls=%d want=%d", len(l.comments), tc.comments)
			}
			var summaries []string
			for _, body := range l.comments {
				lines := failLogLines(body)
				if len(lines) > 0 && !strings.Contains(body, "阶段: "+string(machine.PhaseCorrection)) {
					t.Fatalf("standalone FAIL comment: %q", body)
				}
				summaries = append(summaries, lines...)
			}
			if len(summaries) != 1 {
				t.Fatalf("FAIL summaries=%q", summaries)
			}
			line := summaries[0]
			if !strings.HasPrefix(line, "FAIL: approach=") || !strings.Contains(line, "head=abc1234") || !strings.Contains(line, "driver/internal/executor/faillog.go") || strings.Contains(line, ticket.Todo) || !strings.Contains(line, " | reason="+tc.cause+" | evidence=") {
				t.Fatalf("invalid summary: %q", line)
			}
			// Before the branch check no merge-base exists yet; afterwards the summary uses it.
			base := "merge-base-sha"
			if tc.cause == "worker failed" {
				base = "base-sha"
			}
			if len(f.diffs) != 1 || f.diffs[0] != [2]string{base, "abc1234567890"} {
				t.Fatalf("diff queries=%v", f.diffs)
			}
			if tc.cause == "worker failed" && !strings.HasSuffix(line, " | evidence=worker_done outcome=failed") {
				t.Fatalf("worker evidence: %q", line)
			}
			if tc.cause == "gate red" && !strings.HasSuffix(line, " | evidence=check_shell") {
				t.Fatalf("gate evidence: %q", line)
			}
			if tc.cause == "reviewer red" && !strings.HasSuffix(line, " | evidence=driver/internal/executor/executor.go:381") {
				t.Fatalf("review evidence: %q", line)
			}
			if strings.Contains(line, "external-secret") {
				t.Fatalf("raw failure leaked: %q", line)
			}
			b, err := os.ReadFile(filepath.Join(e.Config.Root, e.Config.Repository, ticket.Identifier, "fail.log"))
			if err != nil || string(b) != line+"\n" {
				t.Fatalf("local FAIL=%q err=%v", b, err)
			}
		})
	}
}
func TestFailLogFormatLimit(t *testing.T) {
	e, _, tools, _, _, ticket := recoveryFixture(t)
	l := &failLogIssues{}
	e.Issues = l
	e.Config.Escalate = "0"
	f := failLogToolFixture(e, tools)
	f.files = append(f.files, strings.Repeat("a", 600))
	ticket.Todo = strings.Repeat("做法", 400) + "\nFAIL: injected | reason=fake"
	tools.gateReds = 1
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	lines := failLogLines(strings.Join(l.comments, "\n"))
	if len(lines) != 1 {
		t.Fatalf("FAIL lines=%q", lines)
	}
	line := lines[0]
	if !utf8.ValidString(line) || utf8.RuneCountInString(line) > 300 || !strings.HasPrefix(line, "FAIL: approach=") || !strings.Contains(line, " | reason=gate red | evidence=check_shell") {
		t.Fatalf("invalid bounded summary: %q", line)
	}
}
func TestFailLogAllHistory(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	e.Config.Escalate = "0"
	o.failFirst = true
	tools.gateReds, tools.reviewReds = 1, 1
	l, r := &failLogIssues{}, &failLogRecovery{}
	e.Issues, e.Recovery = l, r
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	lines := failLogLines(strings.Join(l.comments, "\n"))
	if len(lines) != 3 || len(r.requests) != 3 || len(r.specs) != 3 {
		t.Fatalf("lines=%q requests=%d specs=%d", lines, len(r.requests), len(r.specs))
	}
	if lines[0] == lines[1] || lines[1] == lines[2] {
		t.Fatalf("different failures have identical summaries: %q", lines)
	}
	for i, req := range r.requests {
		for _, brief := range []string{req.Brief, r.specs[i]} {
			if !strings.Contains(brief, "不得重复以上已失败的做法") {
				t.Fatalf("missing warning: %q", brief)
			}
			for _, line := range lines[:i+1] {
				if !strings.Contains(brief, line) {
					t.Fatalf("missing history %q in %q", line, brief)
				}
			}
			if got := len(failLogLines(brief)); got != i+1 {
				t.Fatalf("history count=%d want=%d", got, i+1)
			}
		}
	}
}
func TestFailLogRebuild(t *testing.T) {
	e, _, tools, _, _, ticket := recoveryFixture(t)
	e.Config.Escalate = "0"
	l := &failLogIssues{}
	e.Issues = l
	tools.gateReds = 1
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	old := failLogLines(strings.Join(l.comments, "\n"))
	if len(old) != 1 {
		t.Fatalf("initial history=%q", old)
	}
	dir := filepath.Join(e.Config.Root, e.Config.Repository, ticket.Identifier)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	// The scheduler re-reserves the ticket for the executor before the rebuild.
	l.comments = append(l.comments, "not FAIL: ignored\n FAIL: ignored\nordinary comment", "阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=1")
	r := &failLogRecovery{}
	e.Recovery = r
	e.Orca = &recoveryOrca{}
	tools.reviewReds = 1
	if _, err := e.Start(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "fail.log"))
	if err != nil || len(failLogLines(string(b))) != 2 || !strings.HasPrefix(string(b), old[0]+"\n") || strings.Contains(string(b), "ignored") {
		t.Fatalf("rebuilt=%q err=%v", b, err)
	}
	if len(r.requests) != 1 || !strings.Contains(r.requests[0].Brief, old[0]) || !strings.Contains(r.specs[0], old[0]) {
		t.Fatalf("restored history missing: %+v", r)
	}
}
func TestFailLogReadFailure(t *testing.T) {
	e, o, _, _, _, ticket := recoveryFixture(t)
	e.Issues = &failLogIssues{readErr: errors.New("credential-secret")}
	_, err := e.Execute(context.Background(), ticket)
	if err == nil || strings.Contains(err.Error(), "credential-secret") || len(o.starts) != 0 {
		t.Fatalf("err=%v starts=%d", err, len(o.starts))
	}
}

func TestFailLogDifferentAttempts(t *testing.T) {
	e, _, tools, _, _, ticket := recoveryFixture(t)
	e.Config.Escalate = "0"
	l := &failLogIssues{}
	e.Issues = l
	f := failLogToolFixture(e, tools)
	f.heads = []string{"abc1234567890", "def5678901234", "def5678901234"}
	f.gateReds = 2
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	lines := failLogLines(strings.Join(l.comments, "\n"))
	if len(lines) != 2 || lines[0] == lines[1] || !strings.Contains(lines[0], "head=abc1234") || !strings.Contains(lines[1], "head=def5678") {
		t.Fatalf("attempt summaries=%q", lines)
	}
}
func TestFailLogEvidenceSanitization(t *testing.T) {
	for _, cause := range []string{"gate", "review", "review fallback"} {
		t.Run(cause, func(t *testing.T) {
			e, _, tools, _, _, ticket := recoveryFixture(t)
			e.Config.Escalate = "0"
			l := &failLogIssues{}
			e.Issues = l
			f := failLogToolFixture(e, tools)
			want := ""
			switch cause {
			case "gate":
				f.gateReds, f.gateStep = 1, "check_[shell](x)"
				want = "check_shellx"
			case "review":
				f.reviewReds, f.reviewFile = 1, "driver/internal/[executor]/executor.go"
				want = "driver/internal/executor/executor.go:381"
			default:
				f.reviewReds, f.reviewFile = 1, ""
				want = "review"
			}
			if _, err := e.Execute(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			lines := failLogLines(strings.Join(l.comments, "\n"))
			if len(lines) != 1 || !strings.HasSuffix(lines[0], " | evidence="+want) {
				t.Fatalf("summaries=%q want evidence=%s", lines, want)
			}
		})
	}
}
func TestFailLogChangedFilesCommand(t *testing.T) {
	var got runx.Spec
	c := &Commands{Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
		got = spec
		return []byte("driver/internal/executor/executor.go\x00driver/internal/executor/faillog.go\x00"), nil
	}}
	// Assert the optional operation before extending Tools, so this test can run red.
	reader, ok := any(c).(interface {
		ChangedFiles(context.Context, string, string, string) ([]string, error)
	})
	if !ok {
		t.Fatal("Commands lacks ChangedFiles")
	}
	files, err := reader.ChangedFiles(context.Background(), "/repo/demo", "base-sha", "abc1234567890")
	if err != nil || strings.Join(files, ",") != "driver/internal/executor/executor.go,driver/internal/executor/faillog.go" {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if got.Path != "git" || got.Dir != "/repo/demo" || strings.Join(got.Args, " ") != "diff --no-ext-diff --name-only -z base-sha..abc1234567890 --" {
		t.Fatalf("command=%+v", got)
	}
}
