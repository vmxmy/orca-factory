package machine

import "testing"

func TestEscalationTransfersToResolver(t *testing.T) {
	cases := []struct {
		name  string
		state State
		event Event
	}{
		{"unchanged branch", State{Phase: PhaseBranchCheck, Owner: OwnerExecutor, ExecAlive: true}, EvBranchNo},
		{"correction exhausted", State{Phase: PhaseGate, Owner: OwnerExecutor, ExecAlive: true, Corrections: 3}, EvGateRed},
		{"infra exhausted", State{Phase: PhaseReview, Owner: OwnerExecutor, ExecAlive: true, Infra: 2}, EvReviewEmpty},
		{"executor failed twice", State{Phase: PhaseWorker, Owner: OwnerExecutor, ExecFailures: 1}, EvTick},
		{"approval required", State{Phase: PhaseOpenPR, Owner: OwnerExecutor, ExecAlive: true, ManualGlob: true}, EvOpenPR},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := Step(tt.state, tt.event, DefaultLimits)
			if err != nil {
				t.Fatal(err)
			}
			if got.Owner != Owner("处置Agent") || got.Phase != Phase("Agent处置") || got.ExecAlive {
				t.Fatalf("handoff must be scheduled for resolver, got %+v", got)
			}
		})
	}
}
