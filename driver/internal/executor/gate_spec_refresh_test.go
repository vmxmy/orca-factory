package executor

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

const replayOwnership = "阶段: gate\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1"

type refreshTools struct {
	*recoveryTools
	issues          *budgetIssues
	heads           int
	specs           []string
	directiveAtHead int
	directiveInGate bool
}

func (f *refreshTools) Head(ctx context.Context, repo, branch string) (string, error) {
	f.heads++
	if f.heads == f.directiveAtHead {
		f.issues.comments = append(f.issues.comments, "gate-spec-refresh: 负责人已改正 CHECK")
	}
	return f.recoveryTools.Head(ctx, repo, branch)
}
func (f *refreshTools) Gate(ctx context.Context, repo, head, spec string) (string, error) {
	b, err := os.ReadFile(spec)
	if err != nil {
		return "", err
	}
	f.specs = append(f.specs, string(b))
	if f.directiveInGate {
		f.issues.comments = append(f.issues.comments, "gate-spec-refresh: 负责人已改正 CHECK")
	}
	return f.recoveryTools.Gate(ctx, repo, head, spec)
}

func refreshReceipts(comments []string) int {
	n := 0
	for _, c := range comments {
		if strings.HasPrefix(c, "gate-spec-refresh 已执行 attempt=1 term=term_exec") {
			n++
		}
	}
	return n
}

// refreshExit runs an executor whose correction is followed by a refresh
// directive, so it exits before gating the correction with the stale spec.
func refreshExit(t *testing.T) (*Executor, *recoveryOrca, *refreshTools, *budgetIssues, Ticket) {
	t.Helper()
	e, o, base, _, l, ticket := recoveryFixture(t)
	base.gateReds = 1
	tools := &refreshTools{recoveryTools: base, issues: l, directiveAtHead: 2}
	e.Tools = tools
	if _, err := e.Execute(context.Background(), ticket); !errors.Is(err, errGateSpecRefresh) {
		t.Fatalf("refresh did not stop the executor: %v", err)
	}
	if len(tools.specs) != 1 {
		t.Fatalf("stale spec gated after refresh: %d gates", len(tools.specs))
	}
	if refreshReceipts(l.comments) != 1 {
		t.Fatalf("refresh exit left no audit receipt: %q", l.comments)
	}
	e.Config.Terminal = "replacement_terminal"
	ticket.Check = "go test ./unit -run Refreshed"
	return e, o, tools, l, ticket
}

// The replacement rewrites gate.spec from the current ticket face and gates
// the same head without charging a correction or dispatching a worker.
func TestGateSpecRefreshReplaysGateWithoutCorrection(t *testing.T) {
	e, o, tools, l, ticket := refreshExit(t)
	dispatches := len(o.starts)
	l.comments = append(l.comments, replayOwnership)
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Corrections != 1 {
		t.Fatalf("replay failed: %+v err=%v", result, err)
	}
	if len(tools.specs) != 2 || !strings.Contains(tools.specs[1], "CHECK: go test ./unit -run Refreshed") {
		t.Fatalf("replacement gated a stale spec: %q", tools.specs)
	}
	if len(o.starts) != dispatches {
		t.Fatalf("replay dispatched a worker: %d -> %d", dispatches, len(o.starts))
	}
}

// A second directive in an attempt that already refreshed is ignored, so the
// directive cannot restart the stage without bound.
func TestGateSpecRefreshOncePerAttempt(t *testing.T) {
	e, _, tools, l, ticket := refreshExit(t)
	l.comments = append(l.comments, replayOwnership, "gate-spec-refresh: 再次刷新")
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("second directive stopped the attempt: %+v err=%v", result, err)
	}
	if len(tools.specs) != 2 || refreshReceipts(l.comments) != 1 {
		t.Fatalf("gates=%d receipts=%d", len(tools.specs), refreshReceipts(l.comments))
	}
}

// A directive written after the last gate cannot run; the owner learns that
// the handoff froze the old spec instead of losing the directive silently.
func TestGateSpecRefreshNoticeBeforeHandoff(t *testing.T) {
	e, _, base, _, l, ticket := recoveryFixture(t)
	tools := &refreshTools{recoveryTools: base, issues: l, directiveInGate: true}
	e.Tools = tools
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("handoff failed: %+v err=%v", result, err)
	}
	notice := slices.Index(l.comments, "gate-spec-refresh 未执行：已进入交接，按旧 spec 冻结")
	opened := slices.IndexFunc(l.comments, func(c string) bool { return strings.HasPrefix(c, "PR 已开: ") })
	if notice < 0 || opened < notice {
		t.Fatalf("no notice before handoff: %q", l.comments)
	}
}
