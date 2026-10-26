package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// The recorded reuse receipts (see CONTRACTS.md): a reused terminal accepts the
// dispatch; a stale handle is rejected with terminal_handle_stale and a released
// terminal with agent_unconfigured.
func TestRecordedWorkerStartReuseTerminal(t *testing.T) {
	for name, wantCode := range map[string]string{
		"orca-worker-start-reuse-terminal.json":           "",
		"orca-worker-start-reuse-busy-queued.json":        "",
		"orca-worker-start-reuse-rejected.json":           "terminal_handle_stale",
		"orca-worker-start-reuse-agent-unconfigured.json": "agent_unconfigured",
	} {
		wantOK := wantCode == ""
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", name))
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				OK     *bool `json:"ok"`
				Result struct {
					RunID      string `json:"runId"`
					TaskID     string `json:"taskId"`
					DispatchID string `json:"dispatchId"`
					State      string `json:"state"`
					Stage      string `json:"stage"`
				} `json:"result"`
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(data, &envelope) != nil || envelope.OK == nil {
				t.Fatalf("invalid envelope: %s", data)
			}
			if *envelope.OK != wantOK {
				t.Fatalf("ok=%v want %v: %s", *envelope.OK, wantOK, data)
			}
			if wantOK {
				if envelope.Result.DispatchID == "" || envelope.Result.Stage != "input_accepted" {
					t.Fatalf("reuse receipt missing dispatch/stage: %s", data)
				}
			} else if envelope.Error == nil || envelope.Error.Code != wantCode || !orca.IsTerminalStale(&orca.RejectedError{Code: envelope.Error.Code}) {
				t.Fatalf("expected reuse rejection %s: %s", wantCode, data)
			}
		})
	}
}

type reuseOrca struct {
	*recoveryOrca
	started    []orca.StartOptions
	requests   []string
	releases   int
	sequence   int
	rejectCode string
	handles    map[string]string
}

func (o *reuseOrca) WorkerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	o.requests = append(o.requests, request)
	o.started = append(o.started, opts)
	if o.rejectCode != "" && opts.Terminal != "" {
		return nil, &orca.RejectedError{Code: o.rejectCode}
	}
	receipt, err := o.recoveryOrca.WorkerStart(ctx, request, opts)
	if err == nil {
		o.sequence++
		// Distinct dispatch per start (matching TaskList's ctx_N) keeps polling
		// unambiguous; taskId stays whatever the base fixture echoed.
		if o.sequence > 1 {
			receipt["dispatchId"], _ = json.Marshal("ctx_" + strconv.Itoa(o.sequence))
		}
	}
	return receipt, err
}

func (o *reuseOrca) WorkerRelease(ctx context.Context, id, dispatch string) (orca.Receipt, error) {
	o.releases++
	return o.recoveryOrca.WorkerRelease(ctx, id, dispatch)
}

func (o *reuseOrca) Workers(context.Context, orca.ListOptions) ([]orca.Worker, error) {
	// One live worker per started dispatch keeps liveness polling satisfied;
	// the reused terminal stays visible under its latest dispatch.
	workers := make([]orca.Worker, 0, o.sequence)
	for i := 1; i <= o.sequence; i++ {
		dispatch := "ctx_" + strconv.Itoa(i)
		if i == 1 {
			dispatch = "ctx_one"
		}
		w := orca.Worker{DispatchID: dispatch}
		w.AgentTerminalHandle = "term_reuse"
		w.DispatchStatus = "succeeded"
		workers = append(workers, w)
	}
	return workers, nil
}

// Same tier with a live terminal reuses it: Terminal set, no agent flags, no release.
func TestCorrectionReusesWorkerTerminal(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	tools.gateReds = 1
	ro := &reuseOrca{recoveryOrca: o, handles: map[string]string{"ctx_one": "term_reuse"}}
	e.Orca = ro
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var correction *orca.StartOptions
	for i := range ro.started {
		if i > 0 {
			correction = &ro.started[i]
		}
	}
	if correction == nil {
		t.Fatalf("no correction dispatch: %+v", ro.started)
	}
	if correction.Terminal != "term_reuse" || correction.Agent != "" || correction.Model != "" || correction.Effort != "" {
		t.Fatalf("correction start should reuse the terminal without agent flags: %+v", *correction)
	}
	// Dispatch-layer settlement releases stay unchanged; terminal reuse is
	// independent of them (busy-queued sample: reuse queues, not conflicts).
	if ro.releases > len(ro.started) {
		t.Fatalf("unexpected release count: %d", ro.releases)
	}
}

// Escalated tier starts a fresh agent instead of reusing the terminal.
func TestCorrectionEscalationStartsFreshAgent(t *testing.T) {
	e, o, tools, _, l, ticket := recoveryFixture(t)
	tools.gateReds = 1
	_ = l
	ro := &reuseOrca{recoveryOrca: o, handles: map[string]string{"ctx_one": "term_reuse"}}
	e.Orca = ro
	e.Config.Escalate = "1"
	e.Config.Tier = "default"
	e.Config.TierSettings = map[string]string{"strong": "codex:gpt-6-astra:medium"}
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(ro.started) < 2 {
		t.Fatalf("no correction dispatch: %+v", ro.started)
	}
	last := ro.started[len(ro.started)-1]
	if last.Terminal != "" || last.Agent == "" {
		t.Fatalf("escalation must start a fresh agent: %+v", last)
	}
}

// A stale-terminal rejection falls back to a fresh agent without failing the run.
func TestCorrectionReuseRejectedFallsBack(t *testing.T) {
	e, o, tools, service, _, ticket := recoveryFixture(t)
	tools.gateReds = 1
	ro := &reuseOrca{recoveryOrca: o, handles: map[string]string{"ctx_one": "term_reuse"}, rejectCode: "terminal_handle_stale"}
	e.Orca = ro
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var reuseAttempt, fresh *orca.StartOptions
	for i := range ro.started {
		if i == 1 && ro.started[i].Terminal != "" {
			reuseAttempt = &ro.started[i]
		}
		if ro.started[i].Terminal == "" && ro.started[i].Agent != "" {
			fresh = &ro.started[i]
		}
	}
	if fresh == nil {
		t.Fatalf("no fresh-agent fallback: %+v", ro.started)
	}
	if reuseAttempt == nil {
		t.Fatalf("no reuse attempt recorded before fallback: %+v", ro.started)
	}
	if service.children != 1 {
		t.Fatalf("fallback must reuse the child task created before terminal rejection: child calls=%d", service.children)
	}
	if len(ro.requests) < 3 || ro.requests[1] == ro.requests[2] {
		t.Fatalf("fallback must use a distinct request scope after terminal rejection: requests=%v", ro.requests)
	}
}

// A released worker terminal no longer runs an agent: Orca refuses the reuse
// with agent_unconfigured (recorded sample), so the same correction starts a
// fresh agent instead of reporting an uncertain dispatch.
func TestCorrectionStartsFreshTerminalWhenAgentUnconfigured(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	tools.gateReds = 1
	ro := &reuseOrca{recoveryOrca: o, handles: map[string]string{"ctx_one": "term_reuse"}, rejectCode: "agent_unconfigured"}
	e.Orca = ro
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Corrections != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	last := ro.started[len(ro.started)-1]
	if len(ro.started) != 3 || ro.started[1].Terminal != "term_reuse" || last.Terminal != "" || last.Agent == "" || last.Task != ro.started[1].Task {
		t.Fatalf("correction must fall back to a fresh agent terminal: %+v", ro.started)
	}
}
