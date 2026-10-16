package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestExecutorTransitionLog(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "normal"
		if resume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			e, o, _, _, _, ticket := fixture(t, "new-sha")
			var logs bytes.Buffer
			e.Stderr = &logs
			var result Result
			var err error
			if resume {
				var envelope struct {
					Data struct {
						Issue struct {
							Comments struct{ Nodes []task.Comment }
						}
					}
				}
				if err := json.Unmarshal(recorded(t, "linear-comments-executor-handoff.json"), &envelope); err != nil {
					t.Fatal(err)
				}
				// The human requeued the recorded ticket and the scheduler reclaimed it.
				requeued := task.Comment{Body: "阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=2", CreatedAt: time.Date(2026, 10, 5, 14, 20, 0, 0, time.UTC)}
				e.Issues = &logRecordedIssues{comments: append(envelope.Data.Issue.Comments.Nodes, requeued)}
				if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one"}); err != nil {
					t.Fatal(err)
				}
				result, err = e.Start(context.Background(), ticket)
			} else {
				result, err = e.Execute(context.Background(), ticket)
			}
			if err != nil || result.State.Phase != machine.PhaseMergeQueue {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			var events, phases []string
			var effects []any
			starts, ends := map[string]int{}, map[string]int{}
			active := map[string]bool{}
			snapshot, recovered := false, false
			for _, r := range transitionLogRecords(t, &logs) {
				switch r["msg"] {
				case "transition":
					events = append(events, r["event"].(string))
					phases = append(phases, r["from"].(string)+" -> "+r["to"].(string))
					effects = append(effects, r["effects"])
				case "external.start":
					step := r["step"].(string)
					if active[step] {
						t.Fatalf("unpaired start: %v", r)
					}
					active[step] = true
					starts[step]++
				case "external.end":
					step := r["step"].(string)
					if !active[step] || r["result"] != "ok" {
						t.Fatalf("invalid end: %v", r)
					}
					active[step] = false
					ends[step]++
					if duration, ok := r["duration_ms"].(float64); !ok || duration < 0 {
						t.Fatalf("duration: %v", r)
					}
				case "recovery.snapshot":
					want := map[string]any{"Run": "run_one", "Task": "task_one", "Dispatch": "ctx_one", "Pending": false, "Completed": false}
					if !reflect.DeepEqual(r["checkpoint"], want) || r["ticket_phase"] != string(machine.PhaseWorker) || r["ticket_owner"] != "执行器 term=- run=- attempt=1 scheduler-attempt=2" {
						t.Fatalf("snapshot: %v", r)
					}
					snapshot = true
				case "recovery.path":
					if r["path"] != "completed-worker" {
						t.Fatalf("path: %v", r)
					}
					recovered = true
				}
			}
			wantEvents := []string{"claim", "workerOk", "branchOk", "gateGreen", "reviewGreen", "openPr"}
			wantPhases := []string{"待领 -> worker 运行中", "worker 运行中 -> 分支检查", "分支检查 -> gate", "gate -> 审查", "审查 -> 开 PR", "开 PR -> 排队合并"}
			wantEffects := []any{
				[]any{"label-auto-impl-then-read-back", "orca-terminal-create-executor", "write-ownership owner=执行器 attempt=1 note=claim", "worker-start-with-request-id"},
				[]any{"worker-release"}, []any{}, []any{"run-reviewer-with-diff-on-stdin"}, []any{},
				[]any{"open-pr-rest-idempotent-by-branch", "write-ownership owner=合并通道 attempt=1 note=handoff"},
			}
			if !reflect.DeepEqual(events, wantEvents) || !reflect.DeepEqual(phases, wantPhases) || !reflect.DeepEqual(effects, wantEffects) {
				t.Fatalf("events=%v phases=%v effects=%v", events, phases, effects)
			}
			wantSteps := map[string]int{"task-list": 1, "gate": 1, "reviewer": 1, "open-pr": 1}
			if !resume {
				wantSteps["worker-start"] = 1
			}
			if !reflect.DeepEqual(starts, wantSteps) || !reflect.DeepEqual(ends, wantSteps) {
				t.Fatalf("starts=%v ends=%v want=%v", starts, ends, wantSteps)
			}
			if resume && (!snapshot || !recovered || len(o.workerRequests) != 0) {
				t.Fatalf("snapshot=%v recovered=%v starts=%v", snapshot, recovered, o.workerRequests)
			}
		})
	}
	t.Run("gate failure", func(t *testing.T) {
		e, _, tools, _, _, ticket := recoveryFixture(t)
		tools.gateErr = errors.New("private remote error ghp_secretvalue")
		e.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
		var logs bytes.Buffer
		e.Stderr = &logs
		result, err := e.Execute(context.Background(), ticket)
		if !errors.Is(err, context.Canceled) || result.State.Phase != machine.PhaseGate {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		failures, gateEnds := 0, 0
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "executor failed" {
				failures++
				if r["level"] != "ERROR" || r["phase"] != "gate" || r["error"] != "context canceled" {
					t.Fatalf("failure: %v", r)
				}
			}
			if r["msg"] == "external.end" && r["step"] == "gate" {
				gateEnds++
				if r["result"] != "error" {
					t.Fatalf("gate end: %v", r)
				}
				if duration, ok := r["duration_ms"].(float64); !ok || duration < 0 {
					t.Fatalf("duration: %v", r)
				}
			}
		}
		if failures != 1 || gateEnds != 1 || strings.Contains(logs.String(), "private remote error") || strings.Contains(logs.String(), "ghp_secretvalue") {
			t.Fatalf("failures=%d ends=%d logs=%s", failures, gateEnds, logs.String())
		}
		e, _, tools, _, _, ticket = recoveryFixture(t)
		tools.gateReds = 1
		logs.Reset()
		e.Stderr = &logs
		result, err = e.Execute(context.Background(), ticket)
		if err != nil || result.State.Phase != machine.PhaseMergeQueue {
			t.Fatalf("correction result=%+v err=%v", result, err)
		}
		corrections := 0
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "correction" {
				corrections++
				if r["level"] != "INFO" || r["reason"] != "gate red" || r["fail_summary"] != "FAIL: approach=head=new-sha files=unit/change.go | reason=gate red | evidence=gate" {
					t.Fatalf("correction: %v", r)
				}
			}
		}
		if corrections != 1 {
			t.Fatalf("corrections=%d logs=%s", corrections, logs.String())
		}
	})
}

func transitionLogRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("invalid log %q: %v", line, err)
		}
		records = append(records, r)
	}
	return records
}

type logRecordedIssues struct {
	fakeIssues
	comments []task.Comment
}

func (l *logRecordedIssues) ListComments(context.Context, string) ([]task.Comment, error) {
	return l.comments, nil
}

func TestExecutorWarningLogRedactsCredentials(t *testing.T) {
	e, o, _, _, _, ticket := fixture(t, "new-sha")
	o.releaseErr = errors.New("release failed with ghp_privatecredential")
	var logs bytes.Buffer
	e.Stderr = &logs
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Phase != machine.PhaseMergeQueue {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if strings.Contains(logs.String(), "ghp_privatecredential") {
		t.Fatal("warning leaked credential")
	}
	warnings := 0
	for _, record := range transitionLogRecords(t, &logs) {
		if record["level"] == "WARN" {
			warnings++
			if record["component"] != "executor" || !strings.Contains(record["msg"].(string), "[REDACTED]") {
				t.Fatalf("warning=%v", record)
			}
		}
	}
	if warnings != 1 {
		t.Fatalf("warnings=%d", warnings)
	}
}

func TestExecutorContextLog(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "resume"}[resume], func(t *testing.T) {
			e, _, _, _, _, ticket := fixture(t, "new-sha")
			var fallback bytes.Buffer
			e.Stderr = &fallback
			path := filepath.Join(t.TempDir(), "executor.log")
			sink, err := logx.RotatingFile(path, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := sink.Close(); err != nil {
					t.Error(err)
				}
			})
			ctx := runx.WithLogger(context.Background(), logx.New("executor", sink).With("ticket", ticket.Identifier))
			if resume {
				if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one"}); err != nil {
					t.Fatal(err)
				}
				_, err = e.Start(ctx, ticket)
			} else {
				_, err = e.Execute(ctx, ticket)
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			events := []string{`"msg":"transition"`, `"msg":"external.end"`, `"ticket":"` + ticket.Identifier + `"`}
			if resume {
				events = append(events, `"msg":"recovery.snapshot"`, `"msg":"recovery.path"`)
			}
			for _, event := range events {
				if !bytes.Contains(data, []byte(event)) {
					t.Errorf("rotating log missing %s: %s", event, data)
				}
			}
			if fallback.Len() != 0 {
				t.Errorf("context logs bypassed sink: %s", &fallback)
			}
		})
	}
}
