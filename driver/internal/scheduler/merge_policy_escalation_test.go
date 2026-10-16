package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func assertHumanEscalation(t *testing.T, bodies []string) {
	t.Helper()
	for _, b := range bodies {
		if strings.HasPrefix(b, "转人: ") && strings.Contains(b, "\n阶段: 转人\n归属: 人\nmerge-progress-v1: ") {
			return
		}
	}
	t.Fatalf("lane 3 path not escalated directly to a human: %q", bodies)
}

// policyBlockedFixture adds a second ticket (ENG-2, PR#2) that changes one
// extra file on its own branch, ahead of f.tracker (ENG-1) in FIFO order, so
// a merge policy glob can target only the second ticket's diff.
func policyBlockedFixture(t *testing.T, f *mergeFixture, extraFile string) *mergeIssues {
	t.Helper()
	dir := f.repos[0].Path
	gitMergeTest(t, dir, "checkout", "main")
	gitMergeTest(t, dir, "checkout", "-b", "agent/ENG-2/executor")
	if err := os.WriteFile(filepath.Join(dir, extraFile), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	gitMergeTest(t, dir, "add", extraFile)
	gitMergeTest(t, dir, "commit", "-m", "blocked change")
	blockedHead := gitMergeTest(t, dir, "rev-parse", "HEAD")
	gitMergeTest(t, dir, "checkout", "agent/ENG-1/executor")

	blocked := *f.tracker
	blocked.issue.ID = "blocked"
	blocked.issue.Identifier = "ENG-2"
	blocked.rows = append([]task.Comment(nil), f.tracker.rows...)
	bf := *f
	bf.tracker = &blocked
	rewriteHandoff(t, &bf, func(h map[string]any) {
		h["ticket_id"] = "blocked"
		h["identifier"] = "ENG-2"
		h["pr"] = 2
		h["queue_id"] = "q0"
		h["enqueued_at"] = "2026-10-06T00:00:00Z"
		h["head"] = blockedHead
	})
	f.runtime.Clients[f.repos[0].Path] = &mergeTickets{rows: []*mergeIssues{&blocked, f.tracker}}
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := json.Marshal(f.prBody)
		if err != nil {
			t.Error(err)
			return
		}
		var pr map[string]any
		if err := json.Unmarshal(raw, &pr); err != nil {
			t.Error(err)
			return
		}
		pr["number"] = 2
		pr["merged"] = false
		pr["head"].(map[string]any)["ref"] = "agent/ENG-2/executor"
		pr["head"].(map[string]any)["sha"] = blockedHead
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/2"):
			if err := json.NewEncoder(w).Encode(pr); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			rows := []any{pr}
			if !f.merged {
				rows = append(rows, f.prBody)
			}
			if err := json.NewEncoder(w).Encode(rows); err != nil {
				t.Error(err)
			}
		default:
			original.ServeHTTP(w, r)
		}
	})
	return &blocked
}

// TestRuntimeMergeManualGlobReviewLeavesQueue is OF-103's first acceptance
// assertion: a PR that hits AGENT_LANE2_MANUAL_GLOBS transfers to the
// disposition agent for review (same mechanism as OF-102's lane 2 review)
// and leaves the merge queue instead of waiting on it forever.
func TestRuntimeMergeManualGlobReviewLeavesQueue(t *testing.T) {
	f := newMergeFixture(t)
	blocked := policyBlockedFixture(t, f, "manual-change")
	f.repos[0].Env["AGENT_LANE2_MANUAL_GLOBS"] = "manual-change"
	for i := 0; i < 2; i++ {
		_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	}
	assertLane2Review(t, blocked.bodies)
	if f.puts != 1 || f.tracker.issue.State.Name != "Done" || blocked.issue.State.Name == "Done" {
		t.Fatalf("lane 1 blocked behind manual glob review: puts=%d lane1=%s blocked=%s", f.puts, f.tracker.issue.State.Name, blocked.issue.State.Name)
	}
}

// TestRuntimeMergeLane3EscalatesDirectlyToHuman is OF-103's second
// acceptance assertion: a PR that hits AGENT_LANE3_GLOBS transfers straight
// to a human (no resolver review, lane 3 is excluded from automerge
// authorization) and leaves the merge queue.
func TestRuntimeMergeLane3EscalatesDirectlyToHuman(t *testing.T) {
	f := newMergeFixture(t)
	blocked := policyBlockedFixture(t, f, "secret-change")
	f.repos[0].Env["AGENT_LANE3_GLOBS"] = "secret-change"
	for i := 0; i < 2; i++ {
		_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	}
	assertHumanEscalation(t, blocked.bodies)
	if f.puts != 1 || f.tracker.issue.State.Name != "Done" || blocked.issue.State.Name == "Done" {
		t.Fatalf("lane 1 blocked behind lane 3 escalation: puts=%d lane1=%s blocked=%s", f.puts, f.tracker.issue.State.Name, blocked.issue.State.Name)
	}
}
