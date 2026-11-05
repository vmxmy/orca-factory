package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestDeploymentRestartKeepsBarrierAndOtherRepositoryContinues(t *testing.T) {
	f := newMergeFixture(t)
	f.repos[0].Env["AGENT_DEPLOY_WORKFLOW"] = "deploy.yml"
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
	deployState := "in_progress"
	smokes := 0
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "bash" {
			smokes++
			return nil, nil
		}
		return runx.Run(ctx, s)
	}
	b := *f.tracker
	b.issue.ID = "b"
	b.issue.Project.ID = "o/s"
	b.rows = append([]task.Comment(nil), f.tracker.rows...)
	bf := *f
	bf.tracker = &b
	rewriteHandoff(t, &bf, func(h map[string]any) { h["repository"] = "o/s"; h["ticket_id"] = "b"; h["queue_id"] = "b" })
	dir := t.TempDir()
	gitMergeTest(t, dir, "clone", f.repos[0].Path, ".")
	br := config.Repository{Path: dir, StaleHours: 8, MaxInFlight: 3, Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "o/s", "AGENT_SCOPE_AVOID": "0"}}
	f.repos = append(f.repos, br)
	f.runtime.Repositories[dir] = br
	source := &mergeTickets{rows: []*mergeIssues{f.tracker, &b}}
	f.runtime.Clients[dir] = source
	f.runtime.Clients[f.repos[0].Path] = source
	original := f.server.Config.Handler
	bMerged := false
	bPuts := 0
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/actions/workflows/") {
			if r.URL.Query().Get("head_sha") != strings.Repeat("a", 40) || !strings.HasSuffix(r.URL.Path, "/deploy.yml/runs") {
				t.Errorf("wrong deployment identity: %s", r.URL)
			}
			if _, err := fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":19,"workflow_id":7,"path":".github/workflows/deploy.yml","head_sha":%q,"status":%q,"conclusion":"success"}]}`, strings.Repeat("a", 40), deployState); err != nil {
				t.Error(err)
				return
			}
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/repos/o/s/") {
			original.ServeHTTP(w, r)
			return
		}
		raw, rawMarshalErr := json.Marshal(f.prBody)
		if rawMarshalErr != nil {
			t.Error(rawMarshalErr)
			return
		}
		var pr map[string]any
		if err := json.Unmarshal(raw, &pr); err != nil {
			t.Error(err)
			return
		}
		pr["head"].(map[string]any)["repo"] = map[string]any{"full_name": "o/s"}
		pr["base"].(map[string]any)["repo"] = map[string]any{"full_name": "o/s"}
		pr["merged"] = bMerged
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/1/merge"):
			bPuts++
			bMerged = true
			if _, err := fmt.Fprint(w, `{}`); err != nil {
				t.Error(err)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			if err := json.NewEncoder(w).Encode(pr); err != nil {
				t.Error(err)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			if bMerged {
				if err := json.NewEncoder(w).Encode([]any{}); err != nil {
					t.Error(err)
					return
				}
			} else {
				if err := json.NewEncoder(w).Encode([]any{pr}); err != nil {
					t.Error(err)
					return
				}
			}
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			if _, err := fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":1,"status":"completed","conclusion":"success","head_sha":%q}]}`, f.head); err != nil {
				t.Error(err)
				return
			}
		default:
			original.ServeHTTP(w, r)
		}
	})
	err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	if err == nil || f.puts != 1 || bPuts != 1 || b.issue.State.Name != "Done" || f.tracker.issue.State.Name != "In Progress" {
		t.Fatalf("repository isolation failed: err=%v a=%d b=%d state=%s", err, f.puts, bPuts, b.issue.State.Name)
	}
	// An operator may mark the issue Done or transfer ownership while waiting.
	f.tracker.issue.State.Name = "Done"
	f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "operator", CreatedAt: time.Now(), Body: "归属: 无"})
	for i := 0; i < 2; i++ {
		rebuilt := *f.runtime
		rebuilt.GitHub = nil
		if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err == nil {
			t.Fatal("lost deployment barrier on restart")
		}
	}
	if smokes != 0 || f.puts != 1 {
		t.Fatalf("pending performed side effects: smoke=%d merges=%d", smokes, f.puts)
	}
	deployState = "completed"
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if smokes != 1 || f.puts != 1 || !strings.Contains(strings.Join(f.tracker.bodies, "\n"), `"state":"passed"`) {
		t.Fatalf("deployment not completed: smoke=%d merges=%d receipts=%v", smokes, f.puts, f.tracker.bodies)
	}

}

