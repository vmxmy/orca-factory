package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type budgetOrca struct {
	*recoveryOrca
	failure error
	reads   int
}

func (o *budgetOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	o.reads++
	return nil, o.failure
}

func TestBudgetTaskListGlobal(t *testing.T) {
	e, base, _, _, l, ticket := recoveryFixture(t)
	o := &budgetOrca{recoveryOrca: base, failure: fmt.Errorf("secret-token: %w", &infra.Global{Dep: infra.Orca, Err: errors.New("secret-token")})}
	e.Orca = o
	sleeps := 0
	e.Sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
	result, err := e.Execute(context.Background(), ticket)
	var global *infra.Global
	if !errors.As(err, &global) || global.Dep != infra.Orca {
		t.Fatalf("lost Global classification: result=%+v err=%v", result, err)
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(global.Error(), "secret-token") {
		t.Fatal("external error leaked")
	}
	if result.State.Phase != machine.PhaseWorker || !result.State.ExecAlive || result.State.Infra != 0 || result.State.Corrections != 0 || sleeps != 0 || o.reads != 1 || len(l.updates) != 1 || o.releases != 0 {
		t.Fatalf("global consumed budget or continued: result=%+v sleeps=%d reads=%d updates=%v", result, sleeps, o.reads, l.updates)
	}
}

type budgetRecovery struct {
	recoveryService
	failure error
}

func (r *budgetRecovery) Correction(ctx context.Context, req CorrectionRequest) error {
	r.sessions++
	if r.failure != nil {
		return r.failure
	}
	return os.WriteFile(req.Path, []byte(" \n\t"), 0600)
}

func TestBudgetCorrectionDependencyStop(t *testing.T) {
	for _, failure := range []error{fmt.Errorf("secret-token: %w", context.Canceled), fmt.Errorf("secret-token: %w", &infra.Global{Dep: infra.Claude, Err: errors.New("secret-token")})} {
		e, o, tools, _, l, ticket := recoveryFixture(t)
		tools.gateReds = 1
		r := &budgetRecovery{failure: failure}
		e.Recovery = r
		sleeps := 0
		e.Sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
		result, err := e.Execute(context.Background(), ticket)
		var global *infra.Global
		classified := errors.Is(err, context.Canceled)
		if errors.As(failure, &global) {
			classified = errors.As(err, &global) && global.Dep == infra.Claude
		}
		if !classified || result.State.Phase != machine.PhaseCorrection || result.State.Infra != 0 || result.State.Corrections != 1 || r.sessions != 1 || r.children != 0 || sleeps != 0 || len(o.starts) != 1 || len(l.updates) != 1 {
			t.Fatalf("result=%+v err=%v sessions=%d sleeps=%d", result, err, r.sessions, sleeps)
		}
		if strings.Contains(err.Error(), "secret-token") {
			t.Fatal("external error leaked")
		}
	}
}

func TestBudgetEmptyCorrectionArtifact(t *testing.T) {
	e, o, tools, _, l, ticket := recoveryFixture(t)
	tools.emptyReviews = 1
	tools.reviewReds = 1
	r := &budgetRecovery{}
	e.Recovery = r
	var sleeps []time.Duration
	e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerHuman || result.State.Infra != 3 || result.State.Corrections != 1 || r.sessions != 2 || r.children != 0 || len(o.starts) != 1 || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 5 * time.Minute}) {
		t.Fatalf("result=%+v err=%v sessions=%d children=%d sleeps=%v", result, err, r.sessions, r.children, sleeps)
	}
	assertBudgetHandoff(t, l)
}

func TestBudgetStartTaskListStop(t *testing.T) {
	for _, failure := range []error{fmt.Errorf("secret-token: %w", &infra.Global{Dep: infra.Orca, Err: infra.HTTPError{StatusCode: 503}}), fmt.Errorf("secret-token: %w", context.Canceled)} {
		e, base, _, _, l, ticket := recoveryFixture(t)
		o := &budgetOrca{recoveryOrca: base, failure: failure}
		e.Orca = o
		sleeps := 0
		e.Sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
		result, err := e.Start(context.Background(), ticket)
		var global *infra.Global
		ok := errors.Is(err, context.Canceled)
		if errors.As(failure, &global) {
			ok = errors.As(err, &global) && global.Dep == infra.Orca
		}
		if !ok || result.State.Infra != 0 || result.State.Corrections != 0 || sleeps != 0 || o.reads != 1 || len(l.updates) != 0 || len(o.starts) != 0 {
			t.Fatalf("result=%+v err=%v sleeps=%d reads=%d", result, err, sleeps, o.reads)
		}
		if strings.Contains(err.Error(), "secret-token") {
			t.Fatal("secret leaked")
		}
	}
}

