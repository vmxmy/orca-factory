package machine

import (
	"reflect"
	"testing"
)

func TestInfraFailureKeepsActualStage(t *testing.T) {
	for _, phase := range []Phase{PhaseUnclaimed, PhaseWorker, PhaseBranchCheck, PhaseGate, PhaseReview, PhaseCorrection, PhaseOpenPR} {
		for _, alive := range []bool{false, true} {
			s := Initial()
			s.Phase, s.ExecAlive, s.Corrections = phase, alive, 2
			for n := 1; n <= 3; n++ {
				next, fx, err := Step(s, Event("infraFailure"), DefaultLimits)
				if err != nil {
					t.Fatalf("phase=%s alive=%v: %v", phase, alive, err)
				}
				if next.Infra != n || next.Corrections != 2 {
					t.Fatalf("state=%+v", next)
				}
				if n < 3 {
					want := s
					want.Infra = n
					if !reflect.DeepEqual(next, want) || len(fx) != 1 {
						t.Fatalf("changed actual stage: before=%+v after=%+v effects=%v", s, next, fx)
					}
				} else if next.Owner != OwnerHuman || next.Phase != PhaseHuman || next.ExecAlive {
					t.Fatalf("not handed off: %+v", next)
				}
				s = next
			}
		}
	}
}