func TestDeploymentFailureRequiresExplicitRecovery(t *testing.T) {
	f := newMergeFixture(t)
	f.repos[0].Env["AGENT_DEPLOY_WORKFLOW"] = "deploy.yml"
	conclusion := "failure"
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/actions/") {
			if _, err := fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":19,"workflow_id":7,"path":".github/workflows/deploy.yml","head_sha":%q,"status":"completed","conclusion":%q}]}`, strings.Repeat("a", 40), conclusion); err != nil {
				t.Error(err)
				return
			}
			return
		}
		original.ServeHTTP(w, r)
	})
	_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	if !strings.Contains(strings.Join(f.tracker.bodies, "\n"), `"state":"paused"`) || !strings.Contains(strings.Join(f.tracker.bodies, "\n"), "归属: 处置Agent") {
		t.Fatalf("failure not durably escalated: %v", f.tracker.bodies)
	}
	conclusion = "success"
	f.tracker.issue.State.Name = "Done"
	for i := 0; i < 2; i++ {
		rebuilt := *f.runtime
		rebuilt.GitHub = nil
		if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err == nil {
			t.Fatal("restart/credential recovery bypassed human confirmation")
		}
	}
}

func TestSelfDeployInterruptedBeforeInstallRetries(t *testing.T) {
	for _, installing := range []bool{false, true} {
		t.Run(fmt.Sprintf("installing=%t", installing), func(t *testing.T) {
			f, host := newSelfDeployFixture(t)
			host.kill = "go"
			firstServe(t, f, host)
			if lastState(f) != "deploy-intent" || !markerExists(f, selfSHA, "drained") || markerExists(f, selfSHA, "installing") || markerExists(f, selfSHA, "rolled-back") || installed(t, host.driver) != "old" {
				t.Fatalf("invalid interrupted fixture: state=%s installed=%s", lastState(f), installed(t, host.driver))
			}
			if installing {
				if err := os.WriteFile(f.runtime.selfMarker(selfSHA, "installing"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_ = serveAs(f, host, "old")
			if installing {
				if lastState(f) != "paused" || maintenanceMode(t) != "running" || host.builds != 0 || host.restarts != 0 {
					t.Fatalf("uncertain install did not pause: state=%s builds=%d restarts=%d", lastState(f), host.builds, host.restarts)
				}
				return
			}
			if lastState(f) != "pending" {
				t.Fatalf("interrupted before install must retry: state=%s", lastState(f))
			}
			_ = serveAs(f, host, "old")
			if lastState(f) != "deploy-intent" || host.builds != 1 || host.restarts != 1 || host.smokes != 0 || installed(t, host.driver) != selfSHA {
				t.Fatalf("next tick did not reinstall: state=%s builds=%d restarts=%d smokes=%d installed=%s", lastState(f), host.builds, host.restarts, host.smokes, installed(t, host.driver))
			}
		})
	}
}

// A paused head keeps its lane paused on every later tick, also after a
// restart; the skip is logged so the tickets queued behind it do not look like
// ordinary waits.
func TestPausedDeploymentLaneIsLoggedEachTick(t *testing.T) {
	f, host := newSelfDeployFixture(t)
	host.kill = "go"
	firstServe(t, f, host)
	if err := os.WriteFile(f.runtime.selfMarker(selfSHA, "installing"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_ = serveAs(f, host, "old")
	if lastState(f) != "paused" {
		t.Fatalf("expected paused deployment: state=%s", lastState(f))
	}
	var logs bytes.Buffer
	ctx := runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
	tickets, err := f.runtime.Snapshot(ctx, f.repos)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.runtime.AdvanceMerges(ctx, tickets); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, `"msg":"merge lane paused`) || !strings.Contains(out, `"ticket":"ENG-1"`) || !strings.Contains(out, `"reason":"installed driver is not the squash SHA`) {
		t.Fatalf("paused lane not logged: %s", out)
	}
}

func TestSelfDeployRetryWorkflowBeforeSmokeReinstalls(t *testing.T) {
	f, host := newSelfDeployFixture(t)
	host.kill = "go"
	firstServe(t, f, host)
	if err := os.WriteFile(f.runtime.selfMarker(selfSHA, "installing"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_ = serveAs(f, host, "old")
	if lastState(f) != "paused" {
		t.Fatalf("expected paused deployment: state=%s", lastState(f))
	}
	if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", selfSHA, "retry-workflow", "operator", "reconciled interrupted install; reinstall", false); err != nil {
		t.Fatal(err)
	}
	if lastState(f) != "pending" {
		t.Fatalf("reinstall recovery must be pending: state=%s", lastState(f))
	}
	_ = serveAs(f, host, "old")
	if lastState(f) != "deploy-intent" || host.builds != 1 || host.restarts != 1 || installed(t, host.driver) != selfSHA {
		t.Fatalf("recovery did not reinstall: state=%s builds=%d restarts=%d installed=%s", lastState(f), host.builds, host.restarts, installed(t, host.driver))
	}
}

// Only a deployment no recovery has touched reinstalls by itself; an
// interrupted reinstall pauses until a new explicit decision.
func TestSelfDeployRecoveredReinstallInterruptedPauses(t *testing.T) {
	f, host := newSelfDeployFixture(t)
	host.kill = "go"
	firstServe(t, f, host)
	if err := os.WriteFile(f.runtime.selfMarker(selfSHA, "installing"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_ = serveAs(f, host, "old")
	if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", selfSHA, "retry-workflow", "operator", "reconciled interrupted install; reinstall", false); err != nil {
		t.Fatal(err)
	}
	host.kill = "go"
	_ = serveAs(f, host, "old")
	_ = serveAs(f, host, "old")
	if lastState(f) != "paused" || host.builds != 0 || host.restarts != 0 {
		t.Fatalf("recovered reinstall retried without a decision: state=%s builds=%d restarts=%d", lastState(f), host.builds, host.restarts)
	}
	if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", selfSHA, "retry-workflow", "operator", "reinstall again", false); err != nil {
		t.Fatalf("a second reinstall decision before any smoke was rejected: %v", err)
	}
	_ = serveAs(f, host, "old")
	if lastState(f) != "deploy-intent" || host.builds != 1 || host.restarts != 1 || installed(t, host.driver) != selfSHA {
		t.Fatalf("second decision did not reinstall: state=%s builds=%d restarts=%d installed=%s", lastState(f), host.builds, host.restarts, installed(t, host.driver))
	}
}

func TestDeploymentRetryWorkflowRerunsFailedRun(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		autoRerun    bool
		mainMoved    bool
		rerunFails   bool
		requestFails bool
	}{
		{name: "automatic budget exhausted", autoRerun: true},
		{name: "automatic rerun disabled"},
		{name: "main moved after recovery", autoRerun: true, mainMoved: true},
		{name: "one rerun per recovery", autoRerun: true, rerunFails: true},
		{name: "rerun request result lost", autoRerun: true, rerunFails: true, requestFails: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newMergeFixture(t)
			f.merged = true
			f.repos[0].Env["AGENT_DEPLOY_WORKFLOW"] = "deploy.yml"
			if scenario.autoRerun {
				f.repos[0].Env["AGENT_DEPLOY_AUTO_RERUN"] = "1"
			}
			sha := strings.Repeat("a", 40)
			f.main = sha
			conclusion := "failure"
			reruns := 0
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/actions/runs/19/rerun-failed-jobs"):
					if r.Method != http.MethodPost {
						t.Errorf("rerun method = %s, want POST", r.Method)
					}
					body := f.tracker.bodies[len(f.tracker.bodies)-1]
					if strings.Contains(body, `"workflow_retry_pending":true`) || !strings.Contains(body, fmt.Sprintf(`"reruns":%d`, reruns+1)) {
						t.Errorf("rerun not reserved before request: %s", body)
					}
					reruns++
					if scenario.requestFails && reruns == 2 {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusCreated)
				case strings.Contains(r.URL.Path, "/actions/workflows/"):
					if _, err := fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":19,"workflow_id":7,"path":".github/workflows/deploy.yml","head_sha":%q,"status":"completed","conclusion":%q}]}`, sha, conclusion); err != nil {
						t.Error(err)
					}
				default:
					original.ServeHTTP(w, r)
				}
			})
			for range 2 {
				_ = tick(f)
			}
			initialReruns := 0
			if scenario.autoRerun {
				initialReruns = 1
			}
			if reruns != initialReruns || !strings.Contains(strings.Join(f.tracker.bodies, "\n"), `"state":"paused"`) {
				t.Fatalf("automatic rerun not exhausted: reruns=%d bodies=%v", reruns, f.tracker.bodies)
			}
			if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", sha, "retry-workflow", "operator", "safe to rerun failed deployment", false); err != nil {
				t.Fatal(err)
			}
			if scenario.mainMoved {
				f.main = strings.Repeat("b", 40)
			}
			rebuilt := *f.runtime
			rebuilt.GitHub = nil
			scheduler := &Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}
			if scenario.mainMoved {
				err := scheduler.Tick(context.Background())
				if err == nil || !strings.Contains(err.Error(), "deployment workflow failed; main moved past merge SHA") || reruns != initialReruns {
					t.Fatalf("retry over newer main: err=%v reruns=%d", err, reruns)
				}
				if f.tracker.issue.State.Name == "Done" || !strings.Contains(f.tracker.bodies[len(f.tracker.bodies)-1], `"state":"paused"`) {
					t.Fatalf("main moved without durable pause: state=%s bodies=%v", f.tracker.issue.State.Name, f.tracker.bodies)
				}
				if err := rebuilt.ResumeDeployment(context.Background(), "ENG-1", "q1", sha, "confirm-passed", "operator", "later deployment verified", false); err != nil {
					t.Fatalf("confirm-passed after main moved: %v", err)
				}
				return
			}
			wantError := "deployment pending"
			if scenario.requestFails {
				wantError = "deployment rerun failed"
			}
			if err := scheduler.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("retry did not wait for rerun: %v", err)
			}
			wantReruns := initialReruns + 1
			if reruns != wantReruns || f.tracker.issue.State.Name != "In Progress" {
				t.Fatalf("manual retry did not rerun failed deployment: reruns=%d state=%s", reruns, f.tracker.issue.State.Name)
			}
			if scenario.rerunFails {
				if err := scheduler.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "deployment workflow failed") {
					t.Fatalf("failed rerun did not pause: %v", err)
				}
				if reruns != wantReruns {
					t.Fatalf("recovery reran more than once: reruns=%d want=%d", reruns, wantReruns)
				}
				if err := rebuilt.ResumeDeployment(context.Background(), "ENG-1", "q1", sha, "retry-workflow", "operator", "reconciled failed retry; retry once more", false); err != nil {
					t.Fatal(err)
				}
				if err := scheduler.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "deployment pending") {
					t.Fatalf("new recovery did not wait for rerun: %v", err)
				}
				wantReruns++
				if reruns != wantReruns {
					t.Fatalf("new recovery did not rerun once: reruns=%d want=%d", reruns, wantReruns)
				}
			}
			conclusion = "success"
			if err := scheduler.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if reruns != wantReruns || f.tracker.issue.State.Name != "Done" {
				t.Fatalf("successful rerun did not complete ticket: reruns=%d state=%s", reruns, f.tracker.issue.State.Name)
			}
		})
	}
}

