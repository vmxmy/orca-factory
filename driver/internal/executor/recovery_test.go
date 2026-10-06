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
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type recoveryOrca struct {
	fakeOrca
	starts          []orca.StartOptions
	failFirst       bool
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
	return []orca.Task{{ID: task, RunID: "run_one", DispatchID: dispatch, Result: json.RawMessage(`{"provenance":"worker_report","outcome":"` + outcome + `"}`)}}, nil
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
	missing            bool
	complete           bool
	requests           []CorrectionRequest
}

func (r *recoveryService) Correction(_ context.Context, req CorrectionRequest) error {
	r.sessions++
	r.requests = append(r.requests, req)
	if r.missing {
		return nil
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

type budgetLinear struct {
	fakeLinear
	updates []struct {
		state       string
		add, remove []string
	}
}

func (l *budgetLinear) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (linear.Issue, error) {
	l.updates = append(l.updates, struct {
		state       string
		add, remove []string
	}{state, add, remove})
	return l.fakeLinear.UpdateIssue(ctx, id, state, add, remove)
}
func recoveryFixture(t *testing.T) (*Executor, *recoveryOrca, *recoveryTools, *recoveryService, *budgetLinear, Ticket) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	o := &recoveryOrca{}
	tools := &recoveryTools{fakeTools: fakeTools{head: "new-sha"}}
	service := &recoveryService{}
	l := &budgetLinear{}
	e.Orca = o
	e.Tools = tools
	e.Recovery = service
	e.Linear = l
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
func TestCorrectionTierAndCap(t *testing.T) {
	for _, escalate := range []string{"0", "1"} {
		t.Run(escalate, func(t *testing.T) {
			t.Setenv("AGENT_TIER_ESCALATE", escalate)
			t.Setenv("AGENT_TIER_FAST", "codex:fast-model:low")
			t.Setenv("AGENT_TIER_DEFAULT", "codex:default-model:medium")
			t.Setenv("AGENT_TIER_STRONG", "claude:strong-model:high")
			e, o, tools, s, _, ticket := recoveryFixture(t)
			e.Config.Tier = "fast"
			e.Config.Model = "fast-model"
			e.Config.Effort = "low"
			tools.gateReds = 5
			result, err := e.Execute(context.Background(), ticket)
			if err != nil || result.State.Owner != machine.OwnerResolver || s.sessions != 3 || len(o.starts) != 4 {
				t.Fatalf("result=%+v err=%v sessions=%d starts=%d", result, err, s.sessions, len(o.starts))
			}
			want := []string{"fast-model", "fast-model", "fast-model", "fast-model"}
			if escalate == "1" {
				want = []string{"fast-model", "default-model", "strong-model", "strong-model"}
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
	e, o, tools, s, l, ticket := recoveryFixture(t)
	tools.gateReds = 1
	s.missing = true
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Infra != 3 || result.State.Owner != machine.OwnerResolver || s.sessions != 3 || len(o.starts) != 1 || s.children != 0 {
		t.Fatalf("result=%+v err=%v sessions=%d children=%d starts=%d", result, err, s.sessions, s.children, len(o.starts))
	}
	assertBudgetHandoff(t, l)
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
func assertBudgetHandoff(t *testing.T, l *budgetLinear) {
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
func TestResumeExistingPRWaitsWithoutFrozenEvidence(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	e.GitHub.(*fakeGitHub).existing = &github.PullRequest{Number: 7, HTMLURL: "https://example.test/pr/7"}
	result, err := e.Start(context.Background(), ticket)
	if err == nil || result.State.Owner == machine.OwnerMerge || len(o.starts) != 0 || tools.gateCalls != 0 || tools.reviewCalls != 0 {
		t.Fatalf("result=%+v err=%v starts=%d gate=%d review=%d", result, err, len(o.starts), tools.gateCalls, tools.reviewCalls)
	}
}
func TestCorrectionCommands(t *testing.T) {
	repo := t.TempDir()
	state := filepath.Join(t.TempDir(), "ticket state")
	target := filepath.Join(state, "correction.md")
	for _, key := range []string{"ORCA_PANE_KEY", "ORCA_AGENT_HOOK_PORT", "ORCA_AGENT_HOOK_TOKEN"} {
		t.Setenv(key, "test-hook-value")
	}
	t.Setenv("CORRECTION_TEST_ENV", "preserved")
	c := Commands{Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path != "claude" || len(s.Args) != 10 || s.Args[0] != "-p" || !strings.Contains(s.Args[1], target) || s.Timeout <= 0 || s.Dir != repo {
			t.Fatalf("invalid correction invocation: path=%q args=%q dir=%q timeout=%v", s.Path, s.Args, s.Dir, s.Timeout)
		}
		flags := map[string]string{}
		for i := 2; i < len(s.Args); i += 2 {
			flags[s.Args[i]] = s.Args[i+1]
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
	if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Infra != 2 || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 5 * time.Minute}) {
		t.Fatalf("result=%+v err=%v sleeps=%v", result, err, sleeps)
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
