package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type recoveryOrca struct {
	fakeOrca
	starts          []orca.StartOptions
	failFirst       bool
	failBody        string
	failSubject     string
	failResult      json.RawMessage
	releaseRequests []string
}

func (o *recoveryOrca) WorkerStart(ctx context.Context, id string, opts orca.StartOptions) (orca.Receipt, error) {
	o.starts = append(o.starts, opts)
	r, err := o.fakeOrca.WorkerStart(ctx, id, opts)
	var marshalErr error
	if opts.Task != "" {
		r["taskId"], marshalErr = json.Marshal(opts.Task)
		if marshalErr != nil {
			return nil, marshalErr
		}
	}
	if len(o.starts) > 1 {
		r["dispatchId"], marshalErr = json.Marshal(fmt.Sprintf("ctx_%d", len(o.starts)))
		if marshalErr != nil {
			return nil, marshalErr
		}
	}
	r["worktreePath"] = json.RawMessage(`"/original/worktree"`)
	return r, err
}
func (o *recoveryOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	outcome := "succeeded"
	if o.failFirst && len(o.starts) == 1 {
		outcome = "failed"
	}
	task := "task_one"
	if len(o.starts) > 0 && o.starts[len(o.starts)-1].Task != "" {
		task = o.starts[len(o.starts)-1].Task
	}
	dispatch := "ctx_one"
	if len(o.starts) > 1 {
		dispatch = fmt.Sprintf("ctx_%d", len(o.starts))
	}
	result, err := json.Marshal(map[string]string{"provenance": "worker_report", "outcome": outcome, "subject": o.failSubject, "body": o.failBody})
	if err != nil {
		return nil, err
	}
	if outcome == "failed" && len(o.failResult) > 0 {
		result = o.failResult
	}
	return []orca.Task{{ID: task, RunID: "run_one", DispatchID: dispatch, Result: result}}, nil
}

type recoveryTools struct {
	fakeTools
	gateReds, reviewReds, emptyReviews int
	gateErr                            error
}

func (f *recoveryTools) Gate(ctx context.Context, repo, head, spec string) (string, error) {
	if f.gateErr != nil {
		return "", f.gateErr
	}
	if f.gateReds > 0 {
		f.gateReds--
		f.gateCalls++
		return "VERDICT red\n", nil
	}
	return f.fakeTools.Gate(ctx, repo, head, spec)
}
func (f *recoveryTools) Review(ctx context.Context, repo, base, head string, estimate *float64) (string, string, error) {
	if f.emptyReviews > 0 {
		f.emptyReviews--
		f.reviewCalls++
		return "", "default", nil
	}
	if f.reviewReds > 0 {
		f.reviewReds--
		f.reviewCalls++
		return `{"verdict":"red","step":"review"}`, "default", nil
	}
	return f.fakeTools.Review(ctx, repo, base, head, estimate)
}

type recoveryService struct {
	sessions, children int
	stalledFrom        int
	complete           bool
	requests           []CorrectionRequest
}

func (r *recoveryService) Correction(_ context.Context, req CorrectionRequest) error {
	r.sessions++
	r.requests = append(r.requests, req)
	if r.stalledFrom > 0 && req.Number >= r.stalledFrom {
		return os.WriteFile(req.Path, []byte("\nSTALLED: 同一断言连续失败\a，步骤无新进展\n第二行\n"), 0600)
	}
	return os.WriteFile(req.Path, []byte("Repair the failing assertion"), 0600)
}
func (r *recoveryService) ChildTask(_ context.Context, run, parent, spec, request string) (string, error) {
	if run != "run_one" || (parent != "task_one" && parent != "child_task") || !strings.Contains(spec, "Repair the failing assertion") {
		return "", errors.New("bad child task")
	}
	r.children++
	return "child_task", nil
}
func (r *recoveryService) Complete(context.Context, string, string) (bool, error) {
	return r.complete, nil
}
func (r *recoveryService) Worktree(context.Context, string, string) (string, error) {
	return "/original/worktree", nil
}

type budgetIssues struct {
	fakeIssues
	updates []struct {
		state       string
		add, remove []string
	}
}

