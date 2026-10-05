package orca

import (
	"context"
	"testing"
)

func TestRecordedWorkerListLiveness(t *testing.T) {
	receipt, err := recordedClient(t, "orca-worker-list-liveness.json").WorkerList(context.Background(), ListOptions{Run: "run_9714ea9b8576", IncludeRemote: true})
	if err != nil {
		t.Fatal(err)
	}
	page, err := ParseWorkerList(receipt)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, w := range page.Workers {
		verdict := w.Projection.Liveness.Verdict
		counts[verdict]++
		stage, status, path, reason := "failed", "failed", "/home/laoxu/orca/workspaces/orca-factory/ZII-886-executor", ""
		if verdict == "unverifiable" {
			stage, status, path, reason = "succeeded", "completed", path+"-2", "stale_status"
		}
		if w.DispatchID == "" || w.AgentTerminalHandle == "" || w.DispatchStatus != status || w.Projection.Stage.Worker != stage || w.Projection.Stage.Detail != "settled" || w.Projection.Liveness.Reason != reason || w.WorktreePath() != path {
			t.Fatalf("worker=%+v path=%q", w, w.WorktreePath())
		}
	}
	if counts["exited"] != 3 || counts["unverifiable"] != 1 {
		t.Fatalf("verdict counts=%v", counts)
	}
}