// ZY-365, 2026-11-04: the auto rerun was used, the deployment paused, and a
// retry-workflow recovery paused again without a rerun. These old v1 receipts
// predate workflow_retry_pending. The smoke command and reasons are redacted.
func TestRecordedDeploymentRetryWorkflowReceipts(t *testing.T) {
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded(t, "ledger-comments-deployment-retry-workflow.json"), &recording); err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{ID: "699afdbb-3850-4999-8363-389adbfa4ed9"}
	readMergeHandoff(&ticket, recording.Comments)
	d := ticket.Progress.Deployment
	if ticket.MergeProblem != "" || d.State != "paused" || d.Recovery == nil || d.Recovery.Action != "retry-workflow" || d.WorkflowRetryPending {
		t.Fatalf("recorded retry-workflow receipts: problem=%q deployment=%+v", ticket.MergeProblem, d)
	}
}

// OF-393, 2026-11-05: a self deployment wrote deploy-intent, serve stopped
// before the install, and the next serve paused it. The interrupted retry and
// the reinstall recovery must both extend this real history.
func TestRecordedSelfDeployInterruptedBeforeInstall(t *testing.T) {
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded(t, "ledger-comments-selfdeploy-interrupted.json"), &recording); err != nil {
		t.Fatal(err)
	}
	extend := func(history []task.Comment, from task.Comment, change func(*merge.DeploymentProgress)) Ticket {
		var next merge.Progress
		if err := json.Unmarshal([]byte(from.Body[strings.Index(from.Body, merge.ProgressPrefix)+len(merge.ProgressPrefix):]), &next); err != nil {
			t.Fatal(err)
		}
		d := *next.Deployment
		change(&d)
		next.Deployment = &d
		b, _ := json.Marshal(next)
		at := from.CreatedAt.Add(time.Minute)
		ticket := Ticket{ID: next.TicketID}
		readMergeHandoff(&ticket, append(append([]task.Comment(nil), history...), task.Comment{ID: at.Format(time.RFC3339), Body: merge.ProgressPrefix + string(b), CreatedAt: at}))
		return ticket
	}
	intent, paused := recording.Comments[3], recording.Comments[4]
	retried := extend(recording.Comments[:4], intent, func(d *merge.DeploymentProgress) { d.State, d.Reruns = "pending", d.Reruns+1 })
	if retried.MergeProblem != "" || retried.Progress.Deployment.State != "pending" {
		t.Fatalf("interrupted retry: problem=%q deployment=%+v", retried.MergeProblem, retried.Progress.Deployment)
	}
	recovered := extend(recording.Comments, paused, func(d *merge.DeploymentProgress) {
		d.State, d.Reason = "pending", ""
		d.Recovery = &merge.DeploymentRecovery{Action: "retry-workflow", Operator: "operator", Reason: "reinstall", At: paused.CreatedAt.Add(time.Second)}
	})
	if recovered.MergeProblem != "" || recovered.Progress.Deployment.State != "pending" || recovered.Progress.Deployment.WorkflowRetryPending {
		t.Fatalf("reinstall recovery: problem=%q deployment=%+v", recovered.MergeProblem, recovered.Progress.Deployment)
	}
	history := append([]task.Comment(nil), recording.Comments...)
	step := func(change func(*merge.DeploymentProgress)) Ticket {
		from := history[len(history)-1]
		ticket := extend(history, from, change)
		if ticket.MergeProblem == "" {
			at := from.CreatedAt.Add(time.Minute)
			history = append(history, task.Comment{ID: at.Format(time.RFC3339), Body: merge.ProgressPrefix + progressJSON(t, ticket.Progress), CreatedAt: at})
		}
		return ticket
	}
	// Reinstall decisions reuse the recovery a resolver wrote on 2026-11-04.
	var workflowRecording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded(t, "ledger-comments-deployment-retry-workflow.json"), &workflowRecording); err != nil {
		t.Fatal(err)
	}
	var resolved merge.Progress
	body := workflowRecording.Comments[5].Body
	if err := json.NewDecoder(strings.NewReader(body[strings.Index(body, merge.ProgressPrefix)+len(merge.ProgressPrefix):])).Decode(&resolved); err != nil {
		t.Fatal(err)
	}
	reinstall := *resolved.Deployment.Recovery
	decide := func(action, state string) func(*merge.DeploymentProgress) {
		return func(d *merge.DeploymentProgress) {
			r := reinstall
			if action != r.Action {
				r = merge.DeploymentRecovery{Action: action, Operator: r.Operator, Reason: r.Reason}
			}
			r.At = history[len(history)-1].CreatedAt.Add(30 * time.Second)
			d.State, d.Reason, d.SmokeStarted = state, "", false
			d.Recovery = &r
		}
	}
	set := func(state string, smoked bool) func(*merge.DeploymentProgress) {
		return func(d *merge.DeploymentProgress) {
			d.State, d.SmokeStarted = state, smoked
			if state == "paused" {
				d.Reason = "paused"
			}
		}
	}
	for i, s := range []struct {
		name   string
		change func(*merge.DeploymentProgress)
		valid  bool
	}{
		{"reinstall decision", decide("retry-workflow", "pending"), true},
		{"reinstall intent", set("deploy-intent", false), true},
		{"pending without a new decision", func(d *merge.DeploymentProgress) { d.State, d.Reruns = "pending", d.Reruns+1 }, false},
		{"reinstall interrupted", set("paused", false), true},
		{"second reinstall decision", decide("retry-workflow", "pending"), true},
		{"second reinstall intent", set("deploy-intent", false), true},
		{"smoke", set("smoke-intent", true), true},
		{"smoke failed", set("paused", true), true},
		{"smoke retry decision", decide("retry-smoke", "deploy-intent"), true},
		{"smoke retry paused", set("paused", false), true},
		{"reinstall after a smoke ran", decide("retry-workflow", "pending"), false},
	} {
		if got := step(s.change); (got.MergeProblem == "") != s.valid {
			t.Fatalf("step %d %s: valid=%t problem=%q", i, s.name, s.valid, got.MergeProblem)
		}
	}
}