func (l *budgetIssues) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (task.Issue, error) {
	l.updates = append(l.updates, struct {
		state       string
		add, remove []string
	}{state, add, remove})
	return l.fakeIssues.UpdateIssue(ctx, id, state, add, remove)
}
func recoveryFixture(t *testing.T) (*Executor, *recoveryOrca, *recoveryTools, *recoveryService, *budgetIssues, Ticket) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	o := &recoveryOrca{}
	tools := &recoveryTools{fakeTools: fakeTools{head: "new-sha"}}
	service := &recoveryService{}
	l := &budgetIssues{}
	e.Orca = o
	e.Tools = tools
	e.Recovery = service
	e.Issues = l
	e.Sleep = func(context.Context, time.Duration) error { return nil }
	return e, o, tools, service, l, ticket
}
func TestCorrectionLoop(t *testing.T) {
	for _, cause := range []string{"gate", "review", "worker"} {
		t.Run(cause, func(t *testing.T) {
			e, o, tools, s, _, ticket := recoveryFixture(t)
			switch cause {
			case "gate":
				tools.gateReds = 1
			case "review":
				tools.reviewReds = 1
			case "worker":
				o.failFirst = true
			}
			result, err := e.Execute(context.Background(), ticket)
			if err != nil || result.State.Owner != machine.OwnerMerge || s.sessions != 1 || s.children != 1 || len(o.starts) != 2 {
				t.Fatalf("result=%+v err=%v sessions=%d children=%d starts=%d", result, err, s.sessions, s.children, len(o.starts))
			}
			child := o.starts[1]
			if child.Worktree != "path:/original/worktree" || child.Task != "child_task" || child.Spec != "" || child.Repo != "" || child.BaseBranch != "" || child.Name != "" {
				t.Fatalf("child: %+v", child)
			}
		})
	}
}

type fallbackRecovery struct {
	recoveryService
	correction func(context.Context, CorrectionRequest) error
	specs      []string
}

func (r *fallbackRecovery) Correction(ctx context.Context, req CorrectionRequest) error {
	r.sessions++
	r.requests = append(r.requests, req)
	return r.correction(ctx, req)
}

func (r *fallbackRecovery) ChildTask(_ context.Context, _, _, spec, _ string) (string, error) {
	r.specs = append(r.specs, spec)
	return "child_task", nil
}

func TestCorrectionSessionFallback(t *testing.T) {
	for _, tt := range []struct {
		name       string
		content    string
		write      bool
		err        error
		correction int
		want       string
		stalled    bool
	}{
		{"timeout", "", false, context.DeadlineExceeded, 1, "执行器代写：correction 会话未写出修复步骤（超时），按下列失败证据修复。", false},
		{"session failed", "", false, errors.New("exit status 1"), 1, "执行器代写：correction 会话未写出修复步骤（会话失败），按下列失败证据修复。", false},
		{"written before exit error", "步骤 1：修复断言", true, errors.New("exit status 1"), 1, "步骤 1：修复断言", false},
		{"empty file", "", true, nil, 1, "执行器代写：correction 会话未写出修复步骤（文件为空），按下列失败证据修复。", false},
		{"whitespace file", " \n\t", true, nil, 1, "执行器代写：correction 会话未写出修复步骤（文件为空），按下列失败证据修复。", false},
		{"second correction stalled", "STALLED: 重复失败", true, errors.New("exit status 1"), 2, "", true},
		{"third correction stalled", "STALLED: 重复失败", true, errors.New("exit status 1"), 3, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, o, tools, _, issues, ticket := recoveryFixture(t)
			tools.gateReds = tt.correction
			s := &fallbackRecovery{correction: func(_ context.Context, req CorrectionRequest) error {
				if req.Number < tt.correction {
					return os.WriteFile(req.Path, []byte("Repair the failing assertion"), 0600)
				}
				if tt.write {
					if err := os.WriteFile(req.Path, []byte(tt.content), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return tt.err
			}}
			e.Recovery = s
			var sleeps []time.Duration
			e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
			result, err := e.Execute(context.Background(), ticket)
			if err != nil || result.State.Infra != 0 || len(sleeps) != 0 || s.sessions != tt.correction {
				t.Fatalf("result=%+v err=%v sleeps=%v sessions=%d", result, err, sleeps, s.sessions)
			}
			if tt.stalled {
				if result.State.Owner != machine.OwnerResolver || result.StallReason != "重复失败" || len(o.starts) != tt.correction || len(s.specs) != tt.correction-1 {
					t.Fatalf("result=%+v starts=%d specs=%v", result, len(o.starts), s.specs)
				}
				assertBudgetHandoff(t, issues)
				return
			}
			if result.State.Owner != machine.OwnerMerge || len(o.starts) != 2 || len(s.specs) != 1 {
				t.Fatalf("result=%+v starts=%d specs=%v", result, len(o.starts), s.specs)
			}
			spec := s.specs[0]
			fallback := strings.HasPrefix(tt.want, "执行器代写：")
			body := tt.want
			if fallback {
				log, err := os.ReadFile(failLogPath(e.Config, ticket))
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSpace(string(log)), "\n")
				body += "\n失败原因：" + s.requests[0].Cause + "\n失败摘要：" + lines[len(lines)-1]
			}
			if want := fmt.Sprintf("correction %d/3\n%s\n", tt.correction, body); !strings.Contains(spec, want) {
				t.Fatalf("spec=%q, want contiguous %q", spec, want)
			}
			if !fallback && strings.Contains(spec, "执行器代写") {
				t.Fatalf("written steps replaced with fallback: %q", spec)
			}
		})
	}
}

