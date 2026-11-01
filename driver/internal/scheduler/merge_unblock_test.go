package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func tick(f *mergeFixture) error {
	return (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
}

func TestMergePrecheckHoldsOnlyOnExitOne(t *testing.T) {
	t.Run("exit 1 transfers to human and the next ticket merges", func(t *testing.T) {
		f := newMergeFixture(t)
		held := policyBlockedFixture(t, f, "needs-floor")
		f.repos[0].Env["AGENT_MERGE_PRECHECK_CMD"] = `! git cat-file -e "$AGENT_MERGE_HEAD:needs-floor"`
		for range 2 {
			_ = tick(f)
		}
		assertHumanEscalation(t, held.bodies)
		if held.issue.State.Name == "Done" || f.puts != 1 || f.tracker.issue.State.Name != "Done" {
			t.Fatalf("queue blocked behind held ticket: puts=%d held=%s next=%s", f.puts, held.issue.State.Name, f.tracker.issue.State.Name)
		}
	})
	t.Run("a hold after an unconfirmed squash keeps the frozen main", func(t *testing.T) {
		f := newMergeFixture(t)
		f.repos[0].Env["AGENT_MERGE_PRECHECK_CMD"] = "exit 1"
		tickets, err := f.runtime.Snapshot(context.Background(), f.repos)
		if err != nil || len(tickets) != 1 {
			t.Fatalf("snapshot: %v %v", tickets, err)
		}
		h := tickets[0].Handoff
		intent := merge.Progress{Version: 1, QueueID: h.QueueID, TicketID: h.TicketID, Repository: h.Repository, PR: h.PR, Head: h.Head, Main: f.main, Phase: "merge-intent", Attempt: 1, Deployment: &merge.DeploymentProgress{Workflow: "deploy.yml", State: "pending"}}
		raw, err := json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "intent", CreatedAt: time.Now(), Body: merge.ProgressPrefix + string(raw)})
		_ = tick(f)
		tickets, err = f.runtime.Snapshot(context.Background(), f.repos)
		if err != nil || len(tickets) != 1 {
			t.Fatalf("held ticket unreadable: %v %v", tickets, err)
		}
		if tickets[0].MergeProblem != "" || DeploymentBarrier(tickets[0]) || tickets[0].Progress.Main != f.main {
			t.Fatalf("hold still blocks the queue: problem=%q barrier=%v main=%q", tickets[0].MergeProblem, DeploymentBarrier(tickets[0]), tickets[0].Progress.Main)
		}
	})
	t.Run("exit 0 squashes", func(t *testing.T) {
		f := newMergeFixture(t)
		f.repos[0].Env["AGENT_MERGE_PRECHECK_CMD"] = fmt.Sprintf(`[ "$AGENT_MERGE_HEAD" = %q ]`, f.head)
		if err := tick(f); err != nil {
			t.Fatal(err)
		}
		if f.puts != 1 || f.tracker.issue.State.Name != "Done" {
			t.Fatalf("not merged: squash=%d state=%s", f.puts, f.tracker.issue.State.Name)
		}
	})
	t.Run("other failures wait without a decision", func(t *testing.T) {
		f := newMergeFixture(t)
		f.repos[0].Env["AGENT_MERGE_PRECHECK_CMD"] = "exit 127"
		err := tick(f)
		bodies := strings.Join(f.tracker.bodies, "\n")
		if err == nil || !strings.Contains(err.Error(), "merge precheck unavailable") || f.puts != 0 || strings.Contains(bodies, "转人") {
			t.Fatalf("infrastructure failure decided: err=%v squash=%d bodies=%s", err, f.puts, bodies)
		}
	})
}

func TestDeploymentAutoRerunRunsAtMostOnce(t *testing.T) {
	type script struct {
		name        string
		autoRerun   bool
		mainMoved   bool
		afterRerun  string
		wantReruns  int
		wantDone    bool
		wantResolve bool
	}
	for _, c := range []script{
		{name: "transient failure recovers", autoRerun: true, afterRerun: "success", wantReruns: 1, wantDone: true},
		{name: "persistent failure pauses after one rerun", autoRerun: true, afterRerun: "failure", wantReruns: 1, wantResolve: true},
		{name: "never reruns over a newer main", autoRerun: true, mainMoved: true, afterRerun: "success", wantResolve: true},
		{name: "disabled by default", afterRerun: "success", wantResolve: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMergeFixture(t)
			f.merged = true
			f.repos[0].Env["AGENT_DEPLOY_WORKFLOW"] = "deploy.yml"
			if c.autoRerun {
				f.repos[0].Env["AGENT_DEPLOY_AUTO_RERUN"] = "1"
			}
			sha, _ := f.prBody["merge_commit_sha"].(string)
			if !c.mainMoved {
				f.main = sha
			}
			reruns := 0
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/actions/runs/19/rerun-failed-jobs"):
					if r.Method != http.MethodPost {
						t.Errorf("rerun method %s", r.Method)
					}
					reruns++
					w.WriteHeader(201)
					if _, err := fmt.Fprint(w, `{}`); err != nil {
						t.Error(err)
					}
				case strings.Contains(r.URL.Path, "/actions/workflows/"):
					conclusion := "failure"
					if reruns > 0 {
						conclusion = c.afterRerun
					}
					if _, err := fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":19,"workflow_id":7,"path":".github/workflows/deploy.yml@refs/heads/main","head_sha":%q,"status":"completed","conclusion":%q}]}`, sha, conclusion); err != nil {
						t.Error(err)
					}
				default:
					original.ServeHTTP(w, r)
				}
			})
			for range 3 {
				_ = tick(f)
			}
			bodies := strings.Join(f.tracker.bodies, "\n")
			if reruns != c.wantReruns {
				t.Fatalf("reruns=%d want %d bodies=%s", reruns, c.wantReruns, bodies)
			}
			if done := f.tracker.issue.State.Name == "Done"; done != c.wantDone {
				t.Fatalf("done=%v want %v bodies=%s", done, c.wantDone, bodies)
			}
			if resolve := strings.Contains(bodies, "Agent处置: deployment workflow failed"); resolve != c.wantResolve {
				t.Fatalf("paused=%v want %v bodies=%s", resolve, c.wantResolve, bodies)
			}
		})
	}
}
