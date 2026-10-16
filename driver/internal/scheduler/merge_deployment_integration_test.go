package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type moveAfterIntent struct {
	*mergeIssues
	move  func()
	moved bool
}

func (l *moveAfterIntent) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	c, err := l.mergeIssues.CreateComment(ctx, id, body)
	if err == nil && !l.moved && strings.Contains(body, `"phase":"merge-intent"`) {
		l.moved = true
		l.move()
	}
	return c, err
}
func TestMergeCatchupPreservesDeploymentAndBudgets(t *testing.T) {
	f := newMergeFixture(t)
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "integration-smoke"
	rewriteHandoff(t, f, func(h map[string]any) { h["corrections"] = 2; h["infra"] = 1; h["attempt"] = 2 })
	dir := f.repos[0].Path
	l := &moveAfterIntent{mergeIssues: f.tracker, move: func() {
		gitMergeTest(t, dir, "checkout", "main")
		gitMergeTest(t, dir, "commit", "--allow-empty", "-m", "main moved after intent")
		f.main = gitMergeTest(t, dir, "rev-parse", "HEAD")
	}}
	f.runtime.Clients[dir] = l
	smokes := 0
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "bash" && s.Args[0] == "-c" {
			smokes++
			return nil, nil
		}
		return runx.Run(ctx, s)
	}
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background()); err == nil {
		t.Fatal("moving main must wait")
	}
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var p merge.Progress
	for _, row := range f.tracker.rows {
		for _, line := range strings.Split(row.Body, "\n") {
			if strings.HasPrefix(line, merge.ProgressPrefix) {
				p = merge.Progress{}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, merge.ProgressPrefix)), &p); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if smokes != 1 || f.puts != 1 || p.Deployment == nil || p.Deployment.State != "passed" || p.CatchupRounds != 1 || p.Corrections != 2 || p.Infra != 1 || p.Attempt != 2 {
		t.Fatalf("integration lost receipt: smokes=%d puts=%d %+v", smokes, f.puts, p)
	}
}

func TestCorrectionDeploymentIntentDoesNotOwnExecutor(t *testing.T) {
	f := newMergeFixture(t)
	var h merge.Handoff
	if err := json.Unmarshal([]byte(strings.Split(f.tracker.rows[0].Body, merge.HandoffPrefix)[1]), &h); err != nil {
		t.Fatal(err)
	}
	p := merge.Progress{Version: 1, QueueID: h.QueueID, TicketID: h.TicketID, Repository: h.Repository, PR: h.PR, Head: h.Head, Main: f.main, Phase: "correction", Corrections: 1, Deployment: &merge.DeploymentProgress{State: "pending", Smoke: "test-smoke"}}
	raw, rawMarshalErr := json.Marshal(p)
	if rawMarshalErr != nil {
		t.Fatal(rawMarshalErr)
	}
	f.tracker.rows = append(f.tracker.rows, task.Comment{CreatedAt: time.Now().Add(time.Second), Body: "归属: 执行器 term=-\n" + merge.ProgressPrefix + string(raw)})
	rows, err := f.runtime.Snapshot(context.Background(), f.repos)
	if err != nil || len(rows) != 1 || rows[0].MergeProblem != "" || DeploymentBarrier(rows[0]) {
		t.Fatalf("unmerged correction mistaken for deployment: %+v %v", rows, err)
	}
	h.QueueID = "repaired-head"
	h.Head = strings.Repeat("b", 40)
	h.Corrections = 1
	raw, rawMarshalErr = json.Marshal(h)
	if rawMarshalErr != nil {
		t.Fatal(rawMarshalErr)
	}
	f.tracker.rows = append(f.tracker.rows, task.Comment{CreatedAt: time.Now().Add(2 * time.Second), Body: "归属: 合并通道\n" + merge.HandoffPrefix + string(raw)})
	rows, err = f.runtime.Snapshot(context.Background(), f.repos)
	if err != nil || len(rows) != 1 || rows[0].MergeProblem != "" || DeploymentBarrier(rows[0]) {
		t.Fatalf("old unmerged intent blocks repaired head: %+v %v", rows, err)
	}
}

func TestDeploymentConfigReservedAtFirstIntentAfterWaiting(t *testing.T) {
	f := newMergeFixture(t)
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "integration-smoke"
	f.checkState = "pending"
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background()); err == nil || f.puts != 0 {
		t.Fatalf("expected pre-intent wait: %v", err)
	}
	smokes := 0
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "bash" {
			smokes++
			return nil, nil
		}
		return runx.Run(ctx, s)
	}
	f.checkState = "success"
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if smokes != 1 || f.puts != 1 {
		t.Fatalf("waiting without intent lost deployment: smoke=%d merges=%d", smokes, f.puts)
	}
}
