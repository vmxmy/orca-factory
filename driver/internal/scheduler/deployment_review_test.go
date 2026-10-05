package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type movingDeploymentIntent struct {
	*mergeLinear
	move func()
}

func (l *movingDeploymentIntent) CreateComment(ctx context.Context, id, body string) (linear.Comment, error) {
	c, err := l.mergeLinear.CreateComment(ctx, id, body)
	if err == nil && strings.Contains(body, `"phase":"merge-intent"`) && l.move != nil {
		move := l.move
		l.move = nil
		move()
	}
	return c, err
}

func TestDeploymentRetryKeepsFrozenIntentConfiguration(t *testing.T) {
	for _, change := range []string{"replace", "remove", "add"} {
		t.Run(change, func(t *testing.T) {
			f := newMergeFixture(t)
			env := f.config.Repositories[0].Env
			if change != "add" {
				env["AGENT_SMOKE_CMD"] = "original-smoke"
			}
			dir := f.config.Repositories[0].Path
			gitMergeTest(t, dir, "checkout", "main")
			if err := os.WriteFile(filepath.Join(dir, "main-change"), []byte("main moved"), 0600); err != nil {
				t.Fatal(err)
			}
			gitMergeTest(t, dir, "add", ".")
			gitMergeTest(t, dir, "commit", "-m", "advance main")
			nextMain := gitMergeTest(t, dir, "rev-parse", "HEAD")
			l := &movingDeploymentIntent{mergeLinear: f.linear, move: func() { f.main = nextMain }}
			f.runtime.Reader = l
			f.runtime.Clients[dir] = l
			var smokes []string
			f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if s.Path == "bash" {
					smokes = append(smokes, s.Args[1])
					return nil, nil
				}
				return runx.Run(ctx, s)
			}
			if err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background()); err == nil || f.puts != 0 || !strings.Contains(strings.Join(f.linear.bodies, "\n"), `"phase":"merge-intent"`) {
				t.Fatalf("expected durable intent followed by moved main: %v merges=%d receipts=%v", err, f.puts, f.linear.bodies)
			}
			if change == "remove" {
				delete(env, "AGENT_SMOKE_CMD")
			} else {
				env["AGENT_SMOKE_CMD"] = "replacement-smoke"
			}
			for i := 0; i < 2; i++ {
				rebuilt := *f.runtime
				rebuilt.GitHub = nil
				if err := (&Scheduler{Config: f.config, Backend: &rebuilt}).Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			want := "original-smoke"
			if change == "add" {
				want = ""
			}
			for _, body := range f.linear.bodies {
				for _, line := range strings.Split(body, "\n") {
					if strings.HasPrefix(line, merge.ProgressPrefix) {
						var p merge.Progress
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, merge.ProgressPrefix)), &p); err != nil {
							t.Fatal(err)
						}
						got := ""
						if p.Deployment != nil {
							got = p.Deployment.Smoke
						}
						if got != want {
							t.Fatalf("persisted intent policy changed: got=%q want=%q", got, want)
						}
					}
				}
			}
			if f.puts != 1 || strings.Join(smokes, ",") != want || f.linear.issue.State.Name != "Done" {
				t.Fatalf("wrong execution after restart: merges=%d smoke=%v state=%s", f.puts, smokes, f.linear.issue.State.Name)
			}
		})
	}
}

type deploymentFailureTransport struct {
	base    http.RoundTripper
	failure error
}