type budgetGitHub struct {
	fakeGitHub
	failure error
}

func (g *budgetGitHub) FindOpenPR(context.Context, string, string, string) (*github.PullRequest, error) {
	return nil, g.failure
}

type budgetBaseline struct{ *recoveryTools }

func (b *budgetBaseline) Baseline(context.Context, string) (string, error) {
	return "", &infra.Transient{Err: errors.New("secret-token")}
}

func TestBudgetReadExhaustion(t *testing.T) {
	for _, step := range []string{"baseline", "PR"} {
		t.Run(step, func(t *testing.T) {
			e, _, tools, _, l, ticket := recoveryFixture(t)
			failure := &infra.Ticket{Err: errors.New("secret-token")}
			if step == "PR" {
				e.GitHub = &budgetGitHub{failure: failure}
			}
			if step == "baseline" {
				e.Tools = &budgetBaseline{tools}
			}
			var sleeps []time.Duration
			e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
			result, err := e.Execute(context.Background(), ticket)
			if err != nil || result.State.Infra != 3 || result.State.Corrections != 0 || result.State.Owner != machine.OwnerHuman || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 5 * time.Minute}) {
				t.Fatalf("result=%+v err=%v sleeps=%v", result, err, sleeps)
			}
			assertBudgetHandoff(t, l)
		})
	}
}

type budgetGate struct{ *recoveryTools }

func (b *budgetGate) Gate(context.Context, string, string, string) (string, error) {
	return "VERDICT red\n", &infra.Ticket{Err: &infra.Transient{Err: errors.New("secret-token")}}
}
func TestBudgetGateInfrastructureDoesNotCorrect(t *testing.T) {
	e, o, tools, r, l, ticket := recoveryFixture(t)
	e.Tools = &budgetGate{tools}
	var sleeps []time.Duration
	e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Infra != 3 || result.State.Corrections != 0 || result.State.Owner != machine.OwnerHuman || r.sessions != 0 || len(o.starts) != 1 || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 5 * time.Minute}) {
		t.Fatalf("result=%+v err=%v sessions=%d sleeps=%v", result, err, r.sessions, sleeps)
	}
	assertBudgetHandoff(t, l)
}

type recoveringBaseline struct {
	*recoveryTools
	reads int
}

func (b *recoveringBaseline) Baseline(ctx context.Context, repo string) (string, error) {
	b.reads++
	if b.reads == 1 {
		return "", &infra.Transient{Err: errors.New("secret-token")}
	}
	return b.recoveryTools.Baseline(ctx, repo)
}
func TestBudgetReadSuccessDoesNotRepeatBackoffOrReset(t *testing.T) {
	e, _, tools, _, l, ticket := recoveryFixture(t)
	e.Tools = &recoveringBaseline{recoveryTools: tools}
	tools.emptyReviews = 2
	var sleeps []time.Duration
	e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Infra != 3 || result.State.Corrections != 0 || result.State.Owner != machine.OwnerHuman || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 5 * time.Minute}) {
		t.Fatalf("result=%+v err=%v sleeps=%v", result, err, sleeps)
	}
	assertBudgetHandoff(t, l)
}

type uncertainBudgetOrca struct {
	*recoveryOrca
	failure error
}

