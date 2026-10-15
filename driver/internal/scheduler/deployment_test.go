package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
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
	b := *f.linear
	b.issue.ID = "b"
	b.issue.Project.ID = "p-b"
	b.rows = append([]linear.Comment(nil), f.linear.rows...)
	bf := *f
	bf.linear = &b
	rewriteHandoff(t, &bf, func(h map[string]any) { h["repository"] = "o/s"; h["ticket_id"] = "b"; h["queue_id"] = "b" })
	dir := t.TempDir()
	gitMergeTest(t, dir, "clone", f.repos[0].Path, ".")
	br := config.Repository{Path: dir, StaleHours: 8, MaxInFlight: 3, Env: map[string]string{"AGENT_LINEAR_PROJECT": "p-b", "AGENT_GH_REPO": "o/s", "AGENT_SCOPE_AVOID": "0"}}
	f.repos = append(f.repos, br)
	f.runtime.Repositories[dir] = br
	source := &mergeTickets{rows: []*mergeLinear{f.linear, &b}}
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
	if err == nil || f.puts != 1 || bPuts != 1 || b.issue.State.Name != "Done" || f.linear.issue.State.Name != "In Progress" {
		t.Fatalf("repository isolation failed: err=%v a=%d b=%d state=%s", err, f.puts, bPuts, b.issue.State.Name)
	}
	// An operator may mark the issue Done or transfer ownership while waiting.
	f.linear.issue.State.Name = "Done"
	f.linear.rows = append(f.linear.rows, linear.Comment{ID: "operator", CreatedAt: time.Now(), Body: "归属: 无"})
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
	if smokes != 1 || f.puts != 1 || !strings.Contains(strings.Join(f.linear.bodies, "\n"), `"state":"passed"`) {
		t.Fatalf("deployment not completed: smoke=%d merges=%d receipts=%v", smokes, f.puts, f.linear.bodies)
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
	if !strings.Contains(strings.Join(f.linear.bodies, "\n"), `"state":"paused"`) || !strings.Contains(strings.Join(f.linear.bodies, "\n"), "归属: 处置Agent") {
		t.Fatalf("failure not durably escalated: %v", f.linear.bodies)
	}
	conclusion = "success"
	f.linear.issue.State.Name = "Done"
	for i := 0; i < 2; i++ {
		rebuilt := *f.runtime
		rebuilt.GitHub = nil
		if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err == nil {
			t.Fatal("restart/credential recovery bypassed human confirmation")
		}
	}
}

type uncertainSmokeLinear struct {
	*mergeLinear
	loseResult bool
}

func (l *uncertainSmokeLinear) CreateComment(ctx context.Context, id, body string) (linear.Comment, error) {
	if l.loseResult && strings.Contains(body, `"state":"passed"`) {
		return linear.Comment{}, fmt.Errorf("lost result")
	}
	return l.mergeLinear.CreateComment(ctx, id, body)
}
func TestDeploymentSmokeUnconfirmedNeverReplaysAndCanBeReconciled(t *testing.T) {
	f := newMergeFixture(t)
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
	l := &uncertainSmokeLinear{mergeLinear: f.linear, loseResult: true}
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
	if smokes != 1 || !strings.Contains(strings.Join(f.linear.bodies, "\n"), `"state":"paused"`) {
		t.Fatalf("uncertain smoke replayed or not escalated: %d %v", smokes, f.linear.bodies)
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
	before := len(f.linear.bodies)
	if err := recovery.ResumeDeployment(context.Background(), "ENG-1", "wrong", strings.Repeat("a", 40), "confirm-passed", "operator", "checked production", false); err == nil {
		t.Fatal("stale identity accepted")
	}
	if err := recovery.ResumeDeployment(context.Background(), "ENG-1", "q1", strings.Repeat("a", 40), "confirm-passed", "operator", "checked production", true); err != nil {
		t.Fatal(err)
	}
	if len(f.linear.bodies) != before || smokes != 1 {
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
	if smokes != 1 || f.linear.issue.State.Name != "Done" || !strings.Contains(strings.Join(f.linear.bodies, "\n"), `"action":"confirm-passed"`) {
		t.Fatalf("manual confirmation lost/replayed: smoke=%d %v", smokes, f.linear.bodies)
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
			if err := json.Unmarshal([]byte(strings.TrimPrefix(f.linear.bodies[len(f.linear.bodies)-1], "merge-progress-v1: ")), &p); err != nil {
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
			f.linear.rows = append(f.linear.rows, linear.Comment{ID: "damaged", CreatedAt: time.Now(), Body: "归属: 无\nmerge-progress-v1: " + string(raw)})
			f.linear.issue.State.Name = "Done"
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
	l := &uncertainSmokeLinear{mergeLinear: f.linear, loseResult: true}
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
	next := *f.linear
	next.issue.ID = "next"
	next.issue.Identifier = "ENG-2"
	next.rows = append([]linear.Comment(nil), f.linear.rows[:1]...)
	next.bodies = nil
	nf := *f
	nf.linear = &next
	rewriteHandoff(t, &nf, func(h map[string]any) {
		h["ticket_id"] = "next"
		h["identifier"] = "ENG-2"
		h["pr"] = 2
		h["queue_id"] = "q2"
		h["enqueued_at"] = "2026-10-06T02:00:00Z"
	})
	source := &mergeTickets{rows: []*mergeLinear{&next, f.linear}}
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
	f.linear.issue.State.Name = "Done"
	f.linear.issue.Labels.Nodes = []linear.NamedID{{Name: "auto-failed"}}
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
			if f.puts != 1 || smokes != wantSmoke || f.linear.issue.State.Name != "Done" {
				t.Fatalf("retry failed: merges=%d smoke=%d state=%s", f.puts, smokes, f.linear.issue.State.Name)
			}
		})
	}
}

func TestDeploymentSmokeIntentCannotBeResetWithoutHumanReceipt(t *testing.T) {
	f := newMergeFixture(t)
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
	l := &uncertainSmokeLinear{mergeLinear: f.linear, loseResult: true}
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
	if err := json.Unmarshal([]byte(strings.TrimPrefix(f.linear.bodies[len(f.linear.bodies)-1], "merge-progress-v1: ")), &p); err != nil {
		t.Fatal(err)
	}
	p["deployment"].(map[string]any)["state"] = "pending"
	raw, rawMarshalErr := json.Marshal(p)
	if rawMarshalErr != nil {
		t.Fatal(rawMarshalErr)
	}
	f.linear.rows = append(f.linear.rows, linear.Comment{ID: "reset", CreatedAt: time.Now(), Body: "merge-progress-v1: " + string(raw)})
	l.loseResult = false
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err == nil || smokes != 1 {
		t.Fatalf("intent reset replayed smoke: err=%v smokes=%d", err, smokes)
	}
}
