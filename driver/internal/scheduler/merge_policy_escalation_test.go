package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestLane2GlobApprovalDoesNotLoop(t *testing.T) {
	f := newMergeFixture(t)
	f.runtime.Clients[f.repos[0].Path] = resolverMergeIssues{f.tracker}
	closeMergeGates(t, f.runtime)
	f.tracker.labels = true
	f.repos[0].Env["AGENT_LANE2_GLOBS"] = "feature"
	rewriteHandoff(t, f, func(h map[string]any) {
		h["gate_spec"] = strings.ReplaceAll(h["gate_spec"].(string), "LANE: 1", "LANE: 2")
	})
	var frozen Ticket
	readMergeHandoff(&frozen, f.tracker.rows)
	handoff, err := json.Marshal(frozen.Handoff)
	if err != nil {
		t.Fatal(err)
	}
	f.tracker.rows[0].Body = "阶段: 排队合并\n归属: 合并通道\nmerge-handoff-v1: " + string(handoff)
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertLane2Review(t, f.tracker.bodies)
	if f.puts != 0 {
		t.Fatalf("unapproved handoff merged: puts=%d", f.puts)
	}
	proposal, err := json.Marshal(resolver.Resolution{Action: "merge", Reason: "lane 2 diff reviewed", Handoff: frozen.Handoff})
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "result", Outcome: "success", Result: string(proposal)})
	if err != nil {
		t.Fatal(err)
	}
	f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "result", CreatedAt: time.Now(), Body: "阶段: Agent处置\n归属: 处置Agent term=controller\n" + resolver.Prefix + string(record)})
	for range 3 {
		if err := tickPastMergeGate(t, s, f.runtime, f); err != nil {
			t.Fatal(err)
		}
		if f.tracker.issue.State.Name == "Done" {
			break
		}
	}
	if f.puts != 1 || f.tracker.issue.State.Name != "Done" {
		t.Fatalf("approved lane 1 handoff hit lane 2 review again: puts=%d state=%s bodies=%q", f.puts, f.tracker.issue.State.Name, f.tracker.bodies)
	}
	var current Ticket
	readMergeHandoff(&current, f.tracker.rows)
	if current.MergeProblem != "" || !reflect.DeepEqual(current.Handoff, frozen.Handoff) {
		t.Fatalf("approval changed frozen handoff: before=%+v after=%+v problem=%s", frozen.Handoff, current.Handoff, current.MergeProblem)
	}
	reviews := 0
	for _, body := range f.tracker.bodies {
		if strings.HasPrefix(body, "Agent处置: lane 2 复核") {
			reviews++
		}
	}
	if reviews != 1 {
		t.Fatalf("review count=%d, want 1", reviews)
	}
}

func TestMergeReviewCommentFailureRecovers(t *testing.T) {
	f := newMergeFixture(t)
	f.tracker.labels = true
	f.tracker.failBody = "Agent处置: lane 2 复核"
	f.repos[0].Env["AGENT_LANE2_GLOBS"] = "feature"
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
	if err := s.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "merge receipt write unconfirmed") {
		t.Fatalf("comment failure not reported: %v", err)
	}
	if f.puts != 0 || len(f.tracker.bodies) != 0 {
		t.Fatalf("failed review wrote a receipt or merged: puts=%d bodies=%q", f.puts, f.tracker.bodies)
	}
	f.tracker.failBody = ""
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("review did not recover after comment write failure: %v", err)
	}
	assertLane2Review(t, f.tracker.bodies)
	var current Ticket
	if err := readOwnership(&current, f.tracker.rows); err != nil {
		t.Fatal(err)
	}
	readMergeHandoff(&current, f.tracker.rows)
	if current.Owner != machine.OwnerResolver || current.Phase != machine.PhaseResolver || current.Progress == nil || current.Progress.Phase != "waiting" || current.Progress.QueueID != "q1" {
		t.Fatalf("review ownership not persisted: %+v", current)
	}
	if !slices.ContainsFunc(f.tracker.issue.Labels.Nodes, func(label task.NamedID) bool { return label.Name == "auto-failed" }) || f.puts != 0 || len(f.tracker.bodies) != 1 {
		t.Fatalf("review not persisted exactly once: puts=%d labels=%v bodies=%q", f.puts, f.tracker.issue.Labels.Nodes, f.tracker.bodies)
	}
}

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

// TestRuntimeMergeLane2GlobOnLane1TicketReviewsAndLeavesQueue is OF-104's
// acceptance assertion: a lane 1 handoff whose PR diff hits AGENT_LANE2_GLOBS
// (needing lane 2 validation it never got) transfers to the disposition
// agent for review, the same mechanism as OF-102's lane 2 review and OF-103's
// manual-glob review, instead of waiting on the merge queue forever.
func TestRuntimeMergeLane2GlobOnLane1TicketReviewsAndLeavesQueue(t *testing.T) {
	f := newMergeFixture(t)
	blocked := policyBlockedFixture(t, f, "lane2-change")
	f.repos[0].Env["AGENT_LANE2_GLOBS"] = "lane2-change"
	for i := 0; i < 2; i++ {
		_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	}
	assertLane2Review(t, blocked.bodies)
	if f.puts != 1 || f.tracker.issue.State.Name != "Done" || blocked.issue.State.Name == "Done" {
		t.Fatalf("lane 1 blocked behind lane 2 glob review: puts=%d lane1=%s blocked=%s", f.puts, f.tracker.issue.State.Name, blocked.issue.State.Name)
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