func (o *uncertainBudgetOrca) WorkerStart(ctx context.Context, id string, opts orca.StartOptions) (orca.Receipt, error) {
	o.recoveryOrca.WorkerStart(ctx, id, opts)
	return nil, o.failure
}
func TestBudgetUnconfirmedWritePreservesSafeClassification(t *testing.T) {
	for _, failure := range []error{
		&infra.Global{Dep: infra.Orca, Err: context.Canceled},
		&infra.Ticket{Err: &infra.Transient{Err: infra.HTTPError{StatusCode: 503}}},
		&infra.Transient{Err: infra.ErrWriteUnconfirmed},
	} {
		e, o, _, _, l, ticket := recoveryFixture(t)
		e.Orca = &uncertainBudgetOrca{recoveryOrca: o, failure: fmt.Errorf("secret-token: %w", failure)}
		sleeps := 0
		e.Sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
		result, err := e.Execute(context.Background(), ticket)
		if err == nil || strings.Contains(err.Error(), "secret-token") || result.State.Infra != 0 || result.State.Corrections != 0 || len(o.starts) != 1 || sleeps != 0 || len(l.updates) != 1 || o.releases != 0 {
			t.Fatalf("result=%+v err=%v starts=%d sleeps=%d", result, err, len(o.starts), sleeps)
		}
		var global *infra.Global
		var ticketErr *infra.Ticket
		var transient *infra.Transient
		var http infra.HTTPError
		switch failure.(type) {
		case *infra.Global:
			if !errors.As(err, &global) || global.Dep != infra.Orca || !errors.Is(err, context.Canceled) {
				t.Fatalf("lost global/cancel: %v", err)
			}
		case *infra.Ticket:
			if !errors.As(err, &ticketErr) || !errors.As(err, &transient) || !errors.As(err, &http) || http.StatusCode != 503 {
				t.Fatalf("lost safe types: %v", err)
			}
		case *infra.Transient:
			if !errors.As(err, &transient) || !errors.Is(err, infra.ErrWriteUnconfirmed) {
				t.Fatalf("lost unconfirmed: %v", err)
			}
		}
	}
}

type recoveringBudgetOrca struct {
	*recoveryOrca
	reads int
}

func (o *recoveringBudgetOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	o.reads++
	if o.reads == 1 {
		return nil, &infra.Ticket{Err: errors.New("secret-token")}
	}
	return o.recoveryOrca.TaskList(ctx, run)
}
func TestBudgetRecoverDispatchSharesExecutionBudget(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			e, base, tools, _, l, ticket := recoveryFixture(t)
			o := &recoveringBudgetOrca{recoveryOrca: base}
			e.Orca = o
			if restored {
				if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Baseline: "base-sha"}); err != nil {
					t.Fatal(err)
				}
			}
			tools.emptyReviews = 2
			var sleeps []time.Duration
			e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
			result, err := e.Start(context.Background(), ticket)
			if err != nil || result.State.Infra != 3 || result.State.Corrections != 0 || result.State.Owner != machine.OwnerHuman || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 5 * time.Minute}) {
				t.Fatalf("result=%+v err=%v sleeps=%v", result, err, sleeps)
			}
			wantStarts := 1
			if restored {
				wantStarts = 0
			}
			if len(base.starts) != wantStarts {
				t.Fatalf("starts=%d want=%d", len(base.starts), wantStarts)
			}
			assertBudgetHandoff(t, l)
		})
	}
}

type worktreeBudgetOrca struct{ *recoveryOrca }

func (o *worktreeBudgetOrca) WorkerStart(ctx context.Context, id string, opts orca.StartOptions) (orca.Receipt, error) {
	receipt, err := o.recoveryOrca.WorkerStart(ctx, id, opts)
	delete(receipt, "worktreePath")
	return receipt, err
}

type worktreeBudgetRecovery struct {
	recoveryService
	reads int
}

func (r *worktreeBudgetRecovery) Worktree(context.Context, string, string) (string, error) {
	r.reads++
	return "", &infra.Ticket{Err: errors.New("secret-token")}
}
func TestBudgetCorrectionWorktreeRead(t *testing.T) {
	e, o, tools, _, l, ticket := recoveryFixture(t)
	e.Orca = &worktreeBudgetOrca{o}
	tools.gateReds = 1
	r := &worktreeBudgetRecovery{}
	e.Recovery = r
	var sleeps []time.Duration
	e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Infra != 3 || result.State.Corrections != 1 || result.State.Owner != machine.OwnerHuman || r.sessions != 1 || r.reads != 3 || r.children != 0 || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 5 * time.Minute}) {
		t.Fatalf("result=%+v err=%v sessions=%d reads=%d sleeps=%v", result, err, r.sessions, r.reads, sleeps)
	}
	assertBudgetHandoff(t, l)
}

