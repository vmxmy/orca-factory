package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

func TestExecutorTransitionLog(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "normal"
		if resume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			e, o, l, _, _, ticket := fixture(t, "new-sha")
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
			var transitions []map[string]any
			active := map[string]bool{}
			starts, ends := map[string]int{}, map[string]int{}
			recovered, snapshot := false, false
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var r map[string]any
				if err := json.Unmarshal([]byte(line), &r); err != nil {
					t.Fatalf("invalid log %q: %v", line, err)
				}
				if r["ticket"] != ticket.Identifier || r["terminal"] != e.Config.Terminal {
					t.Errorf("uncorrelated record: %v", r)
				}
				switch r["msg"] {
				case "transition":
					transitions = append(transitions, r)
					if r["record_kind"] != "decision" || r["decision_kind"] != "execution" || r["allowed"] != true || r["applied"] != false || r["actual_phase"] != r["from"] {
						t.Errorf("decision claims execution: %v", r)
					}
					events = append(events, r["event"].(string))
					phases = append(phases, r["from"].(string)+" -> "+r["to"].(string))
					if r["event"] == "workerOk" && !reflect.DeepEqual(r["effects"], []any{"worker-release"}) {
						t.Fatalf("worker effects=%v", r["effects"])
					}
					if r["from"] == nil || r["to"] == nil || r["effects"] == nil {
						t.Fatalf("incomplete transition: %v", r)
					}
				case "external.start":
					step := r["step"].(string)
					if active[step] {
						t.Errorf("unpaired start: %v", r)
					}
					active[step] = true
					if r["run"] != "run_one" {
						t.Errorf("missing run: %v", r)
					}
					starts[step]++
				case "external.end":
					step := r["step"].(string)
					if !active[step] {
						t.Errorf("end without start: %v", r)
					}
					active[step] = false
					if r["result"] != "ok" || r["error"] != nil || r["run"] != "run_one" {
						t.Errorf("wrong actual result: %v", r)
					}
					if step != "task-list" && (r["task"] != "task_one" || r["dispatch"] != "ctx_one") {
						t.Errorf("missing worker association: %v", r)
					}
					switch step {
					case "gate":
						if r["verdict"] != "green" || r["event"] != "gateGreen" || r["command_result"] != "ok" || r["parse_error"] != nil {
							t.Errorf("gate result: %v", r)
						}
					case "reviewer":
						if r["verdict"] != "green" || r["event"] != "reviewGreen" || r["tier"] != "default" || r["command_result"] != "ok" || r["parse_error"] != nil {
							t.Errorf("review result: %v", r)
						}
					case "task-list":
						if r["count"] != float64(1) {
							t.Errorf("task result: %v", r)
						}
					case "open-pr":
						if r["number"] != float64(42) {
							t.Errorf("PR result: %v", r)
						}
					}
					ends[step]++
					duration, ok := r["duration_ms"].(float64)
					if !ok || duration < 0 || r["result"] == nil {
						t.Fatalf("incomplete external result: %v", r)
					}
				case "recovery.snapshot":
					cp, ok := r["checkpoint"].(map[string]any)
					snapshot = ok && cp["Run"] == "run_one" && cp["Task"] == "task_one" && cp["Dispatch"] == "ctx_one" && cp["Pending"] == false && cp["Completed"] == false && r["linear_phase"] == "worker 运行中" && r["linear_owner"] == "执行器 term=old run=run_one"
				case "recovery.path":
					cp, ok := r["checkpoint"].(map[string]any)
					recovered = r["path"] == "completed-worker" && r["basis"] == "worker-report-succeeded" && ok && cp["Run"] == "run_one" && cp["Task"] == "task_one" && cp["Dispatch"] == "ctx_one" && cp["Completed"] == true && cp["Pending"] == false && cp["Settled"] == true
				}
			}
			if want := []string{"claim", "workerOk", "branchOk", "gateGreen", "reviewGreen", "openPr"}; !reflect.DeepEqual(events, want) {
				t.Fatalf("events=%v want=%v", events, want)
			}
			if want := []string{"待领 -> worker 运行中", "worker 运行中 -> 分支检查", "分支检查 -> gate", "gate -> 审查", "审查 -> 开 PR", "开 PR -> 排队合并"}; !reflect.DeepEqual(phases, want) {
				t.Fatalf("phases=%v want=%v", phases, want)
			}
			wantEffects := [][]any{
				{"label-auto-impl-then-read-back", "orca-terminal-create-executor", "write-ownership owner=执行器 attempt=1 note=claim", "worker-start-with-request-id"},
				{"worker-release"}, {}, {"run-reviewer-with-diff-on-stdin"}, {},
				{"open-pr-rest-idempotent-by-branch", "write-ownership owner=合并通道 attempt=1 note=handoff"},
			}
			for i, r := range transitions {
				if !reflect.DeepEqual(r["effects"], wantEffects[i]) {
					t.Errorf("transition %d effects=%v want=%v", i, r["effects"], wantEffects[i])
				}
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
			if resume && (ends["worker-start"] != 0 || len(o.workerRequests) != 0) {
				t.Errorf("recovery restarted worker: %v", o.workerRequests)
			}
			if ends["task-list"] != 1 {
				t.Errorf("task-list calls=%d", ends["task-list"])
			}
			for _, step := range []string{"gate", "reviewer", "open-pr"} {
				if ends[step] != 1 {
					t.Errorf("%s calls=%d", step, ends[step])
				}
			}
			if resume && (!snapshot || !recovered) {
				t.Fatalf("recovery snapshot=%v path=%v logs=%s", snapshot, recovered, logs.String())
			}
		})
	}
	t.Run("external failure is an error record", func(t *testing.T) {
		e, _, _, _, _, ticket := fixture(t, "new-sha")
		e.Orca = &missingLogReceiptOrca{}
		var logs bytes.Buffer
		e.Stderr = &logs
		result, err := e.Execute(context.Background(), ticket)
		if err == nil || result.State.Phase != machine.PhaseUnclaimed {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		found := false
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "external.end" && r["step"] == "worker-start" {
				found = true
				if r["level"] != "ERROR" || r["result"] != "error" || r["error"] == nil {
					t.Errorf("failed external step must be ERROR: %v", r)
				}
			}
		}
		if !found {
			t.Fatalf("missing failed external step: %s", logs.String())
		}
	})
	t.Run("recovery retains checkpoint correction count", func(t *testing.T) {
		e, o, l, _, _, ticket := fixture(t, "new-sha")
		l.comments = []string{"阶段: worker 运行中\n归属: 执行器 term=old run=run_one"}
		cp := checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Corrections: 1}
		if err := saveCheckpoint(e.Config, ticket, cp); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		e.Stderr = &logs
		result, err := e.Start(context.Background(), ticket)
		if err != nil || result.State.Owner != machine.OwnerMerge || len(o.workerRequests) != 0 {
			t.Fatalf("result=%+v err=%v starts=%v", result, err, o.workerRequests)
		}
		snapshot, path := false, false
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] != "recovery.snapshot" && r["msg"] != "recovery.path" {
				continue
			}
			evidence, ok := r["checkpoint"].(map[string]any)
			if !ok || evidence["Corrections"] != float64(1) {
				t.Errorf("missing checkpoint correction count: %v", r)
			}
			if r["msg"] == "recovery.snapshot" {
				snapshot = true
				if r["linear_phase"] != "worker 运行中" || r["linear_owner"] != "执行器 term=old run=run_one" {
					t.Errorf("missing Linear recovery evidence: %v", r)
				}
			} else {
				path = true
				if r["path"] != "completed-worker" || r["basis"] != "worker-report-succeeded" {
					t.Errorf("wrong recovery path: %v", r)
				}
			}
		}
		if !snapshot || !path {
			t.Fatalf("missing recovery records: %s", logs.String())
		}
	})
	t.Run("invalid reviewer output", func(t *testing.T) {
		e, o, tools, _, _, ticket := recoveryFixture(t)
		tools.emptyReviews = 1
		e.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
		var logs bytes.Buffer
		e.Stderr = &logs
		result, err := e.Execute(context.Background(), ticket)
		if !errors.Is(err, context.Canceled) || result.State.Phase != machine.PhaseReview || tools.reviewCalls != 1 || len(o.starts) != 1 {
			t.Fatalf("result=%+v err=%v review=%d starts=%d", result, err, tools.reviewCalls, len(o.starts))
		}
		records := transitionLogRecords(t, &logs)
		ends, failures, decisions := 0, 0, 0
		for _, r := range records {
			if r["msg"] == "external.end" && r["step"] == "reviewer" {
				ends++
				if r["result"] != "error" || r["command_result"] != "ok" || r["call_error"] != nil || r["parse_error"] != "missing reviewer verdict JSON" || r["verdict"] != "" || r["event"] != "reviewEmpty" {
					t.Errorf("invalid output reported successful: %v", r)
				}
				if d, ok := r["duration_ms"].(float64); !ok || d < 0 {
					t.Errorf("duration: %v", r)
				}
			}
			if r["msg"] == "transition" && r["event"] == "reviewEmpty" {
				decisions++
				if r["from"] != "审查" || r["to"] != "审查" || r["actual_phase"] != "审查" || r["allowed"] != true || !reflect.DeepEqual(r["effects"], []any{"retry-step-after-backoff n=1"}) {
					t.Errorf("empty review decision: %v", r)
				}
			}
			if r["msg"] == "executor failed" {
				failures++
				if r["level"] != "ERROR" || r["phase"] != "审查" || r["error"] != "context canceled" || r["ticket"] != ticket.Identifier || r["run"] != "run_one" || r["task"] != "task_one" || r["dispatch"] != "ctx_one" {
					t.Errorf("wrong final failure: %v", r)
				}
			}
		}
		if ends != 1 || failures != 1 || decisions != 1 {
			t.Fatalf("ends=%d failures=%d decisions=%d", ends, failures, decisions)
		}
	})
	t.Run("budget preserves observed gate phase", func(t *testing.T) {
		e, _, tools, _, _, ticket := recoveryFixture(t)
		tools.gateErr = errors.New("remote private failure")
		e.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
		var logs bytes.Buffer
		e.Stderr = &logs
		result, err := e.Execute(context.Background(), ticket)
		if !errors.Is(err, context.Canceled) || result.State.Phase != machine.PhaseGate {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		found := false
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "transition" && r["event"] == "workerStall" {
				found = true
				if r["decision_kind"] != "budget" || r["actual_phase"] != "gate" || r["from"] != "worker 运行中" || r["to"] != "worker 运行中" || r["allowed"] != true || !reflect.DeepEqual(r["effects"], []any{"retry-step-after-backoff n=1"}) {
					t.Errorf("budget masquerades as execution: %v", r)
				}
			}
		}
		if !found || strings.Contains(logs.String(), "remote private failure") {
			t.Fatalf("missing budget or unsafe error: %s", logs.String())
		}
	})
	t.Run("missing worker receipt leaves claim unapplied", func(t *testing.T) {
		e, _, _, _, _, ticket := fixture(t, "new-sha")
		e.Orca = &missingLogReceiptOrca{}
		var logs bytes.Buffer
		e.Stderr = &logs
		result, err := e.Execute(context.Background(), ticket)
		if err == nil || result.State.Phase != machine.PhaseUnclaimed {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		end, failure := false, false
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "external.end" && r["step"] == "worker-start" {
				end = true
				if r["result"] != "error" || r["command_result"] != "ok" || r["error"] != "worker receipt missing task or dispatch" {
					t.Errorf("invalid receipt: %v", r)
				}
			}
			if r["msg"] == "executor failed" {
				failure = true
				if r["phase"] != "待领" || r["transition_applied"] != false || r["event"] != "claim" || r["decision_to"] != "worker 运行中" {
					t.Errorf("failed action claims transition: %v", r)
				}
			}
		}
		if !end || !failure {
			t.Fatalf("end=%v failure=%v", end, failure)
		}
	})

	t.Run("recovery raw checkpoint differs from reconciled association", func(t *testing.T) {
		e, o, _, _, _, ticket := fixture(t, "new-sha")
		raw := checkpoint{Run: "run_one", Task: "task_one", Pending: true}
		if err := saveCheckpoint(e.Config, ticket, raw); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		e.Stderr = &logs
		if _, err := e.Start(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		snapshot, path := false, false
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "recovery.snapshot" {
				snapshot = true
				cp := r["checkpoint"].(map[string]any)
				if cp["Run"] != "run_one" || cp["Task"] != "task_one" || cp["Dispatch"] != "" || cp["Pending"] != true || cp["Completed"] != false {
					t.Errorf("raw evidence overwritten: %v", r)
				}
			}
			if r["msg"] == "recovery.path" {
				path = true
				cp := r["checkpoint"].(map[string]any)
				if r["path"] != "completed-worker" || r["basis"] != "worker-report-succeeded" || cp["Dispatch"] != "ctx_one" || cp["Pending"] != false || cp["Completed"] != true || r["linear_used_for_selection"] != false {
					t.Errorf("wrong reconciled path: %v", r)
				}
			}
		}
		if !snapshot || !path || len(o.workerRequests) != 0 {
			t.Fatalf("snapshot=%v path=%v starts=%v", snapshot, path, o.workerRequests)
		}
	})
	t.Run("existing PR survives failed Linear observation and corrupt checkpoint", func(t *testing.T) {
		e, o, _, g, tools, ticket := fixture(t, "new-sha")
		e.Linear = &failLogLinear{readErr: errors.New("private remote read error")}
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
		if err != nil || result.PR.Number != 42 || len(o.workerRequests) != 0 || tools.gateCalls != 0 || tools.reviewCalls != 0 || len(g.created) != 0 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		snapshots, decisions := 0, 0
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "recovery.snapshot" {
				snapshots++
				if r["linear_read"] != "error" || r["error"] != "Linear recovery comments read failed" || r["linear_phase"] != nil || r["linear_owner"] != nil || r["checkpoint_error"] != "invalid recovery state" || r["linear_used_for_selection"] != false {
					t.Errorf("read failure confused with missing evidence: %v", r)
				}
			}
			if r["msg"] == "transition" {
				decisions++
				if r["decision_kind"] != "recovery-reconstruction" || r["allowed"] != true || r["applied"] != false || r["actual_phase"] != "待领" {
					t.Errorf("replay claims actual execution: %v", r)
				}
			}
			if r["msg"] == "external.start" || r["msg"] == "external.end" {
				t.Errorf("replay claims external work: %v", r)
			}
			if r["msg"] == "recovery.path" && (r["path"] != "existing-pr" || r["basis"] != "open-pr-by-branch") {
				t.Errorf("wrong PR priority: %v", r)
			}
		}
		if snapshots != 1 || decisions != 6 || strings.Contains(logs.String(), "private remote read error") {
			t.Fatalf("snapshots=%d decisions=%d", snapshots, decisions)
		}
	})
	t.Run("latest Linear evidence does not sort adapter slice", func(t *testing.T) {
		e, _, _, g, _, ticket := fixture(t, "new-sha")
		g.existing = &github.PullRequest{Number: 42, HTMLURL: "https://example.test/pr/42"}
		now := time.Now()
		comments := []linear.Comment{{Body: "阶段: 审查\n归属: newer", CreatedAt: now}, {Body: "阶段: worker 运行中\n归属: older", CreatedAt: now.Add(-time.Hour)}}
		original := append([]linear.Comment(nil), comments...)
		e.Linear = &sharedLogLinear{comments: comments}
		var logs bytes.Buffer
		e.Stderr = &logs
		if _, err := e.Start(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(comments, original) {
			t.Fatalf("adapter slice mutated: %v", comments)
		}
		found := false
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "recovery.snapshot" {
				found = true
				if r["linear_phase"] != "审查" || r["linear_owner"] != "newer" || r["linear_read"] != "ok" || r["linear_used_for_selection"] != false {
					t.Errorf("latest evidence: %v", r)
				}
			}
		}
		if !found {
			t.Fatal("missing snapshot")
		}
	})
	t.Run("recovery failure carries observed phase and step", func(t *testing.T) {
		e, _, _, _, tools, ticket := fixture(t, "new-sha")
		e.Tools = &failedLogHeadTools{fakeTools: tools}
		if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one"}); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		e.Stderr = &logs
		if _, err := e.Start(context.Background(), ticket); err == nil {
			t.Fatal("expected failure")
		}
		found := false
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "executor failed" {
				found = true
				if r["level"] != "ERROR" || r["phase"] != "worker 运行中" || r["recovery_step"] != "branch-lookup" || r["run"] != "run_one" || r["task"] != "task_one" || r["dispatch"] != "ctx_one" || r["error"] != "resume branch lookup failed" {
					t.Errorf("wrong recovery boundary: %v", r)
				}
			}
		}
		if !found || strings.Contains(logs.String(), "private remote head error") {
			t.Fatalf("failure missing or unsafe: %s", logs.String())
		}
	})
	t.Run("rejected decision is recorded", func(t *testing.T) {
		e, _, _, _, _, _ := fixture(t, "new-sha")
		var logs bytes.Buffer
		e.Stderr = &logs
		if _, _, err := e.step(machine.Initial(), machine.EvOpenPR); err == nil {
			t.Fatal("expected rejected decision")
		}
		records := transitionLogRecords(t, &logs)
		if len(records) != 1 {
			t.Fatalf("records=%v", records)
		}
		r := records[0]
		if r["from"] != "待领" || r["to"] != "待领" || r["event"] != "openPr" || !reflect.DeepEqual(r["effects"], []any{}) || r["record_kind"] != "decision" || r["allowed"] != false || r["applied"] != false || r["actual_phase"] != "待领" {
			t.Fatalf("rejection=%v", r)
		}
	})

	t.Run("correction pairs steps and distinguishes red verdicts", func(t *testing.T) {
		e, o, tools, _, _, ticket := recoveryFixture(t)
		tools.gateReds, tools.reviewReds = 1, 1
		var logs bytes.Buffer
		e.Stderr = &logs
		if _, err := e.Execute(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
		starts, ends := map[string]int{}, map[string]int{}
		active := map[string]bool{}
		reds, corrections := 0, 0
		for _, r := range transitionLogRecords(t, &logs) {
			step, _ := r["step"].(string)
			switch r["msg"] {
			case "external.start":
				if active[step] {
					t.Errorf("overlapping step: %v", r)
				}
				active[step] = true
				starts[step]++
			case "external.end":
				if !active[step] {
					t.Errorf("unpaired end: %v", r)
				}
				active[step] = false
				ends[step]++
				if r["result"] != "ok" || r["error"] != nil || r["run"] != "run_one" {
					t.Errorf("correction step failed: %v", r)
				}
				if duration, ok := r["duration_ms"].(float64); !ok || duration < 0 {
					t.Errorf("duration: %v", r)
				}
				if step == "worker-start" {
					wantTask, wantDispatch := "task_one", "ctx_one"
					if ends[step] > 1 {
						wantTask = "child_task"
						if ends[step] == 2 {
							wantDispatch = "ctx_2"
						} else {
							wantDispatch = "ctx_3"
						}
					}
					if r["task"] != wantTask || r["dispatch"] != wantDispatch || r["command_result"] != "ok" || r["receipt_error"] != nil {
						t.Errorf("wrong correction receipt: %v", r)
					}
				}
				if r["verdict"] == "red" {
					reds++
					wantEvent := "gateRed"
					if step == "reviewer" {
						wantEvent = "reviewRed"
					}
					if r["command_result"] != "ok" || r["parse_error"] != nil || r["event"] != wantEvent {
						t.Errorf("business red confused with command failure: %v", r)
					}
				}
			case "correction":
				corrections++
				wantReason, wantSummary := "gateRed", " | reason=gate red | evidence=gate"
				if corrections == 2 {
					wantReason, wantSummary = "reviewRed", " | reason=reviewer red | evidence=review"
				}
				summary, _ := r["fail_summary"].(string)
				if r["reason"] != wantReason || !strings.HasPrefix(summary, "FAIL: approach=head=new-sha files=unit/change.go") || !strings.HasSuffix(summary, wantSummary) {
					t.Errorf("wrong correction summary: %v", r)
				}
			}
		}
		want := map[string]int{"worker-start": 3, "task-list": 3, "gate": 3, "reviewer": 2, "open-pr": 1}
		if !reflect.DeepEqual(starts, want) || !reflect.DeepEqual(ends, want) || reds != 2 || corrections != 2 || len(o.starts) != 3 {
			t.Fatalf("starts=%v ends=%v reds=%d corrections=%d", starts, ends, reds, corrections)
		}
	})

	t.Run("invalid gate output", func(t *testing.T) {
		e, _, _, _, tools, ticket := fixture(t, "new-sha")
		e.Tools = &emptyLogGateTools{fakeTools: tools}
		e.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
		var logs bytes.Buffer
		e.Stderr = &logs
		result, err := e.Execute(context.Background(), ticket)
		if !errors.Is(err, context.Canceled) || result.State.Phase != machine.PhaseGate || tools.gateCalls != 1 {
			t.Fatalf("result=%+v err=%v gateCalls=%d", result, err, tools.gateCalls)
		}
		found := false
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "external.end" && r["step"] == "gate" {
				found = true
				if r["result"] != "error" || r["command_result"] != "ok" || r["call_error"] != nil || r["parse_error"] != "missing gate verdict" || r["event"] != "" {
					t.Errorf("invalid gate reported success: %v", r)
				}
			}
		}
		if !found {
			t.Fatal("missing gate end")
		}
	})
	t.Run("correction missing dispatch remains unapplied", func(t *testing.T) {
		e, _, tools, _, _, ticket := recoveryFixture(t)
		tools.gateReds = 1
		o := &secondMissingLogOrca{}
		e.Orca = o
		var logs bytes.Buffer
		e.Stderr = &logs
		result, err := e.Execute(context.Background(), ticket)
		if err == nil || result.State.Phase != machine.PhaseCorrection || len(o.starts) != 2 {
			t.Fatalf("result=%+v err=%v starts=%d", result, err, len(o.starts))
		}
		ends, failures := 0, 0
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "external.end" && r["step"] == "worker-start" {
				ends++
				if ends == 2 && (r["result"] != "error" || r["command_result"] != "ok" || r["error"] != "worker receipt missing task or dispatch" || r["task"] != "child_task") {
					t.Errorf("correction receipt reported success: %v", r)
				}
			}
			if r["msg"] == "executor failed" {
				failures++
				if r["phase"] != "写 correction" || r["transition_applied"] != false || r["event"] != "sessionOk" || r["decision_to"] != "worker 运行中" || r["task"] != "child_task" {
					t.Errorf("wrong correction failure phase: %v", r)
				}
			}
		}
		if ends != 2 || failures != 1 {
			t.Fatalf("ends=%d failures=%d", ends, failures)
		}
	})
	t.Run("canceled gate does not claim a selected transition", func(t *testing.T) {
		e, _, _, _, tools, ticket := fixture(t, "new-sha")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		e.Tools = &cancelLogGateTools{fakeTools: tools, cancel: cancel}
		var logs bytes.Buffer
		e.Stderr = &logs
		result, err := e.Execute(ctx, ticket)
		if !errors.Is(err, context.Canceled) || result.State.Phase != machine.PhaseGate {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		found := false
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "external.end" && r["step"] == "gate" {
				found = true
				if r["event"] != "" || r["stop_reason"] != "context canceled" {
					t.Errorf("canceled gate claims transition: %v", r)
				}
			}
		}
		if !found {
			t.Fatal("missing gate result")
		}
	})

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
		workerEnds := 0
		for _, r := range transitionLogRecords(t, &logs) {
			if r["msg"] == "external.end" && r["step"] == "worker-start" {
				workerEnds++
			}
		}
		if !strings.Contains(logs.String(), `"reason":"gateRed"`) || !strings.Contains(logs.String(), `"fail_summary":"FAIL: approach=`) || workerEnds != 2 {
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

func transitionLogRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("invalid log %q: %v", line, err)
		}
		records = append(records, r)
	}
	return records
}

