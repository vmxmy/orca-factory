package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type idleOrca struct {
	fakeOrca
	poll, terminalCalls int
	worktree            string
	sends               [][2]string
	sendErr             error
	unconfirmed         bool
	tasks               func(int) []orca.Task
	terminals           func(int) ([]orca.Terminal, error)
}

func (f *idleOrca) WorkerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	r, err := f.fakeOrca.WorkerStart(ctx, request, opts)
	var marshalErr error
	r["worktreePath"], marshalErr = json.Marshal(f.worktree)
	if marshalErr != nil {
		return nil, marshalErr
	}
	return r, err
}
func (f *idleOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	f.poll++
	return f.tasks(f.poll), nil
}
func (f *idleOrca) TerminalList(context.Context) ([]orca.Terminal, error) {
	f.terminalCalls++
	return f.terminals(f.poll)
}
func idleTask(done bool) []orca.Task {
	task := orca.Task{ID: "task_one", RunID: "run_one", DispatchID: "ctx_one"}
	if done {
		task.Result = json.RawMessage(`{"provenance":"worker_report","outcome":"succeeded"}`)
	}
	return []orca.Task{task}
}
func TestIdleDetection(t *testing.T) {
	for _, scenario := range []string{"once", "new output", "completed", "read error"} {
		t.Run(scenario, func(t *testing.T) {
			e, _, _, _, _, ticket := fixture(t, "new-sha")
			now := e.Now()
			initial := now
			e.Now = func() time.Time { return now }
			e.Config.PollInterval = 6 * time.Minute
			e.Sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
			o := &idleOrca{worktree: "/worker"}
			o.tasks = func(n int) []orca.Task {
				if n > 6 {
					t.Fatal("polling did not finish")
				}
				tasks := idleTask(n == 5)
				if scenario == "completed" {
					tasks[0].Status = "completed"
				}
				return tasks
			}
			o.terminals = func(n int) ([]orca.Terminal, error) {
				if scenario == "read error" {
					return nil, errors.New("private detail")
				}
				last := initial.Add(-16 * time.Minute)
				if scenario == "new output" {
					last = initial.Add(-14 * time.Minute)
					if n > 1 {
						last = now
					}
				}
				return []orca.Terminal{{WorktreeID: "repo::/worker", LastOutputAt: last.UnixMilli()}}, nil
			}
			e.Orca = o
			result, err := e.Execute(context.Background(), ticket)
			want := 0
			if scenario == "once" {
				want = 1
			}
			if err != nil || result.State.Infra != want || result.State.Owner != machine.OwnerMerge {
				t.Fatalf("result=%+v err=%v want infra=%d", result, err, want)
			}
			calls := 4
			if scenario == "completed" {
				calls = 0
			}
			if o.terminalCalls != calls {
				t.Fatalf("terminal calls=%d want=%d", o.terminalCalls, calls)
			}
		})
	}
}

func TestIdleBudgetExhaustion(t *testing.T) {
	e, _, l, g, _, ticket := fixture(t, "new-sha")
	now := e.Now()
	e.Now = func() time.Time { return now }
	e.Config.WorkerIdleTimeout = time.Minute
	e.Config.PollInterval = time.Minute
	e.Sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
	o := &idleOrca{worktree: "/worker"}
	o.tasks = func(n int) []orca.Task {
		if n > 5 {
			t.Fatal("budget did not end waiting")
		}
		return idleTask(false)
	}
	o.terminals = func(n int) ([]orca.Terminal, error) {
		return []orca.Terminal{{WorktreePath: "/worker", LastOutputAt: now.Add(-2 * time.Minute).UnixMilli()}}, nil
	}
	e.Orca = o
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerHuman || result.State.Infra != machine.DefaultLimits.Infra {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if o.releases != 0 || len(g.created) != 0 || !strings.Contains(l.comments[len(l.comments)-1], "转人:") {
		t.Fatalf("release=%d PR=%v comments=%v", o.releases, g.created, l.comments)
	}
}

func TestIdleUnknownWorktree(t *testing.T) {
	for _, path := range []string{"", "/missing"} {
		t.Run(path, func(t *testing.T) {
			e, _, _, _, _, ticket := fixture(t, "new-sha")
			e.Sleep = func(context.Context, time.Duration) error { return nil }
			o := &idleOrca{worktree: path}
			o.tasks = func(n int) []orca.Task {
				if n > 2 {
					t.Fatal("polling did not finish")
				}
				return idleTask(n == 2)
			}
			o.terminals = func(int) ([]orca.Terminal, error) {
				return []orca.Terminal{{WorktreePath: "/other", LastOutputAt: 1}}, nil
			}
			e.Orca = o
			result, err := e.Execute(context.Background(), ticket)
			if err != nil || result.State.Infra != 0 || result.State.Owner != machine.OwnerMerge {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if path == "" && o.terminalCalls != 0 {
				t.Fatal("read terminals without worktree")
			}
		})
	}
}

func TestIdleTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 15 * time.Minute}, {"0", 15 * time.Minute}, {"-1", 15 * time.Minute}, {"invalid", 15 * time.Minute},
		{"1.5", 15 * time.Minute}, {"9223372036854775807", 15 * time.Minute}, {"2", 2 * time.Minute},
	} {
		if got := workerIdleTimeout(tc.value); got != tc.want {
			t.Fatalf("value=%q got=%v want=%v", tc.value, got, tc.want)
		}
	}
}