type failedHeadTools struct{ *recoveryTools }

func (f *failedHeadTools) Head(context.Context, string, string) (string, error) {
	return "", &infra.Ticket{Err: errors.New("secret-token")}
}

type failedCompleteRecovery struct{ recoveryService }

func (f *failedCompleteRecovery) Complete(context.Context, string, string) (bool, error) {
	return false, &infra.Ticket{Err: errors.New("secret-token")}
}

func TestBudgetRecoveryReadExhaustion(t *testing.T) {
	for _, step := range []string{"head", "PR", "complete", "tasks"} {
		t.Run(step, func(t *testing.T) {
			e, o, tools, _, l, ticket := recoveryFixture(t)
			wantCorrections := 0
			switch step {
			case "head":
				e.Tools = &failedHeadTools{tools}
			case "PR":
				e.GitHub = &budgetGitHub{failure: &infra.Ticket{Err: errors.New("secret-token")}}
			case "complete":
				e.Recovery = &failedCompleteRecovery{}
			case "tasks":
				e.Orca = &budgetOrca{recoveryOrca: o, failure: &infra.Ticket{Err: errors.New("secret-token")}}
				wantCorrections = 2
				if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Corrections: 2}); err != nil {
					t.Fatal(err)
				}
			}
			var sleeps []time.Duration
			e.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
			result, err := e.Start(context.Background(), ticket)
			if err != nil || result.State.Infra != 3 || result.State.Corrections != wantCorrections || result.State.Owner != machine.OwnerHuman || len(o.starts) != 0 || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 5 * time.Minute}) {
				t.Fatalf("result=%+v err=%v starts=%d sleeps=%v", result, err, len(o.starts), sleeps)
			}
			assertBudgetHandoff(t, l)
		})
	}
}

type canceledReadOrca struct {
	*recoveryOrca
	cancel context.CancelFunc
}

func (o *canceledReadOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	tasks, err := o.recoveryOrca.TaskList(ctx, run)
	o.cancel()
	return tasks, err
}
func TestBudgetCancellationDuringReadStopsBeforeTransition(t *testing.T) {
	e, o, _, _, l, ticket := recoveryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Orca = &canceledReadOrca{recoveryOrca: o, cancel: cancel}
	sleeps := 0
	e.Sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
	result, err := e.Execute(ctx, ticket)
	if !errors.Is(err, context.Canceled) || result.State.Phase != machine.PhaseWorker || result.State.Infra != 0 || result.State.Corrections != 0 || o.releases != 0 || sleeps != 0 || len(l.comments) != 1 {
		t.Fatalf("result=%+v err=%v releases=%d sleeps=%d comments=%v", result, err, o.releases, sleeps, l.comments)
	}
}

type mixedBudgetOrca struct {
	*idleOrca
	reads int
}

func (o *mixedBudgetOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	o.reads++
	if o.reads == 1 {
		return nil, &infra.Ticket{Err: errors.New("secret-token")}
	}
	return idleTask(o.reads == 4), nil
}
func TestBudgetReadsStallReviewShareFakeClock(t *testing.T) {
	e, _, tools, _, l, ticket := recoveryFixture(t)
	now := e.Now()
	e.Now = func() time.Time { return now }
	e.Config.PollInterval = 2 * time.Minute
	e.Config.WorkerIdleTimeout = time.Minute
	var sleeps []time.Duration
	e.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		now = now.Add(d)
		return nil
	}
	idle := &idleOrca{worktree: "/worker"}
	lastOutput := now.Add(-2 * time.Minute).UnixMilli()
	idle.terminals = func(int) ([]orca.Terminal, error) {
		return []orca.Terminal{{WorktreePath: "/worker", LastOutputAt: lastOutput}}, nil
	}
	e.Orca = &mixedBudgetOrca{idleOrca: idle}
	tools.emptyReviews = 1
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Infra != 3 || result.State.Corrections != 0 || result.State.Owner != machine.OwnerHuman || !reflect.DeepEqual(sleeps, []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 2 * time.Minute}) {
		t.Fatalf("result=%+v err=%v sleeps=%v", result, err, sleeps)
	}
	if len(idle.sends) != 1 {
		t.Fatalf("nudge replayed: %v", idle.sends)
	}
	assertBudgetHandoff(t, l)
}