type missingLogReceiptOrca struct{ fakeOrca }

func (o *missingLogReceiptOrca) WorkerStart(context.Context, string, orca.StartOptions) (orca.Receipt, error) {
	return orca.Receipt{}, nil
}

type sharedLogLinear struct {
	fakeLinear
	comments []linear.Comment
}

func (l *sharedLogLinear) ListComments(context.Context, string) ([]linear.Comment, error) {
	return l.comments, nil
}

type failedLogHeadTools struct{ *fakeTools }

func (f *failedLogHeadTools) Head(context.Context, string, string) (string, error) {
	return "", errors.New("private remote head error")
}

type emptyLogGateTools struct{ *fakeTools }

func (f *emptyLogGateTools) Gate(context.Context, string, string, string) (string, error) {
	f.gateCalls++
	return "", nil
}

type secondMissingLogOrca struct{ recoveryOrca }

func (o *secondMissingLogOrca) WorkerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	receipt, err := o.recoveryOrca.WorkerStart(ctx, request, opts)
	if len(o.starts) == 2 {
		delete(receipt, "dispatchId")
	}
	return receipt, err
}

type cancelLogGateTools struct {
	*fakeTools
	cancel context.CancelFunc
}

func (f *cancelLogGateTools) Gate(ctx context.Context, repo, head, spec string) (string, error) {
	output, err := f.fakeTools.Gate(ctx, repo, head, spec)
	f.cancel()
	return output, err
}