func TestCorrectionFallbackOmitsEmptySummary(t *testing.T) {
	want := "执行器代写：correction 会话未写出修复步骤（文件为空），按下列失败证据修复。\n失败原因：head CI failed"
	if got := string(correctionFallback(nil, "head CI failed", "")); got != want {
		t.Fatalf("fallback=%q, want %q", got, want)
	}
}

func TestCorrectionTierAndCap(t *testing.T) {
	for _, escalate := range []string{"0", "1"} {
		t.Run(escalate, func(t *testing.T) {
			t.Setenv("AGENT_TIER_ESCALATE", escalate)
			t.Setenv("AGENT_TIER_DEFAULT", "codex:default-model:medium")
			t.Setenv("AGENT_TIER_STRONG", "claude:strong-model:high")
			e, o, tools, s, _, ticket := recoveryFixture(t)
			e.Config.Tier = "default"
			e.Config.Model = "default-model"
			e.Config.Effort = "medium"
			tools.gateReds = 5
			result, err := e.Execute(context.Background(), ticket)
			if err != nil || result.State.Owner != machine.OwnerResolver || s.sessions != 3 || len(o.starts) != 4 {
				t.Fatalf("result=%+v err=%v sessions=%d starts=%d", result, err, s.sessions, len(o.starts))
			}
			want := []string{"default-model", "default-model", "default-model", "default-model"}
			if escalate == "1" {
				want = []string{"default-model", "strong-model", "strong-model", "strong-model"}
			}
			for i, start := range o.starts {
				if start.Model != want[i] {
					t.Fatalf("start %d model=%s want=%s", i, start.Model, want[i])
				}
			}
		})
	}
}
func TestCorrectionMissingArtifactBudget(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	tools.gateReds = 1
	s := &fallbackRecovery{correction: func(context.Context, CorrectionRequest) error { return nil }}
	e.Recovery = s
	var sleeps []time.Duration
	e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Infra != 0 || len(sleeps) != 0 || result.State.Owner != machine.OwnerMerge || s.sessions != 1 || len(o.starts) != 2 || len(s.specs) != 1 || !strings.Contains(s.specs[0], "执行器代写：correction 会话未写出修复步骤（文件为空）") {
		t.Fatalf("result=%+v err=%v sleeps=%v sessions=%d specs=%q starts=%d", result, err, sleeps, s.sessions, s.specs, len(o.starts))
	}
}
func TestInfraBudgetBackoff(t *testing.T) {
	e, _, tools, _, _, ticket := recoveryFixture(t)
	tools.emptyReviews = 2
	var sleeps []time.Duration
	e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Infra != 2 || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 5 * time.Minute}) {
		t.Fatalf("result=%+v err=%v sleeps=%v", result, err, sleeps)
	}
}
func TestInfraBudgetExhaustion(t *testing.T) {
	e, _, tools, _, l, ticket := recoveryFixture(t)
	tools.emptyReviews = 3
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Infra != 3 || result.State.Owner != machine.OwnerResolver {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	assertBudgetHandoff(t, l)
}
func assertBudgetHandoff(t *testing.T, l *budgetIssues) {
	t.Helper()
	last := l.updates[len(l.updates)-1]
	if last.state != "In Progress" || !reflect.DeepEqual(last.add, []string{"auto-impl", "auto-failed"}) || !reflect.DeepEqual(last.remove, []string{"ready-for-agent"}) {
		t.Fatalf("labels=%+v", last)
	}
	lines := strings.Split(l.comments[len(l.comments)-1], "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[3], "阶段: Agent处置") || !strings.HasPrefix(lines[4], "归属: 处置Agent") || !strings.HasPrefix(lines[0], "Agent处置: ") || !strings.HasPrefix(lines[1], "证据: ") || !strings.HasPrefix(lines[2], "处置要求: ") {
		t.Fatalf("handoff=%q", lines)
	}
}
func TestResumeTaskResult(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	state := checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Worktree: "/original/worktree", Baseline: "base-sha"}
	if err := saveCheckpoint(e.Config, ticket, state); err != nil {
		t.Fatal(err)
	}
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || len(o.starts) != 0 || tools.gateCalls != 1 {
		t.Fatalf("result=%+v err=%v starts=%d gates=%d", result, err, len(o.starts), tools.gateCalls)
	}
}
func TestResumeCompleteBranch(t *testing.T) {
	e, o, tools, s, _, ticket := recoveryFixture(t)
	s.complete = true
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || len(o.starts) != 0 || tools.gateCalls != 1 {
		t.Fatalf("result=%+v err=%v starts=%d gates=%d", result, err, len(o.starts), tools.gateCalls)
	}
}
func TestResumeExistingPRHandsToResolverWithoutFrozenEvidence(t *testing.T) {
	e, o, tools, _, l, ticket := recoveryFixture(t)
	e.GitHub.(*fakeGitHub).existing = &github.PullRequest{Number: 7, HTMLURL: "https://example.test/pr/7"}
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver || len(o.starts) != 0 || tools.gateCalls != 0 || tools.reviewCalls != 0 {
		t.Fatalf("result=%+v err=%v starts=%d gate=%d review=%d", result, err, len(o.starts), tools.gateCalls, tools.reviewCalls)
	}
	if len(l.comments) != 1 || !strings.HasPrefix(l.comments[0], "Agent处置: existing PR has no frozen handoff\n") || !strings.Contains(l.comments[0], "PR #7 head=new-sha") {
		t.Fatalf("expected one resolver event with PR evidence: %v", l.comments)
	}
	assertBudgetHandoff(t, l)
}
func TestCorrectionCommands(t *testing.T) {
	repo := t.TempDir()
	writeRole(t, repo, "correction.md", "CORRECTION ROLE\n")
	state := filepath.Join(t.TempDir(), "ticket state")
	target := filepath.Join(state, "correction.md")
	for _, key := range []string{"ORCA_PANE_KEY", "ORCA_AGENT_HOOK_PORT", "ORCA_AGENT_HOOK_TOKEN"} {
		t.Setenv(key, "test-hook-value")
	}
	t.Setenv("CORRECTION_TEST_ENV", "preserved")
	t.Setenv("AGENT_CORRECTION", "claude:claude-opus-5-5:high")
	t.Setenv("AGENT_CORRECTION_FALLBACK", "")
	c := Commands{Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path != "claude" || len(s.Args) != 14 || s.Args[0] != "-p" || !strings.Contains(s.Args[1], target) || s.Timeout != 10*time.Minute || s.Dir != repo {
			t.Fatalf("invalid correction invocation: path=%q args=%q dir=%q timeout=%v", s.Path, s.Args, s.Dir, s.Timeout)
		}
		flags := map[string]string{}
		for i := 2; i < len(s.Args); i += 2 {
			flags[s.Args[i]] = s.Args[i+1]
		}
		if flags["--model"] != "claude-opus-5-5" || flags["--effort"] != "high" {
			t.Fatalf("unexpected correction model or effort: %v", flags)
		}
		if flags["--permission-mode"] != "acceptEdits" || flags["--add-dir"] != state {
			t.Fatalf("missing correction write authorization: %v", flags)
		}
		for _, flag := range []string{"--allowedTools", "--tools"} {
			tools := strings.Split(flags[flag], ",")
			if !reflect.DeepEqual(tools, []string{"Read", "Grep", "Glob", "Write", "Edit"}) {
				t.Fatalf("%s tools=%q", flag, tools)
			}
		}
		if s.Env == nil {
			t.Fatal("correction inherits Orca hook environment")
		}
		preserved := false
		for _, entry := range s.Env {
			key, _, _ := strings.Cut(entry, "=")
			switch key {
			case "ORCA_PANE_KEY", "ORCA_AGENT_HOOK_PORT", "ORCA_AGENT_HOOK_TOKEN":
				t.Fatalf("correction environment contains %s", key)
			}
			if entry == "CORRECTION_TEST_ENV=preserved" {
				preserved = true
			}
		}
		if !preserved {
			t.Fatal("correction dropped unrelated environment")
		}
		return nil, nil
	}}
	if err := c.Correction(context.Background(), CorrectionRequest{Repo: repo, Path: target, Cause: "gate red"}); err != nil {
		t.Fatal(err)
	}
}