func progressJSON(t *testing.T, p *merge.Progress) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDeploymentRetryPendingCannotBeRearmed(t *testing.T) {
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded(t, "ledger-comments-deployment-retry-workflow.json"), &recording); err != nil {
		t.Fatal(err)
	}
	paused := recording.Comments[4]
	var base merge.Progress
	if err := json.Unmarshal([]byte(paused.Body[strings.Index(paused.Body, merge.ProgressPrefix)+len(merge.ProgressPrefix):]), &base); err != nil {
		t.Fatal(err)
	}
	at := paused.CreatedAt
	receipt := func(change func(*merge.DeploymentProgress)) task.Comment {
		next := base
		d := *base.Deployment
		change(&d)
		next.Deployment = &d
		b, _ := json.Marshal(next)
		at = at.Add(time.Minute)
		return task.Comment{ID: at.Format(time.RFC3339), Body: merge.ProgressPrefix + string(b), CreatedAt: at}
	}
	recovery := func(action string) func(*merge.DeploymentProgress) {
		return func(d *merge.DeploymentProgress) {
			d.Recovery = &merge.DeploymentRecovery{Action: action, Operator: "operator", Reason: "reconciled", At: paused.CreatedAt.Add(time.Second)}
			d.Reason = ""
			d.State = "pending"
			if action == "confirm-passed" {
				d.State = "passed"
			}
		}
	}
	armed := func(change func(*merge.DeploymentProgress)) func(*merge.DeploymentProgress) {
		return func(d *merge.DeploymentProgress) {
			change(d)
			d.WorkflowRetryPending = true
		}
	}
	consumed := func(d *merge.DeploymentProgress) {
		d.WorkflowRetryPending = false
		d.Reruns++
	}
	retry := recovery("retry-workflow")
	for _, scenario := range []struct {
		name     string
		receipts []func(*merge.DeploymentProgress)
		problem  string
	}{
		{name: "retry consumed once", receipts: []func(*merge.DeploymentProgress){armed(retry), func(d *merge.DeploymentProgress) { retry(d); consumed(d) }}},
		{name: "consumed retry rearmed", problem: "conflicting deployment progress", receipts: []func(*merge.DeploymentProgress){armed(retry), func(d *merge.DeploymentProgress) { retry(d); consumed(d) }, func(d *merge.DeploymentProgress) { retry(d); consumed(d); d.WorkflowRetryPending = true }}},
		{name: "confirm-passed carries retry", problem: "conflicting deployment progress", receipts: []func(*merge.DeploymentProgress){armed(recovery("confirm-passed"))}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			at = paused.CreatedAt
			comments := append([]task.Comment(nil), recording.Comments[:5]...)
			for _, change := range scenario.receipts {
				comments = append(comments, receipt(change))
			}
			ticket := Ticket{ID: base.TicketID}
			readMergeHandoff(&ticket, comments)
			if ticket.MergeProblem != scenario.problem {
				t.Fatalf("merge problem = %q, want %q; last=%s", ticket.MergeProblem, scenario.problem, comments[len(comments)-1].Body)
			}
		})
	}
	t.Run("first receipt preset", func(t *testing.T) {
		first := base
		d := *base.Deployment
		d.State, d.Reason, d.Recovery, d.Reruns, d.WorkflowRetryPending = "pending", "", nil, 0, true
		first.Deployment = &d
		if deploymentTransition(nil, &first) {
			t.Fatal("first deployment receipt armed a workflow rerun without a recovery decision")
		}
	})
}

