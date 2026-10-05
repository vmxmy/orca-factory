package executor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

func TestStartMergeCorrectionRepairsExistingPR(t *testing.T) {
	e, o, tools, recovery, l, ticket := recoveryFixture(t)
	tools.gateReds = 1
	first, err := e.Execute(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	g := e.GitHub.(*fakeGitHub)
	g.existing = &first.PR
	g.existing.Head.SHA = "new-sha"
	var h merge.Handoff
	for _, body := range l.comments {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, merge.HandoffPrefix) {
				json.Unmarshal([]byte(strings.TrimPrefix(line, merge.HandoffPrefix)), &h)
			}
		}
	}
	// External receipt is authoritative even when this executor has an old checkpoint.
	raw, _ := json.Marshal(map[string]any{"version": 1, "queue_id": h.QueueID, "ticket_id": h.TicketID, "repository": h.Repository, "pr": h.PR, "head": h.Head, "main": "main-sha", "phase": "correction", "corrections": 2, "catchup_rounds": 1, "infra": 1, "attempt": 2, "reason": "merged-result gate red"})
	l.comments = append(l.comments, "阶段: correction\n归属: 执行器\n"+merge.ProgressPrefix+string(raw))
	tools.head = "repaired-sha"
	g.existing.Head.SHA = "repaired-sha"
	e.Config.Terminal = "new-executor"
	e.Now = func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || recovery.sessions != 2 || recovery.children != 2 || len(o.starts) != 3 {
		t.Fatalf("correction bypassed: result=%+v err=%v sessions=%d children=%d starts=%d", result, err, recovery.sessions, recovery.children, len(o.starts))
	}
	if o.starts[2].Worktree != "path:/original/worktree" || tools.gateCalls != 3 || tools.reviewCalls != 2 || result.State.Corrections != 2 || result.State.Infra != 1 || result.State.Attempt != 2 {
		t.Fatalf("lost repair/budgets: result=%+v starts=%+v gates=%d reviews=%d", result, o.starts, tools.gateCalls, tools.reviewCalls)
	}
	var next merge.Handoff
	for _, body := range l.comments {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, merge.HandoffPrefix) {
				json.Unmarshal([]byte(strings.TrimPrefix(line, merge.HandoffPrefix)), &next)
			}
		}
	}
	if next.Head != "repaired-sha" || next.QueueID == h.QueueID || !next.EnqueuedAt.After(h.EnqueuedAt) || next.CatchupRounds != 1 || next.Corrections != 2 || next.Infra != 1 {
		t.Fatalf("bad requeue: %+v", next)
	}
}

func correctionReceipt(t *testing.T, e *Executor, l *budgetLinear, ticket Ticket, infra int) {
	t.Helper()
	var h merge.Handoff
	for _, body := range l.comments {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, merge.HandoffPrefix) {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, merge.HandoffPrefix)), &h); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	p := merge.Progress{Version: 1, QueueID: h.QueueID, TicketID: h.TicketID, Repository: h.Repository, PR: h.PR, Head: h.Head, Main: "main-sha", Phase: "correction", Corrections: h.Corrections + 1, CatchupRounds: 1, Infra: infra, Attempt: 2, Reason: "text conflict"}
	raw, _ := json.Marshal(p)
	l.comments = append(l.comments, "阶段: 写 correction\n归属: 执行器\n"+merge.ProgressPrefix+string(raw))
	e.GitHub.(*fakeGitHub).existing = &github.PullRequest{Number: h.PR, HTMLURL: "https://example.test/pr/42"}
	e.GitHub.(*fakeGitHub).existing.Head.SHA = "repaired-sha"
	e.Config.Terminal = "new-executor"
}

func TestStartMergeCorrectionRestartDoesNotRepeatRepair(t *testing.T) {
	e, o, tools, recovery, l, ticket := recoveryFixture(t)
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	correctionReceipt(t, e, l, ticket, 1)
	tools.head = "repaired-sha"
	tools.gateErr = context.Canceled
	if _, err := e.Start(context.Background(), ticket); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled validation: %v", err)
	}
	tools.gateErr = nil
	e.Config.Terminal = "another-executor"
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Corrections != 1 || result.State.Infra != 1 || len(o.starts) != 2 || recovery.sessions != 1 || recovery.children != 1 {
		t.Fatalf("repair repeated: result=%+v err=%v starts=%d sessions=%d children=%d", result, err, len(o.starts), recovery.sessions, recovery.children)
	}
}