func TestInfraBudgetGateTransport(t *testing.T) {
	e, _, tools, _, l, ticket := recoveryFixture(t)
	tools.gateErr = errors.New("private transport detail")
	var sleeps []time.Duration
	e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Infra != 3 || result.State.Owner != machine.OwnerResolver || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 5 * time.Minute}) {
		t.Fatalf("result=%+v err=%v sleeps=%v", result, err, sleeps)
	}
	assertBudgetHandoff(t, l)
}
func TestInfraBudgetSharedAcrossCorrection(t *testing.T) {
	e, _, tools, _, _, ticket := recoveryFixture(t)
	tools.emptyReviews = 1
	tools.reviewReds = 1
	s := &missingOnceRecovery{recoveryService: recoveryService{}}
	e.Recovery = s
	var sleeps []time.Duration
	e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Infra != 1 || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute}) || s.children != 1 {
		t.Fatalf("result=%+v err=%v sleeps=%v children=%d", result, err, sleeps, s.children)
	}
}

type missingOnceRecovery struct{ recoveryService }

func (s *missingOnceRecovery) Correction(ctx context.Context, r CorrectionRequest) error {
	if s.sessions == 0 {
		s.sessions++
		return nil
	}
	return s.recoveryService.Correction(ctx, r)
}