type uncertainSmokeIssues struct {
	*mergeIssues
	loseResult bool
}

func (l *uncertainSmokeIssues) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	if l.loseResult && strings.Contains(body, `"state":"passed"`) {
		return task.Comment{}, fmt.Errorf("lost result")
	}
	return l.mergeIssues.CreateComment(ctx, id, body)
}
func TestDeploymentSmokeUnconfirmedNeverReplaysAndCanBeReconciled(t *testing.T) {
	f := newMergeFixture(t)
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
	l := &uncertainSmokeIssues{mergeIssues: f.tracker, loseResult: true}
	f.runtime.Clients[f.repos[0].Path] = l
	smokes := 0
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "bash" {
			smokes++
			if !strings.Contains(strings.Join(s.Env, "\n"), "AGENT_MERGE_SHA="+strings.Repeat("a", 40)) {
				t.Fatal("smoke missing actual squash identity")
			}
			return nil, nil
		}
		return runx.Run(ctx, s)
	}
	_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	if smokes != 1 {
		t.Fatalf("smoke not executed via configured bash adapter: %d", smokes)
	}
	l.loseResult = false
	for i := 0; i < 2; i++ {
		rebuilt := *f.runtime
		rebuilt.GitHub = nil
		_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background())
	}
	if smokes != 1 || !strings.Contains(strings.Join(f.tracker.bodies, "\n"), `"state":"paused"`) {
		t.Fatalf("uncertain smoke replayed or not escalated: %d %v", smokes, f.tracker.bodies)
	}
}