func (f *idleOrca) TerminalSend(_ context.Context, terminal, text string) (orca.Receipt, error) {
	f.sends = append(f.sends, [2]string{terminal, text})
	if f.sendErr != nil || f.unconfirmed {
		return orca.Receipt{"send": json.RawMessage(`{"prompt":{"stages":["input_accepted"]}}`)}, f.sendErr
	}
	return orca.Receipt{"send": json.RawMessage(`{"prompt":{"stages":["input_accepted","turn_started"]}}`)}, nil
}

func TestIdleNudge(t *testing.T) {
	t.Run("full grace interval", func(t *testing.T) {
		e, _, _, _, _, _ := fixture(t, "new-sha")
		now := e.Now()
		e.Now = func() time.Time { return now }
		o := &idleOrca{worktree: "/worker"}
		o.terminals = func(int) ([]orca.Terminal, error) {
			return []orca.Terminal{{Handle: "term_worker", WorktreePath: "/worker", LastOutputAt: 1}}, nil
		}
		e.Orca = o
		state := machine.Initial()
		var idle workerIdle
		if idle.observe(context.Background(), e, "/worker", time.Minute, state) {
			t.Fatal("first idle counted before nudge grace")
		}
		now = now.Add(time.Minute)
		if idle.observe(context.Background(), e, "/worker", time.Minute, state) {
			t.Fatal("grace interval did not restart at nudge")
		}
		now = now.Add(time.Millisecond)
		if !idle.observe(context.Background(), e, "/worker", time.Minute, state) {
			t.Fatal("idle beyond grace not counted")
		}
		if idle.observe(context.Background(), e, "/worker", time.Minute, state) {
			t.Fatal("same idle counted twice")
		}
		if len(o.sends) != 1 {
			t.Fatalf("sends=%v", o.sends)
		}
	})

	t.Run("unconfirmed nudge", func(t *testing.T) {
		e, _, _, _, _, _ := fixture(t, "new-sha")
		now := e.Now()
		e.Now = func() time.Time { return now }
		o := &idleOrca{worktree: "/worker", unconfirmed: true}
		o.terminals = func(int) ([]orca.Terminal, error) {
			return []orca.Terminal{{Handle: "term_worker", WorktreePath: "/worker", LastOutputAt: 1}}, nil
		}
		e.Orca = o
		var idle workerIdle
		if !idle.observe(context.Background(), e, "/worker", time.Minute, machine.Initial()) {
			t.Fatal("unconfirmed nudge granted a grace interval")
		}
		if len(o.sends) != 1 {
			t.Fatalf("sends=%v", o.sends)
		}
	})

	t.Run("paused", func(t *testing.T) {
		e, _, _, _, _, _ := fixture(t, "new-sha")
		o := &idleOrca{worktree: "/worker"}
		o.terminals = func(int) ([]orca.Terminal, error) {
			return []orca.Terminal{{Handle: "term_worker", WorktreePath: "/worker", LastOutputAt: 1}}, nil
		}
		e.Orca = o
		state := machine.Initial()
		state.Paused[machine.DepLinear] = false
		state.Paused[machine.DepGitHub] = true
		var idle workerIdle
		for i := 0; i < 3; i++ {
			if idle.observe(context.Background(), e, "/worker", time.Minute, state) {
				t.Fatal("paused worker counted as stalled")
			}
		}
		if len(o.sends) != 0 {
			t.Fatalf("paused sends=%v", o.sends)
		}
	})
	for _, scenario := range []string{"output recovery", "worker_done recovery", "still idle", "idle again", "send failure"} {
		t.Run(scenario, func(t *testing.T) {
			e, _, _, _, _, ticket := fixture(t, "new-sha")
			now := e.Now()
			initial := now
			e.Now = func() time.Time { return now }
			e.Config.WorkerIdleTimeout = 2 * time.Minute
			e.Config.PollInterval = time.Minute
			e.Sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
			o := &idleOrca{worktree: "/worker"}
			if scenario == "send failure" {
				o.sendErr = errors.New("send failed")
			}
			doneAt := 6
			if scenario == "worker_done recovery" {
				doneAt = 2
			}
			o.tasks = func(n int) []orca.Task {
				if n > doneAt {
					t.Fatal("polling did not finish")
				}
				if n == 2 && len(o.sends) != 1 {
					t.Fatalf("first idle sent %d nudges, want 1", len(o.sends))
				}
				return idleTask(n == doneAt)
			}
			o.terminals = func(n int) ([]orca.Terminal, error) {
				last := initial.Add(-3 * time.Minute)
				if n > 1 && scenario == "output recovery" {
					last = now
				}
				if n > 1 && scenario == "idle again" {
					last = initial.Add(time.Minute)
				}
				return []orca.Terminal{{Handle: "term_worker", WorktreePath: "/worker", LastOutputAt: last.UnixMilli()}}, nil
			}
			e.Orca = o
			result, err := e.Execute(context.Background(), ticket)
			want := 1
			if scenario == "output recovery" || scenario == "worker_done recovery" {
				want = 0
			}
			if err != nil || result.State.Infra != want || result.State.Owner != machine.OwnerMerge {
				t.Fatalf("result=%+v err=%v want infra=%d", result, err, want)
			}
			if len(o.sends) != 1 || o.sends[0] != [2]string{"term_worker", "请按任务继续；完成时用 worker_done 回报"} {
				t.Fatalf("sends=%v", o.sends)
			}
		})
	}
}
