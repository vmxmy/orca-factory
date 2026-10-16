package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestCanceledDeploymentSnapshotRetainsBarrierAndRecovery(t *testing.T) {
	for _, status := range []string{"Canceled", "Cancelled"} {
		for _, kind := range []string{"workflow-failed", "smoke-unknown"} {
			t.Run(status+"/"+kind, func(t *testing.T) {
				ctx := context.Background()
				f := newMergeFixture(t)
				dir := f.repos[0].Path
				env := f.repos[0].Env
				smokes, nextReads := 0, 0
				original := f.server.Config.Handler
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/pulls/2") {
						nextReads++
						http.Error(w, "later PR must remain blocked", http.StatusConflict)
						return
					}
					if strings.Contains(r.URL.Path, "/actions/") {
						if _, err := fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":19,"workflow_id":7,"path":".github/workflows/deploy.yml","head_sha":%q,"status":"completed","conclusion":"failure"}]}`, strings.Repeat("a", 40)); err != nil {
							t.Error(err)
							return
						}
						return
					}
					original.ServeHTTP(w, r)
				})
				if kind == "workflow-failed" {
					env["AGENT_DEPLOY_WORKFLOW"] = "deploy.yml"
				} else {
					env["AGENT_SMOKE_CMD"] = "smoke-test"
					f.runtime.Clients[dir] = &uncertainSmokeIssues{mergeIssues: f.tracker, loseResult: true}
				}
				f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
					if s.Path == "bash" {
						smokes++
						return nil, nil
					}
					return runx.Run(ctx, s)
				}
				if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(ctx); err == nil || f.puts != 1 {
					t.Fatalf("fixture did not persist failed/unknown deployment: err=%v puts=%d", err, f.puts)
				}
				next := *f.tracker
				next.issue.ID, next.issue.Identifier = "next", "ENG-2"
				next.rows = append([]task.Comment(nil), f.tracker.rows[:1]...)
				next.bodies = nil
				nf := *f
				nf.tracker = &next
				rewriteHandoff(t, &nf, func(h map[string]any) {
					h["ticket_id"] = "next"
					h["identifier"] = "ENG-2"
					h["pr"] = 2
					h["queue_id"] = "q2"
					h["enqueued_at"] = "2026-10-06T02:00:00Z"
				})
				source := &mergeTickets{rows: []*mergeIssues{&next, f.tracker}}
				f.runtime.Clients[dir] = source
				f.tracker.issue.State.Name = status
				f.tracker.issue.Labels.Nodes = nil
				f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "operator-cancel", CreatedAt: time.Now(), Body: "归属: 无"})
				delete(env, "AGENT_DEPLOY_WORKFLOW")
				delete(env, "AGENT_SMOKE_CMD")
				beforeSmokes := smokes
				for restart := 0; restart < 2; restart++ {
					rebuilt := *f.runtime
					rebuilt.GitHub = nil
					rows, err := rebuilt.Snapshot(ctx, f.repos)
					found := false
					for _, row := range rows {
						if row.Identifier == "ENG-1" && DeploymentBarrier(row) {
							found = true
						}
					}
					if err != nil || !found || len(rows) != 2 {
						t.Errorf("restart %d lost canceled barrier: rows=%+v err=%v", restart, rows, err)
					}
					before := len(f.tracker.bodies)
					if err := rebuilt.ResumeDeployment(ctx, "ENG-1", "q1", strings.Repeat("a", 40), "confirm-passed", "operator", "deployment reconciled", true); err != nil {
						t.Errorf("manual recovery cannot find canceled barrier: %v", err)
					}
					if len(f.tracker.bodies) != before {
						t.Fatal("dry recovery wrote")
					}
					if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(ctx); err == nil {
						t.Error("canceled barrier disappeared from scheduler")
					}
				}
				if nextReads != 0 || f.puts != 1 || smokes != beforeSmokes {
					t.Errorf("cancellation bypassed/replayed deployment: nextReads=%d merges=%d smokes=%d/%d", nextReads, f.puts, smokes, beforeSmokes)
				}
				rebuilt := *f.runtime
				rebuilt.GitHub = nil
				if err := rebuilt.ResumeDeployment(ctx, "ENG-1", "q1", strings.Repeat("a", 40), "confirm-passed", "operator", "deployment reconciled", false); err != nil {
					t.Fatalf("manual recovery failed: %v", err)
				}
				if !strings.Contains(strings.Join(f.tracker.bodies, "\n"), `"action":"confirm-passed"`) {
					t.Fatal("manual recovery receipt missing")
				}
				rows, err := rebuilt.Snapshot(ctx, f.repos)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if row.Identifier == "ENG-1" && DeploymentBarrier(row) {
						t.Fatal("explicit recovery did not release barrier")
					}
				}
			})
		}
	}
}

func TestCanceledUnmergedSnapshotDoesNotQueue(t *testing.T) {
	for _, status := range []string{"Canceled", "Cancelled"} {
		t.Run(status, func(t *testing.T) {
			f := newMergeFixture(t)
			f.tracker.issue.State.Name = status
			f.repos[0].Env["AGENT_DEPLOY_WORKFLOW"] = "deploy.yml"
			rows, err := f.runtime.Snapshot(context.Background(), f.repos)
			if err != nil || len(rows) != 0 {
				t.Fatalf("unmerged canceled ticket survived snapshot: %+v err=%v", rows, err)
			}
			s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(s.MergeQueue) != 0 || f.puts != 0 || len(f.tracker.bodies) != 0 {
				t.Fatalf("unmerged cancellation advanced: queue=%v puts=%d bodies=%v", s.MergeQueue, f.puts, f.tracker.bodies)
			}
		})
	}
}