func TestDeploymentManualRecoveryIsPinnedAndDurable(t *testing.T) {
	f := newMergeFixture(t)
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
	smokes := 0
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "bash" {
			smokes++
			return nil, fmt.Errorf("failed")
		}
		return runx.Run(ctx, s)
	}
	_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	recovery, ok := any(f.runtime).(interface {
		ResumeDeployment(context.Context, string, string, string, string, string, string, bool) error
	})
	if !ok {
		t.Fatal("no explicit deployment recovery entrypoint")
	}
	before := len(f.tracker.bodies)
	if err := recovery.ResumeDeployment(context.Background(), "ENG-1", "wrong", strings.Repeat("a", 40), "confirm-passed", "operator", "checked production", false); err == nil {
		t.Fatal("stale identity accepted")
	}
	if err := recovery.ResumeDeployment(context.Background(), "ENG-1", "q1", strings.Repeat("a", 40), "confirm-passed", "operator", "checked production", true); err != nil {
		t.Fatal(err)
	}
	if len(f.tracker.bodies) != before || smokes != 1 {
		t.Fatal("dry-run changed recovery")
	}
	if err := recovery.ResumeDeployment(context.Background(), "ENG-1", "q1", strings.Repeat("a", 40), "confirm-passed", "operator", "checked production", false); err != nil {
		t.Fatal(err)
	}
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if smokes != 1 || f.tracker.issue.State.Name != "Done" || !strings.Contains(strings.Join(f.tracker.bodies, "\n"), `"action":"confirm-passed"`) {
		t.Fatalf("manual confirmation lost/replayed: smoke=%d %v", smokes, f.tracker.bodies)
	}
}