func (s *missingOnceRecovery) ChildTask(_ context.Context, _, _, spec, _ string) (string, error) {
	if !strings.Contains(spec, "执行器代写：correction 会话未写出修复步骤（文件为空）") {
		return "", errors.New("missing fallback in child task")
	}
	s.children++
	return "child_task", nil
}

func TestResumeLegacyRunResult(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	e.Orca = &legacyRunOrca{recoveryOrca: o}
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || len(o.starts) != 0 || tools.gateCalls != 1 {
		t.Fatalf("result=%+v err=%v starts=%d gates=%d", result, err, len(o.starts), tools.gateCalls)
	}
}

type legacyRunOrca struct{ *recoveryOrca }

func (o *legacyRunOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	tasks, err := o.recoveryOrca.TaskList(ctx, run)
	tasks[0].Spec = "Original brief: agent/ZII-861/executor"
	return tasks, err
}

func TestCorrectionReleaseEachDispatch(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	tools.gateReds = 2
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(o.releaseRequests) != 3 {
		t.Fatalf("release requests=%v", o.releaseRequests)
	}
	seen := map[string]bool{}
	for _, id := range o.releaseRequests {
		if seen[id] {
			t.Fatalf("release request reused across dispatches: %s", id)
		}
		seen[id] = true
	}
}

func (o *recoveryOrca) WorkerRelease(ctx context.Context, id, dispatch string) (orca.Receipt, error) {
	o.releaseRequests = append(o.releaseRequests, id)
	o.releases++
	return orca.Receipt{}, o.releaseErr
}

// clockedIssues stamps each comment with the test clock so a park comment can
// fall after the checkpoint's dispatch start.
type clockedIssues struct {
	budgetIssues
	now func() time.Time
	at  []time.Time
}

func (l *clockedIssues) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	l.at = append(l.at, l.now())
	return l.budgetIssues.CreateComment(ctx, id, body)
}
func (l *clockedIssues) ListComments(ctx context.Context, id string) ([]task.Comment, error) {
	comments, err := l.budgetIssues.ListComments(ctx, id)
	for i := range comments {
		comments[i].CreatedAt = l.at[i]
	}
	return comments, err
}

// park appends the recorded 搁置 comment (归属: 无) at its recorded time and the
// scheduler's next claim one minute later.
func (l *clockedIssues) park(release task.Comment) {
	l.comments = append(l.comments, release.Body, "阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1")
	l.at = append(l.at, release.CreatedAt, release.CreatedAt.Add(time.Minute))
}

