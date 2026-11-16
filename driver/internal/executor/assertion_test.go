package executor

import (
	"context"
	"strings"
	"testing"
)

func TestLegacyNewTestExplainsAssertionMigration(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, strings.Repeat("b", 40))
	ticket.NewTest, ticket.TestFiles = true, "test/probe.sh"
	_, err := e.Execute(context.Background(), ticket)
	if err == nil || !strings.Contains(err.Error(), "CHECK_ASSERTION") || !strings.Contains(err.Error(), "gate-spec-refresh") {
		t.Fatalf("legacy new-test diagnostic = %v", err)
	}
}

func TestGateSpecRefreshCarriesAssertion(t *testing.T) {
	e, o, tools, l, ticket := refreshExit(t)
	dispatches := len(o.starts)
	ticket.NewTest, ticket.TestFiles, ticket.Assertion = true, "test/probe.sh test/assertion.json", "test/assertion.json"
	l.comments = append(l.comments, replayOwnership)
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Attempt != 1 || result.State.Corrections != 1 || len(o.starts) != dispatches {
		t.Fatalf("refresh changed attempt or dispatch: %+v err=%v", result, err)
	}
	if len(tools.specs) != 2 || !strings.Contains(tools.specs[1], "CHECK_ASSERTION: test/assertion.json\n") {
		t.Fatalf("refreshed assertion lost: %q", tools.specs)
	}
}
