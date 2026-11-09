package executor

import (
	"context"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

const workerIdleNudge = "请按任务继续；完成时用 worker_done 回报"

// workerIdle belongs to one dispatch and survives output recovery and read retries.
type workerIdle struct {
	lastOutput          int64
	observed, nudged    bool
	nudgedAt, stalledAt time.Time
}

// observe returns true each time the terminal has stayed silent for a full
// timeout since its last output, the nudge, or the previous counted stall.
func (i *workerIdle) observe(ctx context.Context, e *Executor, worktree string, timeout time.Duration, state machine.State) bool {
	for _, paused := range state.Paused {
		if paused {
			return false
		}
	}
	if worktree == "" {
		return false
	}
	terminals, err := e.Orca.TerminalList(ctx)
	if err != nil {
		return false
	}
	terminal, found := orca.TerminalForWorktree(terminals, worktree)
	if !found {
		return false
	}
	if !i.observed || terminal.LastOutputAt > i.lastOutput {
		i.lastOutput, i.observed = terminal.LastOutputAt, true
	}
	since := time.UnixMilli(i.lastOutput)
	for _, mark := range []time.Time{i.nudgedAt, i.stalledAt} {
		if mark.After(since) {
			since = mark
		}
	}
	if e.Now().Sub(since) <= timeout {
		return false
	}
	if !i.nudged {
		// Even an uncertain send consumes the only nudge; never replay it. Only a
		// nudge that observably started a turn earns a fresh grace interval.
		i.nudged = true
		receipt, err := e.Orca.TerminalSend(ctx, terminal.Handle, workerIdleNudge)
		if err == nil && orca.TurnStarted(receipt) {
			i.nudgedAt = e.Now()
			return false
		}
	}
	i.stalledAt = e.Now()
	return true
}

// lastOutputText is the evidence form of the latest observed terminal output.
func (i *workerIdle) lastOutputText() string {
	if !i.observed {
		return "未知"
	}
	return time.UnixMilli(i.lastOutput).UTC().Format(time.RFC3339)
}
