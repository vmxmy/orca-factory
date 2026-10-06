package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

func (e *Executor) ledgerPath(t Ticket) string {
	return filepath.Join(e.Config.Root, e.Config.Repository, t.Identifier, "ledger-run.json")
}

func (e *Executor) initLedger(t Ticket, resume bool) {
	if e.LedgerWriter == nil || e.runLedger != nil {
		return
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Config.Root == "" {
		root, err := executorStateRoot()
		if err != nil {
			e.warn("ledger: state root unavailable")
			return
		}
		e.Config.Root = root
	}
	if resume {
		old, err := merge.ReadLedgerRecord(e.ledgerPath(t))
		if err == nil {
			e.runLedger = old
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			e.warn("ledger: recovery record unavailable")
			return
		}
	}
	now := e.Now().UTC()
	key := sha256.Sum256([]byte(t.Identifier + "\x00" + now.Format(time.RFC3339Nano)))
	e.runLedger = &merge.LedgerRecord{Repository: e.Config.Owner + "/" + e.Config.Repository, Team: e.Config.LedgerTeam, Project: e.Config.LedgerProject, Stage: "queue", StageAt: now,
		CompletionPath: filepath.Join(e.Config.Root, e.Config.Repository, t.Identifier, "ledger-finished", hex.EncodeToString(key[:])+".json"),
		Row:            merge.LedgerRow{Ticket: t.Identifier, Lane: t.Lane, Tier: e.Config.Tier, StartedAt: now, StageSecs: map[string]float64{"queue": 0, "worker": 0, "gate": 0, "review": 0, "merge": 0}}}
	e.saveLedger(t)
}

func ledgerStage(phase machine.Phase) string {
	switch phase {
	case machine.PhaseWorker, machine.PhaseCorrection:
		return "worker"
	case machine.PhaseBranchCheck, machine.PhaseGate:
		return "gate"
	case machine.PhaseReview, machine.PhaseOpenPR:
		return "review"
	case machine.PhaseMergeQueue:
		return "merge"
	default:
		return "queue"
	}
}

func (e *Executor) ledgerTransition(t Ticket, next machine.State, event machine.Event) {
	if e.runLedger == nil {
		return
	}
	r := e.runLedger
	now := e.Now().UTC()
	r.Row.StageSecs[r.Stage] += max(0, now.Sub(r.StageAt).Seconds())
	r.Stage, r.StageAt = ledgerStage(next.Phase), now
	r.Row.Corrections = next.Corrections
	if event == machine.EvGateGreen || event == machine.EvGateRed {
		if r.Row.FirstGate == "" {
			r.Row.FirstGate = "green"
			if event == machine.EvGateRed {
				r.Row.FirstGate = "red"
			}
		}
	}
	e.saveLedger(t)
}

func (e *Executor) saveLedger(t Ticket) {
	if e.runLedger != nil {
		if err := merge.SaveLedgerRecord(e.ledgerPath(t), *e.runLedger); err != nil {
			e.warn("ledger: cannot save claim attribution")
		}
	}
}

func (e *Executor) freezeLedger(t Ticket, result Result, worktree string) {
	if e.runLedger == nil {
		return
	}
	r := e.runLedger
	r.Row.PR = result.PR.HTMLURL
	r.Row.Corrections = result.State.Corrections
	if worktree != "" {
		r.Worktrees = []string{worktree}
	}
	e.saveLedger(t)
	if err := merge.SaveLedgerRecord(merge.MergeLedgerPath(e.Config.Root, r.Repository, result.PR.Number), *r); err != nil {
		e.warn("ledger: cannot save merge attribution")
	}
}

func (e *Executor) writeHandoffLedger(ctx context.Context, t Ticket, state machine.State) {
	if e.LedgerWriter == nil {
		return
	}
	e.initLedger(t, true)
	if e.runLedger == nil {
		return
	}
	e.runLedger.Row.Corrections = state.Corrections
	e.saveLedger(t)
	if err := merge.FinishLedger(ctx, e.ledgerPath(t), e.LedgerWriter, "escalated"); err != nil {
		e.warn("ledger: handoff row failed")
	}
}