func TestDeploymentDoneWithDamagedEvidenceFailsClosed(t *testing.T) {
	for _, damage := range []string{"invalid-json", "wrong-queue", "unknown-phase", "missing-deployment", "missing-sha"} {
		t.Run(damage, func(t *testing.T) {
			f := newMergeFixture(t)
			f.merged = true
			f.repos[0].Env["AGENT_DEPLOY_WORKFLOW"] = "deploy.yml"
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/actions/") {
					if _, err := fmt.Fprint(w, `{"total_count":0,"workflow_runs":[]}`); err != nil {
						t.Error(err)
						return
					}
					return
				}
				original.ServeHTTP(w, r)
			})
			_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
			var p map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(f.tracker.bodies[len(f.tracker.bodies)-1], "merge-progress-v1: ")), &p); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "wrong-queue":
				p["queue_id"] = "wrong"
			case "unknown-phase":
				p["phase"] = "future"
			case "missing-deployment":
				delete(p, "deployment")
			case "missing-sha":
				p["merge_sha"] = ""
			}
			raw, rawMarshalErr := json.Marshal(p)
			if rawMarshalErr != nil {
				t.Fatal(rawMarshalErr)
			}
			if damage == "invalid-json" {
				raw = []byte("{")
			}
			f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "damaged", CreatedAt: time.Now(), Body: "归属: 无\nmerge-progress-v1: " + string(raw)})
			f.tracker.issue.State.Name = "Done"
			f.repos[0].Env["AGENT_DEPLOY_WORKFLOW"] = ""
			rebuilt := *f.runtime
			rebuilt.GitHub = nil
			if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err == nil {
				t.Fatal("damaged durable barrier vanished with Done/config removal")
			}
		})
	}
}

