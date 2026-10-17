package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type livenessBackend struct {
	fakeBackend
	verdict orca.Liveness
	reapErr error
	reaped  []string
}

func (f *livenessBackend) Liveness(context.Context, Ticket) (orca.Liveness, error) {
	return f.verdict, nil
}
func (f *livenessBackend) ReapDead(_ context.Context, t Ticket) error {
	f.reaped = append(f.reaped, t.Identifier)
	return f.reapErr
}

type reapOrca struct {
	fakeOrca
	abandoned []string
	closed    []string
	closeErr  error
	abandon   orca.Receipt
}

func (f *reapOrca) WorkerAbandon(_ context.Context, request, dispatch string) (orca.Receipt, error) {
	if request != orca.RequestID("OF-1", "scheduler", "abandon-"+dispatch, 1) {
		panic("wrong abandon request")
	}
	f.abandoned = append(f.abandoned, dispatch)
	return f.abandon, nil
}
func (f *reapOrca) TerminalClose(_ context.Context, terminal string) error {
	f.closed = append(f.closed, terminal)
	return f.closeErr
}

// abandonReceipt is the result object WorkerAbandon returns for a recorded
// abandon sample: Orca keeps the worker's agent terminal open.
func abandonReceipt(t *testing.T, sample string) orca.Receipt {
	t.Helper()
	var envelope struct{ Result orca.Receipt }
	if err := json.Unmarshal(recorded(t, sample), &envelope); err != nil || envelope.Result == nil {
		t.Fatalf("abandon sample: %v", err)
	}
	return envelope.Result
}

const residualTerminal = "term_78b5d1c8-4cc9-470b-ab4a-7d48cc31622d"

const terminalListWithExecutor = `{"ok":true,"result":{"terminals":[{"handle":"term_exec"}],"totalCount":1,"truncated":false,"hostScope":{"omittedHostIds":[]}}}`

