package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type livenessOrca struct {
	terminalCalls int
	fakeOrca
	receipt      orca.Receipt
	polls, stops int
	stopErr      error
	report       bool
	change       func(int, orca.Receipt)
}

func (o *livenessOrca) WorkerStart(ctx context.Context, req string, opts orca.StartOptions) (orca.Receipt, error) {
	r, err := o.fakeOrca.WorkerStart(ctx, req, opts)
	delete(r, "worktreePath")
	return r, err
}
func (o *livenessOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	return idleTask(o.report), nil
}
func (o *livenessOrca) WorkerList(_ context.Context, opts orca.ListOptions) (orca.Receipt, error) {
	if opts.Run != "run_one" || !opts.IncludeRemote {
		panic("wrong worker list scope")
	}
	o.polls++
	if o.change != nil {
		o.change(o.polls, o.receipt)
	}
	return o.receipt, nil
}
func (o *livenessOrca) TerminalList(context.Context) ([]orca.Terminal, error) {
	o.terminalCalls++
	return nil, nil
}
func (o *livenessOrca) WorkerStop(_ context.Context, request, dispatch string) (orca.Receipt, error) {
	if request == "" || dispatch != "ctx_one" {
		panic("wrong stop target")
	}
	o.stops++
	return orca.Receipt{}, o.stopErr
}
func livenessFixture(t *testing.T, verdict string) *livenessOrca {
	t.Helper()
	var env struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(recorded(t, "orca-worker-list-liveness.json"), &env); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(env.Result["workers"], &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		var p struct{ Liveness struct{ Verdict string } }
		if err := json.Unmarshal(row["projection"], &p); err != nil {
			t.Fatal(err)
		}
		if p.Liveness.Verdict == verdict {
			row["dispatchId"] = json.RawMessage(`"ctx_one"`)
			raw, _ := json.Marshal([]any{row})
			return &livenessOrca{receipt: orca.Receipt{"workers": raw}}
		}
	}
	t.Fatalf("missing recorded verdict %s", verdict)
	return nil
}
func setLiveness(o *livenessOrca, verdict, detail string) {
	var rows []map[string]any
	_ = json.Unmarshal(o.receipt["workers"], &rows)
	p := rows[0]["projection"].(map[string]any)
	p["liveness"].(map[string]any)["verdict"] = verdict
	p["stage"].(map[string]any)["detail"] = detail
	o.receipt["workers"], _ = json.Marshal(rows)
}
func TestWaitFailsExitedWorker(t *testing.T) {
	e, _, l, _, _, ticket := fixture(t, "new-sha")
	o := livenessFixture(t, "exited")
	e.Orca = o
	e.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	result, _ := e.Execute(context.Background(), ticket)
	if result.State.Phase != machine.PhaseCorrection {
		t.Fatalf("state=%+v; expected correction", result.State)
	}
	if o.stops != 0 || o.releases != 0 {
		t.Fatalf("stop=%d release=%d without worker report", o.stops, o.releases)
	}
	if !strings.Contains(strings.Join(l.comments, "\n"), "worker exited without worker_done") {
		t.Fatalf("comments=%q", l.comments)
	}
	cp, err := readCheckpoint(e.Config, ticket)
	if err != nil || cp == nil || cp.Worktree != "/home/laoxu/orca/workspaces/orca-factory/ZII-886-executor" {
		t.Fatalf("checkpoint=%+v err=%v", cp, err)
	}
	if len(o.boardUpdates) == 0 {
		t.Fatal("recovered worktree was not used for board updates")
	}
}
func TestWaitStopsWorkerThatNeverStarted(t *testing.T) {
	for _, elapsed := range []time.Duration{5*time.Minute - time.Nanosecond, 5 * time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			e, _, l, _, _, ticket := fixture(t, "new-sha")
			o := livenessFixture(t, "exited")
			setLiveness(o, "live", "turn_start_unobserved")
			e.Orca = o
			now := e.Now()
			e.Now = func() time.Time { return now }
			sleeps := 0
			e.Sleep = func(context.Context, time.Duration) error {
				sleeps++
				if sleeps > 1 {
					return context.Canceled
				}
				now = now.Add(elapsed)
				return nil
			}
			result, _ := e.Execute(context.Background(), ticket)
			want := 0
			if elapsed >= 5*time.Minute {
				want = 1
			}
			if o.stops != want {
				t.Fatalf("stops=%d want=%d", o.stops, want)
			}
			if want == 1 && (result.State.Phase != machine.PhaseCorrection || !strings.Contains(strings.Join(l.comments, "\n"), "worker never started its turn")) {
				t.Fatalf("state=%+v comments=%q", result.State, l.comments)
			}
			if want == 0 && len(o.boardUpdates) == 0 {
				t.Fatal("recovered worktree missing running board status")
			}
			if want == 0 && result.State.Phase != machine.PhaseWorker {
				t.Fatalf("early failure: %+v", result.State)
			}
			if o.releases != 0 {
				t.Fatal("released without worker report")
			}
		})
	}
}
func TestWaitIgnoresUnverifiable(t *testing.T) {
	for _, detail := range []string{"settled", "turn_start_unobserved"} {
		t.Run(detail, func(t *testing.T) {
			e, _, _, _, _, ticket := fixture(t, "new-sha")
			o := livenessFixture(t, "unverifiable")
			setLiveness(o, "unverifiable", detail)
			e.Orca = o
			now := e.Now()
			e.Now = func() time.Time { return now }
			e.Sleep = func(context.Context, time.Duration) error {
				now = now.Add(6 * time.Minute)
				if o.polls >= 3 {
					return context.Canceled
				}
				return nil
			}
			result, err := e.Execute(context.Background(), ticket)
			if !errors.Is(err, context.Canceled) || result.State.Phase != machine.PhaseWorker || result.State.Infra != 0 || o.stops != 0 || o.releases != 0 {
				t.Fatalf("state=%+v err=%v stops=%d releases=%d", result.State, err, o.stops, o.releases)
			}
			if o.terminalCalls != 0 {
				t.Fatalf("unverifiable invoked idle detection %d times", o.terminalCalls)
			}
			if _, err := os.Stat(checkpointPath(e.Config, ticket)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWaitReportOverridesExited(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "new-sha")
	o := livenessFixture(t, "exited")
	o.report = true
	e.Orca = o
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Phase != machine.PhaseMergeQueue || o.stops != 0 || o.releases != 1 {
		t.Fatalf("state=%+v err=%v stops=%d releases=%d", result.State, err, o.stops, o.releases)
	}
}

func TestWaitRequiresContinuousUnobservedTurn(t *testing.T) {
	e, _, _, _, _, _ := fixture(t, "new-sha")
	o := livenessFixture(t, "exited")
	e.Orca = o
	o.change = func(n int, _ orca.Receipt) {
		detail := "turn_start_unobserved"
		if n == 2 {
			detail = "turn_started"
		}
		setLiveness(o, "live", detail)
	}
	now := e.Now()
	e.Now = func() time.Time { return now }
	e.Sleep = func(context.Context, time.Duration) error {
		now = now.Add(3 * time.Minute)
		if o.polls == 4 {
			return context.Canceled
		}
		return nil
	}
	_, err := e.wait(context.Background(), "run_one", "task_one", "ctx_one", time.Second, &workerWatch{}, func() error { return nil })
	if !errors.Is(err, context.Canceled) || o.stops != 0 {
		t.Fatalf("err=%v stops=%d", err, o.stops)
	}
}

func TestWaitDoesNotRepeatUncertainStop(t *testing.T) {
	e, _, _, _, _, _ := fixture(t, "new-sha")
	o := livenessFixture(t, "exited")
	setLiveness(o, "live", "turn_start_unobserved")
	o.stopErr = errors.New("uncertain stop")
	e.Orca = o
	now := e.Now()
	e.Now = func() time.Time { return now }
	e.Sleep = func(context.Context, time.Duration) error { now = now.Add(5 * time.Minute); return nil }
	watch := &workerWatch{}
	for i := 0; i < 2; i++ {
		report, err := e.wait(context.Background(), "run_one", "task_one", "ctx_one", time.Second, watch, func() error { return nil })
		if !errors.Is(err, o.stopErr) || report != nil {
			t.Fatalf("report=%+v err=%v", report, err)
		}
	}
	if o.stops != 1 {
		t.Fatalf("stops=%d", o.stops)
	}
}