func TestDeploymentFIFOOnlyReleasesAfterConfirmedSmoke(t *testing.T) {
	f := newMergeFixture(t)
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
	l := &uncertainSmokeIssues{mergeIssues: f.tracker, loseResult: true}
	f.runtime.Clients[f.repos[0].Path] = l
	smokes := 0
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "bash" {
			smokes++
			return nil, nil
		}
		return runx.Run(ctx, s)
	}
	_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	// Introduce a later queue member after the first merge, preserving all external evidence.
	next := *f.tracker
	next.issue.ID = "next"
	next.issue.Identifier = "ENG-2"
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
	f.runtime.Clients[f.repos[0].Path] = source
	nextPuts := 0
	nextMerged := false
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, rawMarshalErr := json.Marshal(f.prBody)
		if rawMarshalErr != nil {
			t.Error(rawMarshalErr)
			return
		}
		var pr map[string]any
		if err := json.Unmarshal(raw, &pr); err != nil {
			t.Error(err)
			return
		}
		pr["number"] = 2
		pr["merged"] = nextMerged
		pr["head"].(map[string]any)["ref"] = "agent/ENG-2/executor"
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/2/merge"):
			nextPuts++
			nextMerged = true
			if _, err := fmt.Fprint(w, `{}`); err != nil {
				t.Error(err)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/pulls/2"):
			if err := json.NewEncoder(w).Encode(pr); err != nil {
				t.Error(err)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			if nextMerged {
				if err := json.NewEncoder(w).Encode([]any{}); err != nil {
					t.Error(err)
					return
				}
			} else {
				if err := json.NewEncoder(w).Encode([]any{pr}); err != nil {
					t.Error(err)
					return
				}
			}
		default:
			original.ServeHTTP(w, r)
		}
	})
	// Config removal cannot erase the durable barrier or authorize replay.
	f.repos[0].Env["AGENT_SMOKE_CMD"] = ""
	f.tracker.issue.State.Name = "Done"
	f.tracker.issue.Labels.Nodes = []task.NamedID{{Name: "auto-failed"}}
	for i := 0; i < 2; i++ {
		rebuilt := *f.runtime
		rebuilt.GitHub = nil
		_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background())
	}
	if nextPuts != 0 || smokes != 1 {
		t.Fatalf("later PR bypassed uncertain smoke: next=%d smoke=%d", nextPuts, smokes)
	}
	if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", strings.Repeat("a", 40), "confirm-passed", "operator", "smoke result reconciled", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rebuilt := *f.runtime
		rebuilt.GitHub = nil
		if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if nextPuts != 1 || smokes != 1 || next.issue.State.Name != "Done" {
		t.Fatalf("next PR did not advance once: next=%d smoke=%d state=%s", nextPuts, smokes, next.issue.State.Name)
	}
}

func TestDeploymentOnlyWorkflowManualRetryAndOnlySmokeExplicitReplay(t *testing.T) {
	for _, kind := range []string{"workflow", "smoke"} {
		t.Run(kind, func(t *testing.T) {
			f := newMergeFixture(t)
			smokes := 0
			failed := true
			if kind == "workflow" {
				f.repos[0].Env["AGENT_DEPLOY_WORKFLOW"] = "deploy.yml"
			} else {
				f.repos[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
			}
			f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if s.Path == "bash" {
					smokes++
					if failed {
						return nil, fmt.Errorf("failed")
					}
					return nil, nil
				}
				return runx.Run(ctx, s)
			}
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/actions/") {
					conclusion := "success"
					if failed {
						conclusion = "failure"
					}
					if _, err := fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":19,"workflow_id":7,"path":".github/workflows/deploy.yml","head_sha":%q,"status":"completed","conclusion":%q}]}`, strings.Repeat("a", 40), conclusion); err != nil {
						t.Error(err)
						return
					}
					return
				}
				original.ServeHTTP(w, r)
			})
			_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
			failed = false
			action := "retry-workflow"
			if kind == "smoke" {
				action = "retry-smoke"
				if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", strings.Repeat("a", 40), "retry-workflow", "operator", "reconciled", false); err == nil {
					t.Fatal("workflow retry bypassed uncertain smoke")
				}
			}
			if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", strings.Repeat("a", 40), action, "operator", "production reconciled; safe to retry", false); err != nil {
				t.Fatal(err)
			}
			rebuilt := *f.runtime
			rebuilt.GitHub = nil
			if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantSmoke := 0
			if kind == "smoke" {
				wantSmoke = 2
			}
			if f.puts != 1 || smokes != wantSmoke || f.tracker.issue.State.Name != "Done" {
				t.Fatalf("retry failed: merges=%d smoke=%d state=%s", f.puts, smokes, f.tracker.issue.State.Name)
			}
		})
	}
}

func TestDeploymentSmokeIntentCannotBeResetWithoutHumanReceipt(t *testing.T) {
	f := newMergeFixture(t)
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
	l := &uncertainSmokeIssues{mergeIssues: f.tracker, loseResult: true}
	f.runtime.Clients[f.repos[0].Path] = l
	smokes := 0
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "bash" {
			smokes++
			return nil, nil
		}
		return runx.Run(ctx, s)
	}
	_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	var p map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(f.tracker.bodies[len(f.tracker.bodies)-1], "merge-progress-v1: ")), &p); err != nil {
		t.Fatal(err)
	}
	p["deployment"].(map[string]any)["state"] = "pending"
	raw, rawMarshalErr := json.Marshal(p)
	if rawMarshalErr != nil {
		t.Fatal(rawMarshalErr)
	}
	f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "reset", CreatedAt: time.Now(), Body: "merge-progress-v1: " + string(raw)})
	l.loseResult = false
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err == nil || smokes != 1 {
		t.Fatalf("intent reset replayed smoke: err=%v smokes=%d", err, smokes)
	}
}
