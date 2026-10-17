package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func lane2Handoff(h map[string]any) {
	h["lane"] = 2
	h["gate_output"] = strings.ReplaceAll(h["gate_output"].(string), "LANE 1", "LANE 2")
	h["gate_spec"] = strings.ReplaceAll(h["gate_spec"].(string), "LANE: 1", "LANE: 2")
}

func lane2Automerge(t *testing.T, f *mergeFixture, setting string) {
	t.Helper()
	if setting != "unset" {
		f.repos[0].Env["AGENT_LANE2_AUTOMERGE"] = setting
	}
}

func assertLane2Review(t *testing.T, bodies []string) {
	t.Helper()
	for _, b := range bodies {
		if strings.HasPrefix(b, "Agent处置: lane 2 复核") && strings.Contains(b, "\n阶段: Agent处置\n归属: 处置Agent\nmerge-progress-v1: ") {
			return
		}
	}
	t.Fatalf("lane 2 review not transferred to resolver: %q", bodies)
}

func TestRuntimeMergeLane2ReviewLeavesQueue(t *testing.T) {
	for _, setting := range []string{"unset", "0"} {
		t.Run(setting, func(t *testing.T) {
			f := newMergeFixture(t)
			lane2Automerge(t, f, setting)
			review := *f.tracker
			review.issue.ID = "review"
			review.issue.Identifier = "ENG-2"
			review.rows = append([]task.Comment(nil), f.tracker.rows...)
			rf := *f
			rf.tracker = &review
			rewriteHandoff(t, &rf, func(h map[string]any) {
				lane2Handoff(h)
				h["ticket_id"] = "review"
				h["identifier"] = "ENG-2"
				h["pr"] = 2
				h["queue_id"] = "q0"
				h["enqueued_at"] = "2026-10-06T00:00:00Z"
			})
			f.runtime.Clients[f.repos[0].Path] = &mergeTickets{rows: []*mergeIssues{&review, f.tracker}}
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
			for i := 0; i < 2; i++ {
				_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
			}
			assertLane2Review(t, review.bodies)
			if f.puts != 1 || f.tracker.issue.State.Name != "Done" || review.issue.State.Name == "Done" {
				t.Fatalf("lane 1 blocked behind lane 2 review: puts=%d lane1=%s lane2=%s", f.puts, f.tracker.issue.State.Name, review.issue.State.Name)
			}
		})
	}
}

func TestRuntimeMergeLane2ResolverApprovalMerges(t *testing.T) {
	for _, setting := range []string{"unset", "0"} {
		t.Run(setting, func(t *testing.T) {
			f := newMergeFixture(t)
			lane2Automerge(t, f, setting)
			rewriteHandoff(t, f, lane2Handoff)
			var h merge.Handoff
			if err := json.Unmarshal([]byte(strings.Split(f.tracker.rows[0].Body, merge.HandoffPrefix)[1]), &h); err != nil {
				t.Fatal(err)
			}
			frozen, err := json.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			f.tracker.rows[0].Body = "阶段: 排队合并\n归属: 合并通道\n" + merge.HandoffPrefix + string(frozen)
			s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertLane2Review(t, f.tracker.bodies)
			if f.puts != 0 {
				t.Fatalf("lane 2 merged without review: puts=%d", f.puts)
			}
			proposal, err := json.Marshal(resolver.Resolution{Action: "merge", Reason: "lane 2 reviewed", Handoff: &h})
			if err != nil {
				t.Fatal(err)
			}
			record, err := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "result", Outcome: "success", Result: string(proposal)})
			if err != nil {
				t.Fatal(err)
			}
			f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "result", CreatedAt: time.Now(), Body: "阶段: Agent处置\n归属: 处置Agent term=controller\n" + resolver.Prefix + string(record)})
			for i := 0; i < 3 && f.tracker.issue.State.Name != "Done"; i++ {
				_ = s.Tick(context.Background())
			}
			if f.puts != 1 || f.tracker.issue.State.Name != "Done" {
				t.Fatalf("resolver approval did not merge: puts=%d state=%s bodies=%q", f.puts, f.tracker.issue.State.Name, f.tracker.bodies)
			}
		})
	}
}

// A merge proposal whose PR the merge lane can never accept goes back to the
// resolver with the branch it must use instead of waiting in the queue forever.
func TestResolverMergeProposalRejectsForeignPRBranch(t *testing.T) {
	f := newMergeFixture(t)
	lane2Automerge(t, f, "unset")
	rewriteHandoff(t, f, lane2Handoff)
	var h merge.Handoff
	if err := json.Unmarshal([]byte(strings.Split(f.tracker.rows[0].Body, merge.HandoffPrefix)[1]), &h); err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.prBody["head"].(map[string]any)["ref"] = "vmxmy/ENG-1-resolver-2"
	proposal, _ := json.Marshal(resolver.Resolution{Action: "merge", Reason: "lane 2 reviewed", Handoff: &h})
	record, _ := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "result", Outcome: "success", Result: string(proposal)})
	f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "result", CreatedAt: time.Now(), Body: "阶段: Agent处置\n归属: 处置Agent term=controller\n" + resolver.Prefix + string(record)})
	for range 3 {
		_ = s.Tick(context.Background())
	}
	if f.puts != 0 {
		t.Fatalf("foreign branch merged: puts=%d", f.puts)
	}
	if !slices.ContainsFunc(f.tracker.bodies, func(b string) bool { return strings.Contains(b, "agent/ENG-1/"+h.Unit) && strings.Contains(b, `"phase":"waiting"`) }) {
		t.Fatalf("proposal not returned to resolver with the required branch: %q", f.tracker.bodies)
	}
}
