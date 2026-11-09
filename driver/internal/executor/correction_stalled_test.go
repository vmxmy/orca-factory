package executor

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

func TestCorrectionStalledHandsOffToResolver(t *testing.T) {
	e, o, tools, s, l, ticket := recoveryFixture(t)
	tools.gateReds = 5
	s.stalledFrom = 2
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver || result.State.Corrections != 2 || result.State.Infra != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if s.sessions != 2 || s.children != 1 || len(o.starts) != 2 {
		t.Fatalf("stalled correction dispatched: sessions=%d children=%d starts=%d", s.sessions, s.children, len(o.starts))
	}
	all := strings.Join(l.comments, "\n")
	if !strings.Contains(all, "Agent处置: correction stalled: no progress across failures") || !strings.Contains(all, "停滞判断: 同一断言连续失败，步骤无新进展") {
		t.Fatalf("handoff evidence: %q", all)
	}
	if strings.Contains(all, "\a") || strings.Contains(all, "第二行") {
		t.Fatalf("stall reason not reduced to one clean line: %q", all)
	}
}

func TestCorrectionStalledBeforeSecondFailureIsEmpty(t *testing.T) {
	e, o, tools, s, _, ticket := recoveryFixture(t)
	tools.gateReds = 1
	s.stalledFrom = 1
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver || result.State.Infra != 3 || result.State.Corrections != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if s.sessions != 3 || s.children != 0 || len(o.starts) != 1 {
		t.Fatalf("early verdict dispatched: sessions=%d children=%d starts=%d", s.sessions, s.children, len(o.starts))
	}
}

func TestCorrectionStalledRecordedVerdict(t *testing.T) {
	reason, stalled := stallVerdict(recorded(t, "correction-stalled.md"))
	if !stalled || !strings.HasPrefix(reason, "两次 gate red 均停在 STEP check") || utf8.RuneCountInString(reason) != 200 {
		t.Fatalf("stalled=%v reason=%q", stalled, reason)
	}
}
