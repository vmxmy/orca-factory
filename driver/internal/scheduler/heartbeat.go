package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// LivenessBackend supplies process-level liveness evidence. A terminal handle
// or an active dispatch alone never proves the process lives (OF-22).
type LivenessBackend interface {
	Liveness(context.Context, Ticket) (orca.Liveness, error)
	// ReapDead fences the dead process's dispatches and closes its terminal.
	ReapDead(context.Context, Ticket) error
}

type beatOutcome int

const (
	// beatLegacy: no heartbeat evidence; terminal and dispatch evidence decide.
	beatLegacy beatOutcome = iota
	beatSkip
	beatRecover
)

// heartbeat turns the heartbeat verdict into a Tick decision. An alive or
// uncertain verdict never redispatches; a dead one is reaped before recovery.
func (s *Scheduler) heartbeat(ctx context.Context, t Ticket, problems *[]error) beatOutcome {
	backend, ok := s.Backend.(LivenessBackend)
	if !ok {
		return beatLegacy
	}
	verdict, err := backend.Liveness(ctx, t)
	if verdict == orca.LivenessNone && err == nil {
		return beatLegacy
	}
	s.decision(t, "heartbeat", "", "terminal", t.Terminal, "run", t.Run, "result", verdict, "err", err)
	switch verdict {
	case orca.LivenessDead:
		if err := backend.ReapDead(ctx, t); err != nil {
			*problems = append(*problems, errors.New("dead process reap failed: "+t.Identifier))
			return beatSkip
		}
		return beatRecover
	default:
		return beatSkip
	}
}

func (r *Runtime) beatPath(t Ticket) (string, error) {
	if r.StateRoot == "" {
		return "", errors.New("state root unknown")
	}
	repo := filepath.Base(t.Repo)
	if _, name, ok := strings.Cut(r.Repositories[t.Repo].Env["AGENT_GH_REPO"], "/"); ok && name != "" {
		repo = name
	}
	unit := "executor"
	if t.Owner == machine.OwnerResolver {
		unit = "resolver"
	}
	if !identifier.MatchString(t.Identifier) || !identifier.MatchString(repo) {
		return "", errors.New("invalid heartbeat identity")
	}
	return orca.HeartbeatPath(r.StateRoot, repo, t.Identifier, unit), nil
}

// Liveness judges the ticket's executor or resolver from its heartbeat file.
// A missing file, or a beat written by another terminal, is no evidence.
func (r *Runtime) Liveness(_ context.Context, t Ticket) (orca.Liveness, error) {
	if t.Terminal == "" || t.Terminal == "-" || r.StateRoot == "" {
		return orca.LivenessNone, nil
	}
	path, err := r.beatPath(t)
	if err != nil {
		return orca.LivenessUncertain, err
	}
	beat, found, err := orca.ReadBeat(path)
	if err != nil {
		return orca.LivenessUncertain, err
	}
	if !found || (beat.Terminal != "" && beat.Terminal != t.Terminal) {
		return orca.LivenessNone, nil
	}
	now := r.clock()
	timeout := r.HeartbeatTimeout
	if timeout <= 0 {
		timeout = orca.DefaultHeartbeatTimeout
	}
	alive := r.PIDAlive
	if alive == nil {
		alive = orca.PIDAlive
	}
	return beat.Judge(now, timeout, alive), nil
}

func (r *Runtime) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// ReapDead abandons every unsettled dispatch of the ticket's Run (fencing
// without claiming the process stopped), closes the leftover terminal and
// removes the stale heartbeat so the relaunched process starts clean. Any
// uncertain read stops the reap before a write.
func (r *Runtime) ReapDead(ctx context.Context, t Ticket) error {
	path, err := r.beatPath(t)
	if err != nil {
		return err
	}
	absent, err := r.TerminalAbsent(ctx, t)
	if err != nil {
		return err
	}
	var active []string
	if t.Run != "" {
		rows, err := r.dispatches(ctx, t.Run)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if !settledDispatch(row.Status) {
				active = append(active, row.ID)
			}
		}
	}
	for _, id := range active {
		if id == "" {
			return errors.New("active dispatch without ID")
		}
		if _, err := r.Orca.WorkerAbandon(ctx, orca.RequestID(t.Identifier, "scheduler", "abandon-"+id, 1), id); err != nil {
			return err
		}
	}
	if !absent {
		if err := r.Orca.TerminalClose(ctx, t.Terminal); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot remove stale heartbeat")
	}
	return nil
}
