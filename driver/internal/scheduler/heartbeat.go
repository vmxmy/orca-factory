package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
// A missing file, or a beat written by another terminal, is no evidence. A
// claim not yet bound to a terminal has no handle to compare, so its beat counts.
func (r *Runtime) Liveness(_ context.Context, t Ticket) (orca.Liveness, error) {
	if r.StateRoot == "" {
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
	if !found {
		return orca.LivenessNone, nil
	}
	return r.judge(t, beat), nil
}

func (r *Runtime) judge(t Ticket, beat orca.Beat) orca.Liveness {
	if registered(t) && beat.Terminal != "" && beat.Terminal != t.Terminal {
		return orca.LivenessNone
	}
	timeout := r.HeartbeatTimeout
	if timeout <= 0 {
		timeout = orca.DefaultHeartbeatTimeout
	}
	alive := r.PIDAlive
	if alive == nil {
		alive = orca.PIDAlive
	}
	return beat.Judge(r.clock(), timeout, alive)
}

func (r *Runtime) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func registered(t Ticket) bool { return t.Terminal != "" && t.Terminal != "-" }

// ReapDead fences unsettled dispatches of the Run the dead process last beat
// and of the recorded Run: a claim still at run=-, or a resume that opened a
// new Run, leaves the process's live workers only in the beat. Abandon fences
// without claiming the process stopped. It closes the leftover terminal and
// every worker terminal the abandon receipts report Orca kept open, and
// removes the stale heartbeat so the relaunched process starts clean. An
// already abandoned worker (dispatchStatus failed, workerState abandoned in
// orca-worker-list-abandoned.json) is fenced again: a reap that failed after
// its abandon learns the retained terminals only from the repeat receipt
// (orca-worker-abandon-settled.json). A resolver owns t.Resolution.Run; t.Run is
// the executor Run it was handed, and a resolver killed before its first
// Resolution save is known only by its beat. The beat is judged again, so one a
// relaunched process replaced is never reaped. Any uncertain read stops the
// reap before a write.
func (r *Runtime) ReapDead(ctx context.Context, t Ticket) error {
	path, err := r.beatPath(t)
	if err != nil {
		return err
	}
	beat, found, err := orca.ReadBeat(path)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("heartbeat vanished before reap")
	}
	if r.judge(t, beat) != orca.LivenessDead {
		return errors.New("heartbeat no longer dead")
	}
	recorded := t.Run
	if t.Owner == machine.OwnerResolver {
		recorded = ""
		if t.Resolution != nil {
			recorded = t.Resolution.Run
		}
	}
	absent := true
	if registered(t) {
		if absent, err = r.TerminalAbsent(ctx, t); err != nil {
			return err
		}
	}
	var runs, active []string
	for _, run := range []string{beat.Run, recorded} {
		if run != "" && !slices.Contains(runs, run) {
			runs = append(runs, run)
		}
	}
	for _, run := range runs {
		rows, err := r.dispatches(ctx, run)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if (!settledDispatch(row.Status) || row.WorkerState == "abandoned") && !slices.Contains(active, row.ID) {
				active = append(active, row.ID)
			}
		}
	}
	var leftover []string
	if !absent {
		leftover = append(leftover, t.Terminal)
	}
	for _, id := range active {
		if id == "" {
			return errors.New("active dispatch without ID")
		}
		receipt, err := r.Orca.WorkerAbandon(ctx, orca.RequestID(t.Identifier, "scheduler", "abandon-"+id, 1), id)
		if err != nil {
			return err
		}
		residual, err := abandonedTerminals(receipt)
		if err != nil {
			return err
		}
		for _, terminal := range residual {
			if !slices.Contains(leftover, terminal) {
				leftover = append(leftover, terminal)
			}
		}
	}
	for _, terminal := range leftover {
		// A handle Orca no longer knows is already closed.
		var rejected *orca.RejectedError
		if err := r.Orca.TerminalClose(ctx, terminal); err != nil && (!errors.As(err, &rejected) || rejected.Code != "terminal_handle_stale") {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot remove stale heartbeat")
	}
	return nil
}

// abandonedTerminals confirms an abandon receipt and returns the terminals
// Orca retained (orca-worker-abandon.json: processAction none).
func abandonedTerminals(receipt orca.Receipt) ([]string, error) {
	var state string
	if json.Unmarshal(receipt["state"], &state) != nil || state != "abandoned" {
		return nil, errors.New("worker abandon not confirmed")
	}
	var residual []struct{ Kind, ID string }
	if raw, ok := receipt["residualResources"]; ok && json.Unmarshal(raw, &residual) != nil {
		return nil, errors.New("invalid abandon residual resources")
	}
	var terminals []string
	for _, resource := range residual {
		if resource.Kind == "terminal" && resource.ID != "" {
			terminals = append(terminals, resource.ID)
		}
	}
	return terminals, nil
}