func (d deploymentFailureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/actions/") {
		return nil, d.failure
	}
	return d.base.RoundTrip(r)
}
func TestDeploymentQueryPreservesSafeErrorClassification(t *testing.T) {
	for _, kind := range []string{"cancel", "timeout", "global"} {
		t.Run(kind, func(t *testing.T) {
			f := newMergeFixture(t)
			f.merged = true
			f.config.Repositories[0].Env["AGENT_DEPLOY_WORKFLOW"] = "deploy.yml"
			if kind == "global" {
				original := f.server.Config.Handler
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/actions/") {
						w.WriteHeader(401)
						return
					}
					original.ServeHTTP(w, r)
				})
			} else {
				cause := context.Canceled
				if kind == "timeout" {
					cause = context.DeadlineExceeded
				}
				http.DefaultTransport = deploymentFailureTransport{base: http.DefaultTransport, failure: fmt.Errorf("private-token: %w", cause)}
			}
			err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
			if kind == "global" {
				var global *infra.Global
				if !errors.As(err, &global) || global.Dep != infra.GitHub {
					t.Fatalf("lost global classification: %v", err)
				}
			} else {
				cause := context.Canceled
				if kind == "timeout" {
					cause = context.DeadlineExceeded
				}
				if !errors.Is(err, cause) {
					t.Fatalf("lost cancellation/timeout: %v", err)
				}
			}
			if strings.Contains(fmt.Sprint(err), "private-token") {
				t.Fatal("credential leaked")
			}
		})
	}
}
func TestDeploymentSmokePreservesSafeErrorAndNeverReplays(t *testing.T) {
	for _, kind := range []string{"cancel", "timeout", "global"} {
		t.Run(kind, func(t *testing.T) {
			f := newMergeFixture(t)
			f.merged = true
			f.config.Repositories[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			count := 0
			f.runtime.Run = func(context.Context, runx.Spec) ([]byte, error) {
				count++
				switch kind {
				case "cancel":
					cancel()
					return nil, fmt.Errorf("private-token: %w", context.Canceled)
				case "timeout":
					return nil, fmt.Errorf("private-token: %w", context.DeadlineExceeded)
				default:
					return nil, &infra.Global{Dep: infra.GitHub, Err: errors.New("private-token")}
				}
			}
			err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(ctx)
			if kind == "global" {
				var global *infra.Global
				if !errors.As(err, &global) || global.Dep != infra.GitHub {
					t.Fatalf("lost global classification: %v", err)
				}
				if strings.Contains(fmt.Sprint(global.Err), "private-token") {
					t.Fatal("unsafe global cause exposed")
				}
			} else {
				cause := context.Canceled
				if kind == "timeout" {
					cause = context.DeadlineExceeded
				}
				if !errors.Is(err, cause) {
					t.Fatalf("lost cancellation/timeout: %v", err)
				}
			}
			if strings.Contains(fmt.Sprint(err), "private-token") {
				t.Fatal("credential leaked")
			}
			for i := 0; i < 2; i++ {
				rebuilt := *f.runtime
				rebuilt.GitHub = nil
				_ = (&Scheduler{Config: f.config, Backend: &rebuilt}).Tick(context.Background())
			}
			if count != 1 || f.linear.issue.State.Name == "Done" {
				t.Fatalf("unknown smoke replayed/released: count=%d state=%s", count, f.linear.issue.State.Name)
			}
		})
	}
}

func TestDeploymentHistoryRequiresHumanRecovery(t *testing.T) {
	for _, from := range []string{"smoke-intent", "paused"} {
		for _, to := range []string{"pending", "passed"} {
			for _, proof := range []string{"absent", "missing-operator", "missing-reason", "missing-time", "wrong-action"} {
				t.Run(from+"/"+to+"/"+proof, func(t *testing.T) {
					f := newMergeFixture(t)
					f.merged = true
					f.config.Repositories[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
					l := &uncertainSmokeLinear{mergeLinear: f.linear, loseResult: true}
					f.runtime.Clients[f.config.Repositories[0].Path] = l
					count := 0
					f.runtime.Run = func(context.Context, runx.Spec) ([]byte, error) { count++; return nil, nil }
					_ = (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
					l.loseResult = false
					if from == "paused" {
						_ = (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
					}
					tickets, err := f.runtime.Snapshot(context.Background(), f.config.Repositories)
					if err != nil || len(tickets) != 1 {
						t.Fatalf("barrier missing: %v %v", tickets, err)
					}
					p := tickets[0].Progress
					p.Deployment.State = to
					if to == "pending" {
						p.Deployment.SmokeStarted = false
					}
					if proof != "absent" {
						action := "retry-smoke"
						if to == "passed" {
							action = "confirm-passed"
						}
						p.Deployment.Recovery = &merge.DeploymentRecovery{Action: action, Operator: "Alice", Reason: "reconciled", At: time.Now()}
						switch proof {
						case "missing-operator":
							p.Deployment.Recovery.Operator = ""
						case "missing-reason":
							p.Deployment.Recovery.Reason = ""
						case "missing-time":
							p.Deployment.Recovery.At = time.Time{}
						case "wrong-action":
							p.Deployment.Recovery.Action = "retry-workflow"
						}
					}
					raw, _ := json.Marshal(p)
					f.linear.rows = append(f.linear.rows, linear.Comment{ID: "unauthorized", CreatedAt: time.Now(), Body: merge.ProgressPrefix + string(raw)})
					rebuilt := *f.runtime
					rebuilt.GitHub = nil
					err = (&Scheduler{Config: f.config, Backend: &rebuilt}).Tick(context.Background())
					if err == nil || count != 1 || f.linear.issue.State.Name == "Done" {
						t.Fatalf("unauthorized %s -> %s accepted: err=%v smoke=%d state=%s", from, to, err, count, f.linear.issue.State.Name)
					}
				})
			}
		}
	}
}

func TestDeploymentOldRecoveryCannotAuthorizeAnotherSmoke(t *testing.T) {
	f := newMergeFixture(t)
	f.merged = true
	f.config.Repositories[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
	count := 0
	f.runtime.Run = func(context.Context, runx.Spec) ([]byte, error) { count++; return nil, errors.New("smoke failed") }
	_ = (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
	if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", strings.Repeat("a", 40), "retry-smoke", "Alice", "side effects reconciled", false); err != nil {
		t.Fatal(err)
	}
	_ = (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
	tickets, err := f.runtime.Snapshot(context.Background(), f.config.Repositories)
	if err != nil || len(tickets) != 1 {
		t.Fatalf("missing barrier: %v", err)
	}
	p := tickets[0].Progress
	if p.Deployment.Recovery == nil || count != 2 {
		t.Fatalf("first authorized retry failed: smoke=%d", count)
	}
	p.Deployment.State = "pending"
	p.Deployment.SmokeStarted = false
	raw, _ := json.Marshal(p)
	f.linear.rows = append(f.linear.rows, linear.Comment{ID: "replayed-proof", CreatedAt: time.Now(), Body: merge.ProgressPrefix + string(raw)})
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	if err := (&Scheduler{Config: f.config, Backend: &rebuilt}).Tick(context.Background()); err == nil || count != 2 {
		t.Fatalf("old recovery reused: err=%v smoke=%d", err, count)
	}
}

func TestDeploymentRecoveryRejectsStaleTimestampBeforeWriting(t *testing.T) {
	f := newMergeFixture(t)
	f.merged = true
	f.config.Repositories[0].Env["AGENT_SMOKE_CMD"] = "smoke-test"
	now := time.Now()
	f.runtime.Now = func() time.Time { return now }
	f.runtime.Run = func(context.Context, runx.Spec) ([]byte, error) { return nil, errors.New("smoke failed") }
	_ = (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
	if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", strings.Repeat("a", 40), "retry-smoke", "Alice", "first reconciliation", false); err != nil {
		t.Fatal(err)
	}
	_ = (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
	before := len(f.linear.bodies)
	if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", strings.Repeat("a", 40), "retry-smoke", "Alice", "second reconciliation", false); err == nil {
		t.Fatal("stale recovery time accepted")
	}
	if len(f.linear.bodies) != before {
		t.Fatal("invalid recovery poisoned durable history")
	}
	now = now.Add(time.Second)
	if err := f.runtime.ResumeDeployment(context.Background(), "ENG-1", "q1", strings.Repeat("a", 40), "retry-smoke", "Alice", "second reconciliation", false); err != nil {
		t.Fatal(err)
	}
}