func TestStartMergeCorrectionFromExternalReceiptWithoutCheckpoint(t *testing.T) {
	e, o, tools, recovery, l, ticket := recoveryFixture(t)
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	correctionReceipt(t, e, l, ticket, 1)
	if err := os.Remove(checkpointPath(e.Config, ticket)); err != nil {
		t.Fatal(err)
	}
	tools.head = "repaired-sha"
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || len(o.starts) != 2 || recovery.sessions != 1 || o.starts[1].Worktree != "path:/original/worktree" {
		t.Fatalf("external recovery failed: %+v %v starts=%v", result, err, o.starts)
	}
}

func TestStartMergeCorrectionRequiresChangedHead(t *testing.T) {
	e, o, _, _, l, ticket := recoveryFixture(t)
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	correctionReceipt(t, e, l, ticket, 1)
	result, err := e.Start(context.Background(), ticket)
	if err == nil || result.State.Owner == machine.OwnerMerge || len(o.starts) != 2 {
		t.Fatalf("unchanged correction handed off: %+v %v", result, err)
	}
}

type mergeHeadRetry struct {
	*recoveryTools
	failed bool
}

func (m *mergeHeadRetry) Head(ctx context.Context, repo, branch string) (string, error) {
	if !m.failed {
		m.failed = true
		return "", errors.New("temporary read failure")
	}
	return m.recoveryTools.Head(ctx, repo, branch)
}
func TestStartMergeCorrectionAddsPriorReadBudget(t *testing.T) {
	e, o, tools, recovery, l, ticket := recoveryFixture(t)
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	correctionReceipt(t, e, l, ticket, 2)
	tools.head = "repaired-sha"
	e.Tools = &mergeHeadRetry{recoveryTools: tools}
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerHuman || result.State.Infra != 3 || len(o.starts) != 1 || recovery.sessions != 0 {
		t.Fatalf("cumulative budget bypassed: %+v err=%v starts=%d sessions=%d", result, err, len(o.starts), recovery.sessions)
	}
}

func TestStartMergeCorrectionRejectsChangedFrozenSpec(t *testing.T) {
	e, o, tools, recovery, l, ticket := recoveryFixture(t)
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	correctionReceipt(t, e, l, ticket, 1)
	tools.head = "repaired-sha"
	for _, body := range append([]string(nil), l.comments...) {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, merge.HandoffPrefix) {
				var h merge.Handoff
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, merge.HandoffPrefix)), &h); err != nil {
					t.Fatal(err)
				}
				h.GateSpec = strings.ReplaceAll(h.GateSpec, "go test ./unit", "go test ./unit && true")
				raw, _ := json.Marshal(h)
				l.comments = append(l.comments, merge.HandoffPrefix+string(raw))
			}
		}
	}
	result, err := e.Start(context.Background(), ticket)
	if err == nil || result.State.Owner == machine.OwnerMerge || len(o.starts) != 1 || recovery.sessions != 0 {
		t.Fatalf("conflicting frozen spec used: %+v %v starts=%d", result, err, len(o.starts))
	}
}

func TestStartMergeCorrectionWithTaskButNoHistoricalDispatch(t *testing.T) {
	e, o, tools, recovery, l, ticket := recoveryFixture(t)
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	for i, body := range l.comments {
		lines := strings.Split(body, "\n")
		for j, line := range lines {
			if strings.HasPrefix(line, merge.HandoffPrefix) {
				var h merge.Handoff
				json.Unmarshal([]byte(strings.TrimPrefix(line, merge.HandoffPrefix)), &h)
				h.Dispatch = ""
				raw, _ := json.Marshal(h)
				lines[j] = merge.HandoffPrefix + string(raw)
			}
		}
		l.comments[i] = strings.Join(lines, "\n")
	}
	correctionReceipt(t, e, l, ticket, 1)
	tools.head = "repaired-sha"
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || len(o.starts) != 2 || recovery.children != 1 || result.DispatchID != "ctx_2" {
		t.Fatalf("historical missing dispatch blocked repair: %+v %v", result, err)
	}
}
