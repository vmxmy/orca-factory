package executor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

type refreshTools struct {
	*recoveryTools
	issues *budgetIssues
	heads  int
	specs  []string
}

func (f *refreshTools) Head(ctx context.Context, repo, branch string) (string, error) {
	f.heads++
	if f.heads == 2 {
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
	return f.recoveryTools.Gate(ctx, repo, head, spec)
}

// A refresh written after a correction stops the executor before it gates the
// correction with the stale spec; the replacement rewrites gate.spec from the
// current ticket face and gates without charging another correction.
func TestGateSpecRefreshReplaysGateWithoutCorrection(t *testing.T) {
	e, _, base, _, l, ticket := recoveryFixture(t)
	base.gateReds = 1
	tools := &refreshTools{recoveryTools: base, issues: l}
	e.Tools = tools
	if _, err := e.Execute(context.Background(), ticket); !errors.Is(err, errGateSpecRefresh) {
		t.Fatalf("refresh did not stop the executor: %v", err)
	}
	if len(tools.specs) != 1 {
		t.Fatalf("stale spec gated after refresh: %d gates", len(tools.specs))
	}
	l.comments = append(l.comments, "阶段: gate\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1")
	e.Config.Terminal = "replacement_terminal"
	ticket.Check = "go test ./unit -run Refreshed"
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Corrections != 1 {
		t.Fatalf("replay failed: %+v err=%v", result, err)
	}
	if len(tools.specs) != 2 || !strings.Contains(tools.specs[1], "CHECK: go test ./unit -run Refreshed") {
		t.Fatalf("replacement gated a stale spec: %q", tools.specs)
	}
}
