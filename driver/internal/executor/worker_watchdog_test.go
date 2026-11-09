package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// busyWorker never reports worker_done while its terminal output keeps advancing.
func busyWorker(t *testing.T, now *time.Time) *idleOrca {
	t.Helper()
	o := &idleOrca{worktree: "/worker"}
	o.tasks = func(n int) []orca.Task {
		if n > 30 {
			t.Fatal("busy worker was waited on past the run limit")
		}
		return idleTask(false)
	}
	o.terminals = func(int) ([]orca.Terminal, error) {
		return []orca.Terminal{{Handle: "term_worker", WorktreePath: "/worker", LastOutputAt: now.UnixMilli()}}, nil
	}
	return o
}

func TestWorkerWatchdogRepeatedSilence(t *testing.T) {
	e, _, l, g, _, ticket := fixture(t, "new-sha")
	now := e.Now()
	silentSince := now.Add(-2 * time.Minute)
	e.Now = func() time.Time { return now }
	e.Config.WorkerIdleTimeout = time.Minute
	e.Config.PollInterval = time.Minute
	e.Sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
	o := &idleOrca{worktree: "/worker"}
	o.tasks = func(n int) []orca.Task {
		if n > 30 {
			t.Fatal("silent worker was waited on indefinitely")
		}
		return idleTask(false)
	}
	o.terminals = func(int) ([]orca.Terminal, error) {
		return []orca.Terminal{{Handle: "term_worker", WorktreePath: "/worker", LastOutputAt: silentSince.UnixMilli()}}, nil
	}
	e.Orca = o
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver || result.State.Infra != machine.DefaultLimits.Infra || result.State.HumanReason != "infra budget exhausted: worker stalled" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if o.releases != 0 || len(g.created) != 0 || len(o.sends) != 1 || !strings.Contains(l.comments[len(l.comments)-1], "Agent处置:") {
		t.Fatalf("release=%d PR=%v sends=%v comments=%v", o.releases, g.created, o.sends, l.comments)
	}
}

func TestWorkerWatchdogOverrun(t *testing.T) {
	e, _, l, g, _, ticket := fixture(t, "new-sha")
	now := e.Now()
	start := now
	e.Now = func() time.Time { return now }
	e.Config.PollInterval = time.Minute
	e.Config.WorkerMaxDuration = 10 * time.Minute
	e.Sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
	o := busyWorker(t, &now)
	e.Orca = o
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver || result.State.Infra != 0 || result.State.Corrections != 0 || result.State.HumanReason != "worker overran: no worker_done within run limit" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if now.Sub(start) != 10*time.Minute || o.releases != 0 || len(g.created) != 0 || len(o.sends) != 0 {
		t.Fatalf("elapsed=%v release=%d PR=%v sends=%v", now.Sub(start), o.releases, g.created, o.sends)
	}
	lastOutput := now.Add(-time.Minute).UTC().Format(time.RFC3339)
	if want := "证据: worker 运行中；运行超时: 已运行 10 分钟；终端最后输出 " + lastOutput; !strings.Contains(l.comments[len(l.comments)-1], want) {
		t.Fatalf("comments=%q want %q", l.comments, want)
	}
}

func TestWorkerWatchdogOverrunSurvivesRestart(t *testing.T) {
	e, _, l, _, _, ticket := fixture(t, "new-sha")
	now := e.Now()
	start := now
	crashAt := start.Add(5 * time.Minute)
	e.Now = func() time.Time { return now }
	e.Config.PollInterval = time.Minute
	e.Config.WorkerMaxDuration = 10 * time.Minute
	e.Sleep = func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		if now.Equal(crashAt) {
			return context.Canceled
		}
		return nil
	}
	o := busyWorker(t, &now)
	e.Orca = o
	if result, err := e.Start(context.Background(), ticket); !errors.Is(err, context.Canceled) || result.State.Owner == machine.OwnerResolver {
		t.Fatalf("first executor result=%+v err=%v", result, err)
	}
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver || result.State.Infra != 0 {
		t.Fatalf("restarted result=%+v err=%v", result, err)
	}
	if now.Sub(start) != 10*time.Minute || len(o.workerRequests) != 1 || !strings.Contains(l.comments[len(l.comments)-1], "运行超时: 已运行 10 分钟") {
		t.Fatalf("elapsed=%v starts=%v comments=%q", now.Sub(start), o.workerRequests, l.comments)
	}
}
