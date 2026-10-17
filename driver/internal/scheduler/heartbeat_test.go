package scheduler

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
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
}

func (f *reapOrca) WorkerAbandon(_ context.Context, request, dispatch string) (orca.Receipt, error) {
	if request != orca.RequestID("OF-1", "scheduler", "abandon-"+dispatch, 1) {
		panic("wrong abandon request")
	}
	f.abandoned = append(f.abandoned, dispatch)
	return orca.Receipt{}, nil
}
func (f *reapOrca) TerminalClose(_ context.Context, terminal string) error {
	f.closed = append(f.closed, terminal)
	return nil
}

const terminalListWithExecutor = `{"ok":true,"result":{"terminals":[{"handle":"term_exec"}],"totalCount":1,"truncated":false,"hostScope":{"omittedHostIds":[]}}}`

func TestExecutorHeartbeatLiveness(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	t.Run("tick decisions", func(t *testing.T) {
		executor := Ticket{ID: "e", Identifier: "OF-1", Repo: "a", Owner: machine.OwnerExecutor, Phase: machine.PhaseWorker, Attempt: 1, Terminal: "term_exec", Run: "run_1", UpdatedAt: now}
		resolverTicket := Ticket{ID: "r", Identifier: "OF-2", Repo: "a", Owner: machine.OwnerResolver, Phase: machine.PhaseResolver, Attempt: 1, Terminal: "term_res", UpdatedAt: now}
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
		} {
			_ = os.Remove(path)
			if tc.beat != nil {
				if err := orca.WriteBeat(path, *tc.beat); err != nil {
					t.Fatal(err)
				}
			}
			r.PIDAlive = func(int) bool { return tc.pidExists }
			got, err := r.Liveness(context.Background(), ticket)
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
		o := &reapOrca{fakeOrca: fakeOrca{pages: []orca.Receipt{receipt(`{"scope":{"run":"run_1"},"page":{"hasMore":false},"workers":[{"dispatchId":"ctx_live","dispatchStatus":"running"},{"dispatchId":"ctx_done","dispatchStatus":"completed"}]}`)}}}
		r := &Runtime{Orca: o, StateRoot: root, OrcaPath: "/orca", Now: func() time.Time { return now },
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
		if strings.Join(o.abandoned, ",") != "ctx_live" || strings.Join(o.closed, ",") != "term_exec" {
			t.Fatalf("abandoned=%v closed=%v", o.abandoned, o.closed)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale heartbeat kept: %v", err)
		}
	})
}