func TestExecutorHeartbeatLiveness(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	t.Run("tick decisions", func(t *testing.T) {
		executor := Ticket{ID: "e", Identifier: "OF-1", Repo: "a", Owner: machine.OwnerExecutor, Phase: machine.PhaseWorker, Attempt: 1, Terminal: "term_exec", Run: "run_1", UpdatedAt: now}
		resolverTicket := Ticket{ID: "r", Identifier: "OF-2", Repo: "a", Owner: machine.OwnerResolver, Phase: machine.PhaseResolver, Attempt: 1, Terminal: "term_res", UpdatedAt: now}
		// A claim past the unregistered grace whose executor has not yet registered its terminal.
		unregisteredTicket := Ticket{ID: "u", Identifier: "OF-3", Repo: "a", Owner: machine.OwnerExecutor, Phase: machine.PhaseWorker, Attempt: 1, Terminal: "-", UpdatedAt: now.Add(-time.Hour)}
		unregisteredResolver := Ticket{ID: "ur", Identifier: "OF-4", Repo: "a", Owner: machine.OwnerResolver, Phase: machine.PhaseResolver, Attempt: 1, Terminal: "-", UpdatedAt: now.Add(-time.Hour)}
		for _, tc := range []struct {
			name     string
			ticket   Ticket
			verdict  orca.Liveness
			reapErr  error
			terminal bool // legacy evidence: terminal and dispatch both gone
			actions  int
			reaps    int
		}{
			{"alive heartbeat beats missing terminal", executor, orca.LivenessAlive, nil, true, 0, 0},
			{"uncertain heartbeat never redispatches", executor, orca.LivenessUncertain, nil, true, 0, 0},
			{"dead heartbeat beats live terminal and dispatch", executor, orca.LivenessDead, nil, false, 1, 1},
			{"failed reap keeps the ticket", executor, orca.LivenessDead, errors.New("orca"), false, 0, 1},
			{"no heartbeat falls back to terminal and dispatch evidence", executor, orca.LivenessNone, nil, true, 1, 0},
			{"no heartbeat and live terminal stays", executor, orca.LivenessNone, nil, false, 0, 0},
			{"dead resolver is resolved again", resolverTicket, orca.LivenessDead, nil, false, 1, 1},
			{"alive resolver stays", resolverTicket, orca.LivenessAlive, nil, true, 0, 0},
			{"alive heartbeat keeps an unregistered claim", unregisteredTicket, orca.LivenessAlive, nil, true, 0, 0},
			{"uncertain heartbeat keeps an unregistered claim", unregisteredTicket, orca.LivenessUncertain, nil, true, 0, 0},
			{"dead heartbeat recovers an unregistered claim", unregisteredTicket, orca.LivenessDead, nil, true, 1, 1},
			{"no heartbeat recovers an unregistered claim after grace", unregisteredTicket, orca.LivenessNone, nil, true, 1, 0},
			{"alive heartbeat keeps an unregistered resolver", unregisteredResolver, orca.LivenessAlive, nil, true, 0, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				b := &livenessBackend{fakeBackend: fakeBackend{tickets: []Ticket{tc.ticket}, absent: tc.terminal, inactive: tc.terminal}, verdict: tc.verdict, reapErr: tc.reapErr}
				s := setup(&b.fakeBackend)
				s.Backend = b
				err := s.Tick(context.Background())
				if (tc.reapErr != nil) != (err != nil) {
					t.Fatalf("tick err=%v, reap err=%v", err, tc.reapErr)
				}
				if len(b.actions) != tc.actions || len(b.reaped) != tc.reaps {
					t.Fatalf("actions=%v reaped=%v, want %d actions %d reaps", b.actions, b.reaped, tc.actions, tc.reaps)
				}
			})
		}
	})

	t.Run("heartbeat verdict", func(t *testing.T) {
		root := t.TempDir()
		r := &Runtime{StateRoot: root, HeartbeatTimeout: 5 * time.Minute, Now: func() time.Time { return now }, Repositories: map[string]config.Repository{"/repo": {Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "o/proj"}}}}
		ticket := Ticket{Identifier: "OF-1", Repo: "/repo", Owner: machine.OwnerExecutor, Terminal: "term_exec"}
		path := orca.HeartbeatPath(root, "proj", "OF-1", "executor")
		for _, tc := range []struct {
			name      string
			beat      *orca.Beat
			pidExists bool
			want      orca.Liveness
		}{
			{"no file", nil, false, orca.LivenessNone},
			{"fresh and pid exists", &orca.Beat{PID: 7, Terminal: "term_exec", At: now.Add(-time.Minute)}, true, orca.LivenessAlive},
			{"stale and pid gone", &orca.Beat{PID: 7, Terminal: "term_exec", At: now.Add(-6 * time.Minute)}, false, orca.LivenessDead},
			{"fresh but pid gone", &orca.Beat{PID: 7, Terminal: "term_exec", At: now.Add(-time.Minute)}, false, orca.LivenessUncertain},
			{"stale but pid exists", &orca.Beat{PID: 7, Terminal: "term_exec", At: now.Add(-6 * time.Minute)}, true, orca.LivenessUncertain},
			{"beat from another terminal", &orca.Beat{PID: 7, Terminal: "term_old", At: now.Add(-6 * time.Minute)}, false, orca.LivenessNone},
			{"unregistered claim with fresh beat", &orca.Beat{PID: 7, Terminal: "term_exec", At: now.Add(-time.Minute)}, true, orca.LivenessAlive},
			{"unregistered claim with stale beat", &orca.Beat{PID: 7, Terminal: "term_exec", At: now.Add(-6 * time.Minute)}, false, orca.LivenessDead},
		} {
			_ = os.Remove(path)
			if tc.beat != nil {
				if err := orca.WriteBeat(path, *tc.beat); err != nil {
					t.Fatal(err)
				}
			}
			r.PIDAlive = func(int) bool { return tc.pidExists }
			subject := ticket
			if strings.HasPrefix(tc.name, "unregistered") {
				subject.Terminal = "-"
			}
			got, err := r.Liveness(context.Background(), subject)
			if err != nil || got != tc.want {
				t.Fatalf("%s: got %q err=%v, want %q", tc.name, got, err, tc.want)
			}
		}
		if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		if got, err := r.Liveness(context.Background(), ticket); err == nil || got != orca.LivenessUncertain {
			t.Fatalf("corrupt heartbeat: got %q err=%v, want uncertain with error", got, err)
		}
	})

	t.Run("reap fences dispatches and closes the terminal", func(t *testing.T) {
		root := t.TempDir()
		path := orca.HeartbeatPath(root, "proj", "OF-1", "executor")
		if err := orca.WriteBeat(path, orca.Beat{PID: 7, Terminal: "term_exec", At: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		o := &reapOrca{fakeOrca: fakeOrca{pages: []orca.Receipt{receipt(`{"scope":{"run":"run_1"},"page":{"hasMore":false},"workers":[{"dispatchId":"ctx_live","dispatchStatus":"running"},{"dispatchId":"ctx_done","dispatchStatus":"completed"}]}`)}}, abandon: abandonReceipt(t, "orca-worker-abandon.json")}
		r := &Runtime{Orca: o, StateRoot: root, OrcaPath: "/orca", Now: func() time.Time { return now }, PIDAlive: func(int) bool { return false },
			Repositories: map[string]config.Repository{"/repo": {Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "o/proj"}}},
			Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
				if strings.Join(spec.Args, " ") != "terminal list --limit 10000 --json" {
					t.Fatalf("unexpected command %v", spec.Args)
				}
				return []byte(terminalListWithExecutor), nil
			}}
		ticket := Ticket{Identifier: "OF-1", Repo: "/repo", Owner: machine.OwnerExecutor, Terminal: "term_exec", Run: "run_1"}
		if err := r.ReapDead(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		if strings.Join(o.abandoned, ",") != "ctx_live" || strings.Join(o.closed, ",") != "term_exec,"+residualTerminal {
			t.Fatalf("abandoned=%v closed=%v", o.abandoned, o.closed)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale heartbeat kept: %v", err)
		}
		// Orca refuses a handle it no longer knows (orca-terminal-close-stale.json): already closed.
		if err := orca.WriteBeat(path, orca.Beat{PID: 7, Terminal: "term_exec", At: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		o.page, o.closeErr = 0, &orca.RejectedError{Code: "terminal_handle_stale"}
		if err := r.ReapDead(context.Background(), ticket); err != nil {
			t.Fatalf("stale handle close err=%v", err)
		}
		if err := orca.WriteBeat(path, orca.Beat{PID: 7, Terminal: "term_exec", At: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		o.page, o.closeErr = 0, &orca.RejectedError{Code: "runtime_error"}
		if err := r.ReapDead(context.Background(), ticket); err == nil {
			t.Fatal("failed terminal close accepted")
		}
		if err := orca.WriteBeat(path, orca.Beat{PID: 7, Terminal: "term_exec", At: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		o.abandon = maps.Clone(o.abandon)
		o.abandon["state"] = json.RawMessage(`"running"`)
		o.page, o.closeErr, o.closed = 0, nil, nil
		if err := r.ReapDead(context.Background(), ticket); err == nil {
			t.Fatal("abandon receipt without state abandoned accepted")
		}
		if _, err := os.Stat(path); err != nil || len(o.closed) != 0 {
			t.Fatalf("unconfirmed abandon reaped: heartbeat err=%v closed=%v", err, o.closed)
		}
	})
	t.Run("reap retries the terminal close of a dispatch it already abandoned", func(t *testing.T) {
		root := t.TempDir()
		path := orca.HeartbeatPath(root, "proj", "OF-1", "executor")
		if err := orca.WriteBeat(path, orca.Beat{PID: 7, Run: "run_1", At: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		o := &reapOrca{fakeOrca: fakeOrca{pages: []orca.Receipt{
			receipt(`{"scope":{"run":"run_1"},"page":{"hasMore":false},"workers":[{"dispatchId":"ctx_live","dispatchStatus":"running"}]}`),
			receipt(`{"scope":{"run":"run_1"},"page":{"hasMore":false},"workers":[{"dispatchId":"ctx_live","dispatchStatus":"failed","workerState":"abandoned"}]}`),
		}}, abandon: abandonReceipt(t, "orca-worker-abandon.json"), closeErr: &orca.RejectedError{Code: "runtime_error"}}
		r := &Runtime{Orca: o, StateRoot: root, Now: func() time.Time { return now }, PIDAlive: func(int) bool { return false },
			Repositories: map[string]config.Repository{"/repo": {Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "o/proj"}}},
			Run: func(context.Context, runx.Spec) ([]byte, error) {
				t.Fatal("unregistered executor has no terminal to list")
				return nil, nil
			}}
		ticket := Ticket{Identifier: "OF-1", Repo: "/repo", Owner: machine.OwnerExecutor, Terminal: "-"}
		if err := r.ReapDead(context.Background(), ticket); err == nil {
			t.Fatal("failed terminal close accepted")
		}
		// The next round sees the dispatch abandoned; the repeat abandon (orca-worker-abandon-settled.json) still names its terminal.
		o.abandon, o.closeErr, o.abandoned, o.closed = abandonReceipt(t, "orca-worker-abandon-settled.json"), nil, nil, nil
		if err := r.ReapDead(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		if strings.Join(o.abandoned, ",") != "ctx_live" || strings.Join(o.closed, ",") != residualTerminal {
			t.Fatalf("abandoned=%v closed=%v", o.abandoned, o.closed)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale heartbeat kept: %v", err)
		}
	})
	t.Run("reap fences the run the dead process last beat", func(t *testing.T) {
		root := t.TempDir()
		path := orca.HeartbeatPath(root, "proj", "OF-1", "executor")
		if err := orca.WriteBeat(path, orca.Beat{PID: 7, Run: "run_beat", At: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		o := &reapOrca{fakeOrca: fakeOrca{pages: []orca.Receipt{receipt(`{"scope":{"run":"run_beat"},"page":{"hasMore":false},"workers":[{"dispatchId":"ctx_beat","dispatchStatus":"running"}]}`)}}, abandon: abandonReceipt(t, "orca-worker-abandon.json")}
		r := &Runtime{Orca: o, StateRoot: root, Now: func() time.Time { return now }, PIDAlive: func(int) bool { return false },
			Repositories: map[string]config.Repository{"/repo": {Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "o/proj"}}},
			Run: func(context.Context, runx.Spec) ([]byte, error) {
				t.Fatal("unregistered executor has no terminal to list")
				return nil, nil
			}}
		ticket := Ticket{Identifier: "OF-1", Repo: "/repo", Owner: machine.OwnerExecutor, Terminal: "-"}
		if got, err := r.Liveness(context.Background(), ticket); err != nil || got != orca.LivenessDead {
			t.Fatalf("liveness got %q err=%v, want dead", got, err)
		}
		if err := r.ReapDead(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		if strings.Join(o.abandoned, ",") != "ctx_beat" {
			t.Fatalf("abandoned=%v", o.abandoned)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale heartbeat kept: %v", err)
		}
		if err := orca.WriteBeat(path, orca.Beat{PID: 7, Run: "run_beat", At: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if got, err := r.Liveness(context.Background(), ticket); err != nil || got != orca.LivenessDead {
			t.Fatalf("liveness got %q err=%v, want dead", got, err)
		}
		// A relaunched process replaced the beat after the dead verdict: its Run is not reaped.
		if err := orca.WriteBeat(path, orca.Beat{PID: 8, Run: "run_new", At: now}); err != nil {
			t.Fatal(err)
		}
		o.page, o.abandoned, o.closed = 0, nil, nil
		if err := r.ReapDead(context.Background(), ticket); err == nil {
			t.Fatal("reap of a heartbeat no longer dead succeeded")
		}
		if beat, found, err := orca.ReadBeat(path); err != nil || !found || beat.Run != "run_new" || len(o.abandoned) != 0 || len(o.closed) != 0 || o.page != 0 {
			t.Fatalf("replaced heartbeat reaped: beat=%+v found=%v err=%v abandoned=%v closed=%v pages=%d", beat, found, err, o.abandoned, o.closed, o.page)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := r.ReapDead(context.Background(), ticket); err == nil {
			t.Fatal("reap after the heartbeat vanished succeeded")
		}
		if len(o.abandoned) != 0 || len(o.closed) != 0 {
			t.Fatalf("vanished heartbeat reaped: abandoned=%v closed=%v", o.abandoned, o.closed)
		}
	})
	t.Run("resolver reap fences only its own run", func(t *testing.T) {
		root := t.TempDir()
		path := orca.HeartbeatPath(root, "proj", "OF-1", "resolver")
		if err := orca.WriteBeat(path, orca.Beat{PID: 7, Run: "run_res", At: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		o := &reapOrca{fakeOrca: fakeOrca{pages: []orca.Receipt{receipt(`{"scope":{"run":"run_res"},"page":{"hasMore":false},"workers":[{"dispatchId":"ctx_res","dispatchStatus":"running"}]}`)}}, abandon: abandonReceipt(t, "orca-worker-abandon.json")}
		r := &Runtime{Orca: o, StateRoot: root, Now: func() time.Time { return now }, PIDAlive: func(int) bool { return false },
			Repositories: map[string]config.Repository{"/repo": {Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "o/proj"}}},
			Run: func(context.Context, runx.Spec) ([]byte, error) {
				t.Fatal("unregistered resolver has no terminal to list")
				return nil, nil
			}}
		ticket := Ticket{Identifier: "OF-1", Repo: "/repo", Owner: machine.OwnerResolver, Terminal: "-", Run: "run_src", Resolution: &resolver.Record{Run: "run_res"}}
		if err := r.ReapDead(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		if strings.Join(o.abandoned, ",") != "ctx_res" || strings.Join(o.closed, ",") != residualTerminal {
			t.Fatalf("abandoned=%v closed=%v", o.abandoned, o.closed)
		}
		// Killed before its first Resolution save: the beat alone names its Run.
		if err := orca.WriteBeat(path, orca.Beat{PID: 7, Run: "run_res", At: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		ticket.Resolution = nil
		o.page, o.abandoned, o.closed = 0, nil, nil
		if err := r.ReapDead(context.Background(), ticket); err != nil {
			t.Fatalf("resolver reap before its first resolution record: %v", err)
		}
		if strings.Join(o.abandoned, ",") != "ctx_res" {
			t.Fatalf("abandoned=%v", o.abandoned)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale heartbeat kept: %v", err)
		}
	})
}
