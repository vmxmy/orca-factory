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
		{"scope blocked", []Event{EvClaim, EvWorkerScopeBlocked}, "Agent处置", "处置Agent", 1, 0, 0, 0},
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

// Each driver phase owns one Orca board column; the historical 转人 receipt
// shares 等人批 because new tickets never enter it.
func TestBoardColumnPerPhase(t *testing.T) {
	want := map[Phase]string{
		PhaseUnclaimed:   "driver-unclaimed",
		PhaseWorker:      "driver-worker",
		PhaseBranchCheck: "driver-branch-check",
		PhaseGate:        "driver-gate",
		PhaseReview:      "driver-review",
		PhaseCorrection:  "driver-correction",
		PhaseOpenPR:      "driver-open-pr",
		PhaseMergeQueue:  "driver-merge-queue",
		PhaseResolver:    "driver-resolver",
		PhaseAwaitHuman:  "driver-await-human",
		PhaseHuman:       "driver-await-human",
		PhaseMerged:      "completed",
	}
	for phase, column := range want {
		if got := BoardColumn(phase); got != column {
			t.Errorf("BoardColumn(%q) = %q, want %q", phase, got, column)
		}
	}
}

// A stall verdict needs two charged failures to compare; an earlier verdict
// is an unusable coordinator session, not a handoff.
func TestCorrectionStalledNeedsTwoFailures(t *testing.T) {
	run := func(events ...Event) State {
		t.Helper()
		state := Initial()
		for _, ev := range events {
			next, _, err := Step(state, ev, DefaultLimits)
			if err != nil {
				t.Fatalf("%s: %v", ev, err)
			}
			state = next
		}
		return state
	}
	early := run(EvClaim, EvWorkerFail, EvCorrectionStalled)
	if early.Phase != PhaseCorrection || early.Owner != OwnerExecutor || early.Corrections != 1 || early.Infra != 1 {
		t.Fatalf("early verdict: %+v", early)
	}
	late := run(EvClaim, EvWorkerFail, EvSessionOK, EvWorkerFail, EvCorrectionStalled)
	if late.Phase != PhaseResolver || late.Owner != OwnerResolver || late.Corrections != 2 || late.Infra != 0 || late.HumanReason != "correction stalled: no progress across failures" {
		t.Fatalf("late verdict: %+v", late)
	}
}

// An overrun hands a live worker to the resolver without charging either
// budget, and holds while a dependency is paused.
func TestWorkerWatchdogOverrunEvent(t *testing.T) {
	claimed, _, err := Step(Initial(), EvClaim, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := Step(claimed, EvWorkerOverrun, DefaultLimits)
	if err != nil || next.Phase != PhaseResolver || next.Owner != OwnerResolver || next.ExecAlive || next.Infra != 0 || next.Corrections != 0 || next.HumanReason != "worker overran: no worker_done within run limit" {
		t.Fatalf("overrun: %+v err=%v", next, err)
	}
	paused := copyState(claimed)
	paused.Paused[DepClaude] = true
	held, fx, err := Step(paused, EvWorkerOverrun, DefaultLimits)
	if err != nil || held.Phase != PhaseWorker || held.Owner != OwnerExecutor || len(fx) != 0 {
		t.Fatalf("paused overrun: %+v fx=%v err=%v", held, fx, err)
	}
	reported, _, _ := Step(claimed, EvWorkerOK, DefaultLimits)
	crashed, _, _ := Step(claimed, EvExecCrash, DefaultLimits)
	for name, state := range map[string]State{"unclaimed": Initial(), "branch check": reported, "executor down": crashed} {
		var rejected Rejected
		if _, _, err := Step(state, EvWorkerOverrun, DefaultLimits); !errors.As(err, &rejected) {
			t.Fatalf("%s accepted overrun: err=%v", name, err)
		}
	}
}
