package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

func TestExecutorTransitionLog(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "normal"
		if resume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			e, _, l, _, _, ticket := fixture(t, "new-sha")
			var logs bytes.Buffer
			e.Stderr = &logs
			var result Result
			var err error
			if resume {
				l.comments = []string{"阶段: worker 运行中\n归属: 执行器 term=old run=run_one"}
				if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Pending: false, Completed: false}); err != nil {
					t.Fatal(err)
				}
				result, err = e.Start(context.Background(), ticket)
			} else {
				result, err = e.Execute(context.Background(), ticket)
			}
			if err != nil || result.State.Phase != machine.PhaseMergeQueue {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			var events []string
			var phases []string
			starts, ends := map[string]int{}, map[string]int{}
			recovered, snapshot := false, false
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var r map[string]any
				if err := json.Unmarshal([]byte(line), &r); err != nil {
					t.Fatalf("invalid log %q: %v", line, err)
				}
				switch r["msg"] {
				case "transition":
					events = append(events, r["event"].(string))
					phases = append(phases, r["from"].(string)+" -> "+r["to"].(string))
					if r["event"] == "workerOk" && !reflect.DeepEqual(r["effects"], []any{"worker-release"}) {
						t.Fatalf("worker effects=%v", r["effects"])
					}
					if r["from"] == nil || r["to"] == nil || r["effects"] == nil {
						t.Fatalf("incomplete transition: %v", r)
					}
				case "external.start":
					starts[r["step"].(string)]++
				case "external.end":
					ends[r["step"].(string)]++
					duration, ok := r["duration_ms"].(float64)
					if !ok || duration < 0 || r["result"] == nil {
						t.Fatalf("incomplete external result: %v", r)
					}
				case "recovery.snapshot":
					cp, ok := r["checkpoint"].(map[string]any)
					snapshot = ok && cp["Run"] == "run_one" && cp["Task"] == "task_one" && cp["Dispatch"] == "ctx_one" && cp["Pending"] == false && cp["Completed"] == false && r["linear_phase"] == "worker 运行中" && r["linear_owner"] == "执行器 term=old run=run_one"
				case "recovery.path":
					recovered = r["path"] == "completed-worker"
				}
			}
			if want := []string{"claim", "workerOk", "branchOk", "gateGreen", "reviewGreen", "openPr"}; !reflect.DeepEqual(events, want) {
				t.Fatalf("events=%v want=%v", events, want)
			}
			if want := []string{"待领 -> worker 运行中", "worker 运行中 -> 分支检查", "分支检查 -> gate", "gate -> 审查", "审查 -> 开 PR", "开 PR -> 排队合并"}; !reflect.DeepEqual(phases, want) {
				t.Fatalf("phases=%v want=%v", phases, want)
			}
			if !reflect.DeepEqual(starts, ends) {
				t.Fatalf("starts=%v ends=%v", starts, ends)
			}
			for _, step := range []string{"task-list", "gate", "reviewer", "open-pr"} {
				if ends[step] < 1 {
					t.Errorf("missing %s timing", step)
				}
			}
			if !resume && ends["worker-start"] != 1 {
				t.Error("missing worker-start timing")
			}
			if resume && (!snapshot || !recovered) {
				t.Fatalf("recovery snapshot=%v path=%v logs=%s", snapshot, recovered, logs.String())
			}
		})
	}
}

func TestExecutorLogErrorsAndCorrection(t *testing.T) {
	t.Run("invalid claim", func(t *testing.T) {
		var logs bytes.Buffer
		e := &Executor{Stderr: &logs}
		_, err := e.Execute(context.Background(), Ticket{})
		if err == nil || !strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), `"phase":"待领"`) {
			t.Fatalf("err=%v logs=%s", err, logs.String())
		}
	})
	t.Run("invalid recovery", func(t *testing.T) {
		var logs bytes.Buffer
		e := &Executor{Stderr: &logs}
		_, err := e.Start(context.Background(), Ticket{})
		if err == nil || !strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), `"phase":"recovery"`) {
			t.Fatalf("err=%v logs=%s", err, logs.String())
		}
	})
	t.Run("correction", func(t *testing.T) {
		e, _, tools, _, _, ticket := recoveryFixture(t)
		tools.gateReds = 1
		var logs bytes.Buffer
		e.Stderr = &logs
		_, err := e.Execute(context.Background(), ticket)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), `"reason":"gateRed"`) || !strings.Contains(logs.String(), `"fail_summary":"FAIL: approach=`) || strings.Count(logs.String(), `"msg":"external.end","component":"executor","step":"worker-start"`) != 2 {
			t.Fatalf("logs=%s", logs.String())
		}
	})
}

func TestRecoveryLoggingPreservesExistingPR(t *testing.T) {
	e, _, _, g, _, ticket := fixture(t, "new-sha")
	g.existing = &github.PullRequest{Number: 42, HTMLURL: "https://example.test/pr/42"}
	if err := saveCheckpoint(e.Config, ticket, checkpoint{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointPath(e.Config, ticket), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	e.Stderr = &logs
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.PR.Number != 42 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
