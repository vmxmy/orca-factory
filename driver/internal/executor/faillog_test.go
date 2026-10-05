package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

type failLogLinear struct {
	fakeLinear
	readErr error
	reads   int
}

func (l *failLogLinear) ListComments(context.Context, string) ([]linear.Comment, error) {
	l.reads++
	var comments []linear.Comment
	for _, body := range l.comments {
		comments = append(comments, linear.Comment{Body: body})
	}
	return comments, l.readErr
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
			l := &failLogLinear{}
			e.Linear = l
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
			if !strings.HasPrefix(line, "FAIL: approach=") || !strings.Contains(line, ticket.Todo) || !strings.Contains(line, " | reason="+tc.cause+" | evidence=") {
				t.Fatalf("invalid summary: %q", line)
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
	l := &failLogLinear{}
	e.Linear = l
	e.Config.Escalate = "0"
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
	if !utf8.ValidString(line) || utf8.RuneCountInString(line) > 300 || !strings.HasPrefix(line, "FAIL: approach=") || !strings.Contains(line, " | reason=gate red | evidence=gate") {
		t.Fatalf("invalid bounded summary: %q", line)
	}
}
func TestFailLogAllHistory(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	e.Config.Escalate = "0"
	o.failFirst = true
	tools.gateReds, tools.reviewReds = 1, 1
	l, r := &failLogLinear{}, &failLogRecovery{}
	e.Linear, e.Recovery = l, r
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	lines := failLogLines(strings.Join(l.comments, "\n"))
	if len(lines) != 3 || len(r.requests) != 3 || len(r.specs) != 3 {
		t.Fatalf("lines=%q requests=%d specs=%d", lines, len(r.requests), len(r.specs))
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
	l := &failLogLinear{}
	e.Linear = l
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
	l.comments = append(l.comments, "not FAIL: ignored\n FAIL: ignored\nordinary comment")
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
	e.Linear = &failLogLinear{readErr: errors.New("credential-secret")}
	_, err := e.Execute(context.Background(), ticket)
	if err == nil || strings.Contains(err.Error(), "credential-secret") || len(o.starts) != 0 {
		t.Fatalf("err=%v starts=%d", err, len(o.starts))
	}
}
