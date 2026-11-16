package issues_test

import (
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/executor"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
)

func TestFeishuReadRenderedBriefParses(t *testing.T) {
	brief := issues.FeishuBrief{Todo: "做 A\n第二步", Check: "cd driver && go test ./... -run 'TestX'", Scope: "driver/internal/issues\ndriver/internal/feishu", Body: "## 现状\n说明\n\nCHECK_IS_NEW_TEST: yes\nCHECK_TEST_FILES: test/probe.sh test/assertion.json\nCHECK_ASSERTION: test/assertion.json", Blockers: []string{"b"}, Related: []string{}}
	ticket, err := executor.ParseTicket("id", "OF-1", "标题", brief.Description())
	if err != nil {
		t.Fatal(err)
	}
	if !ticket.NewTest || ticket.Assertion != "test/assertion.json" || ticket.TestFiles != "test/probe.sh test/assertion.json" {
		t.Fatalf("lost assertion evidence: %+v", ticket)
	}
	if ticket.Todo != "做 A\n第二步" || ticket.Check != brief.Check || ticket.Scope != "driver/internal/issues driver/internal/feishu" {
		t.Fatalf("parsed todo=%q check=%q scope=%q", ticket.Todo, ticket.Check, ticket.Scope)
	}
}