func recordedRelease(t *testing.T) task.Comment {
	t.Helper()
	var recording struct {
		Parked struct{ Comments task.Connection[task.Comment] }
	}
	if err := json.Unmarshal(recorded(t, "ledger-issue-parked-resume.json"), &recording); err != nil {
		t.Fatal(err)
	}
	for _, comment := range recording.Parked.Comments.Nodes {
		if strings.Contains(comment.Body, "\n归属: 无\n") && !comment.CreatedAt.IsZero() {
			return comment
		}
	}
	t.Fatal("recorded park comment without 归属: 无")
	return task.Comment{}
}

func TestReclaimAfterParkGetsFreshCorrectionBudget(t *testing.T) {
	ctx := context.Background()
	release := recordedRelease(t)
	t.Run("new claim after park", func(t *testing.T) {
		e, o, tools, service, _, ticket := recoveryFixture(t)
		clock := release.CreatedAt.Add(-time.Hour)
		e.Now = func() time.Time { return clock }
		l := &clockedIssues{now: e.Now}
		e.Issues = l
		tools.gateReds = 5
		result, err := e.Execute(ctx, ticket)
		if err != nil || result.State.Owner != machine.OwnerResolver || len(o.starts) != 4 {
			t.Fatalf("precondition: result=%+v err=%v starts=%d", result, err, len(o.starts))
		}
		l.park(release)
		clock = release.CreatedAt.Add(2 * time.Minute)
		e.Config.Terminal = "term_reclaim"
		service.complete = true
		tools.gateReds = 1
		starts, used := len(o.starts), len(o.used)
		result, err = e.Start(ctx, ticket)
		if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Corrections != 1 || len(o.starts) <= starts || o.starts[starts].Spec == "" {
			t.Fatalf("re-claim did not start a fresh worker: result=%+v err=%v newStarts=%d", result, err, len(o.starts)-starts)
		}
		if len(o.used) != used {
			t.Fatalf("re-claim took over the archived Run: %v", o.used[used:])
		}
		archived, err := filepath.Glob(filepath.Join(filepath.Dir(checkpointPath(e.Config, ticket)), "executor-*.json"))
		if err != nil || len(archived) != 1 {
			t.Fatalf("old checkpoint not archived: %v %v", archived, err)
		}
	})
	t.Run("restart within the same claim", func(t *testing.T) {
		e, o, _, _, _, ticket := recoveryFixture(t)
		e.Now = func() time.Time { return release.CreatedAt.Add(time.Hour) }
		l := &clockedIssues{now: e.Now}
		e.Issues = l
		l.park(release)
		started := release.CreatedAt.Add(2 * time.Minute).Format(time.RFC3339)
		if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Corrections: 2, StartedAt: started}); err != nil {
			t.Fatal(err)
		}
		result, err := e.Start(ctx, ticket)
		if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Corrections != 2 || len(o.starts) != 0 || !reflect.DeepEqual(o.used, []string{"run_one"}) {
			t.Fatalf("restart dropped checkpoint: result=%+v err=%v starts=%d used=%v", result, err, len(o.starts), o.used)
		}
	})
	t.Run("operator release line", func(t *testing.T) {
		e, o, _, _, _, ticket := recoveryFixture(t)
		e.Now = func() time.Time { return release.CreatedAt.Add(time.Hour) }
		l := &operatorReleaseIssues{clockedIssues{now: e.Now}}
		e.Issues = l
		l.park(release)
		started := release.CreatedAt.Add(-time.Minute).Format(time.RFC3339)
		if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Corrections: 2, StartedAt: started}); err != nil {
			t.Fatal(err)
		}
		result, err := e.Start(ctx, ticket)
		if err != nil || result.State.Corrections != 2 || len(o.starts) != 0 || !reflect.DeepEqual(o.used, []string{"run_one"}) {
			t.Fatalf("operator 归属: 无 archived checkpoint: result=%+v err=%v starts=%d used=%v", result, err, len(o.starts), o.used)
		}
	})
}

// operatorReleaseIssues marks the park comment as operator-written.
type operatorReleaseIssues struct{ clockedIssues }

func (l *operatorReleaseIssues) ListComments(ctx context.Context, id string) ([]task.Comment, error) {
	comments, err := l.clockedIssues.ListComments(ctx, id)
	for i := range comments {
		if strings.Contains(comments[i].Body, "\n归属: 无\n") {
			comments[i].Source = task.SourceOperator
		}
	}
	return comments, err
}
