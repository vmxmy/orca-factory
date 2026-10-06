package machine

import (
	"errors"
	"reflect"
	"testing"
)

func TestScenarios(t *testing.T) {
	// Sequences and UI toggles come from prototype 186bcd9 demo.html.
	tests := []struct {
		name                                      string
		events                                    []Event
		phase                                     Phase
		owner                                     Owner
		attempt, corrections, infra, execFailures int
	}{
		{"happy", []Event{EvClaim, EvWorkerOK, EvBranchOK, EvGateGreen, EvReviewGreen, EvOpenPR, EvMerge}, "已合并", "无", 1, 0, 0, 0},
		{"z820", []Event{EvClaim, EvWorkerOK, EvBranchOK, EvGateGreen, EvReviewRed, EvSessionEmpty, EvSessionOK, EvWorkerOK, EvBranchOK, EvGateGreen, EvReviewGreen, EvOpenPR, EvMerge}, "已合并", "无", 1, 1, 1, 0},
		{"z803", []Event{EvClaim, EvExecCrash, EvWorkerOK, EvTick, EvBranchOK, EvGateGreen, EvReviewGreen, EvOpenPR, EvMerge}, "已合并", "无", 2, 0, 0, 1},
		{"unsure", []Event{EvClaim, EvReadFail, EvTick, EvTick, EvWorkerOK}, "分支检查", "执行器", 1, 0, 0, 0},
		{"claude", []Event{EvClaim, EvWorkerOK, EvBranchOK, EvGateGreen, "pauseClaude", EvReviewGreen, "pauseClaude", EvReviewGreen, EvOpenPR, EvMerge}, "已合并", "无", 1, 0, 0, 0},
		{"budget", []Event{EvClaim, EvWorkerOK, EvBranchOK, EvGateGreen, EvReviewEmpty, EvReviewEmpty, EvReviewEmpty}, "Agent处置", "处置Agent", 1, 0, 3, 0},
		{"fake", []Event{"toggleManual", EvClaim, EvWorkerOK, EvBranchOK, EvGateGreen, EvReviewGreen, EvOpenPR, EvTick}, "Agent处置", "处置Agent", 1, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := Initial()
			for i, event := range tt.events {
				// The Go prototype exposes UI configuration through State, not events.
				switch event {
				case "pauseClaude":
					state.Paused[DepClaude] = !state.Paused[DepClaude]
					continue
				case "toggleManual":
					state.ManualGlob = !state.ManualGlob
					continue
				}
				before := state
				next, effects, err := Step(state, event, DefaultLimits)
				if tt.name == "claude" && i == 5 {
					assertRejected(t, before, next, effects, err)
				} else if err != nil {
					t.Fatalf("step %d (%s): %v", i, event, err)
				}
				state = next
			}
			if state.Phase != tt.phase || state.Owner != tt.owner || state.Attempt != tt.attempt || state.Corrections != tt.corrections || state.Infra != tt.infra || state.ExecFailures != tt.execFailures {
				t.Fatalf("got phase=%q owner=%q attempt=%d corrections=%d infra=%d execFailures=%d; want %q %q %d %d %d %d", state.Phase, state.Owner, state.Attempt, state.Corrections, state.Infra, state.ExecFailures, tt.phase, tt.owner, tt.attempt, tt.corrections, tt.infra, tt.execFailures)
			}
		})
	}
	// Keep rejection coverage inside CHECK's TestScenarios selection.
	for _, event := range []Event{EvMerge, EvWorkerOK, Event("unknown")} {
		before := Initial()
		next, effects, err := Step(before, event, DefaultLimits)
		assertRejected(t, before, next, effects, err)
	}
}

func assertRejected(t *testing.T, before, after State, effects []Effect, err error) {
	t.Helper()
	var rejected Rejected
	if !errors.As(err, &rejected) {
		t.Fatalf("got error %v, want Rejected", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected event changed state: before=%+v after=%+v", before, after)
	}
	if len(effects) != 0 {
		t.Fatalf("rejected event emitted effects: %v", effects)
	}
}
