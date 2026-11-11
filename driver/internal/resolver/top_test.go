package resolver

import (
	"context"
	"encoding/json"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"os"
	"testing"
)

type topOrca struct {
	*fakeOrca
	launch string
}

func (o *topOrca) WorkerStart(ctx context.Context, id string, opts orca.StartOptions) (orca.Receipt, error) {
	receipt, err := o.fakeOrca.WorkerStart(ctx, id, opts)
	if err == nil {
		receipt["launch"] = json.RawMessage(o.launch)
	}
	return receipt, err
}

func TestTopModelAcceptance(t *testing.T) {
	for _, model := range []string{"claude-fable-5-2", "replacement"} {
		t.Run(model, func(t *testing.T) {
			c, s, o := setupController()
			if err := json.Unmarshal([]byte(`{"version":1,"attempt":3,"phase":"waiting","top":true,"top_used":true}`), &s.record); err != nil {
				t.Fatal(err)
			}
			c.Agent, c.Model, c.Effort = "claude", "claude-fable-5-2", "xhigh"
			c.Orca = &topOrca{fakeOrca: o, launch: `{"effective":{"agent":"claude","model":"` + model + `","effort":"xhigh"}}`}
			if _, err := c.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(s.record)
			var fields map[string]any
			_ = json.Unmarshal(raw, &fields)
			if model == "claude-fable-5-2" {
				if fields["top_model_accepted"] == true {
					t.Fatalf("launch alone claimed provider acceptance: %s", raw)
				}
				o.report = &orca.WorkerReport{Provenance: "worker_report", Outcome: "succeeded", Body: `{"action":"ruling","reason":"按 CHECK 实施"}`}
				if _, err := c.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
				raw, _ = json.Marshal(s.record)
				_ = json.Unmarshal(raw, &fields)
				if fields["top_model_accepted"] != true {
					t.Fatalf("no durable accepted model: %s", raw)
				}
			} else if fields["top_launch_error"] == nil {
				t.Fatalf("substitute accepted: %s", raw)
			}
		})
	}
}

func TestTopRejectedModelStopsBeforeTransfer(t *testing.T) {
	c, s, o := setupController()
	if err := json.Unmarshal([]byte(`{"version":1,"attempt":4,"phase":"running","top":true,"run":"run_1","task":"task_1","dispatch":"ctx_1","top_launch_error":"model rejected"}`), &s.record); err != nil {
		t.Fatal(err)
	}
	o.stopped = workerRows("run_1", `[{"taskId":"task_1","runId":"run_1","dispatchId":"ctx_1","dispatchStatus":"cancelled"}]`)
	p, err := c.Step(context.Background())
	if err != nil || p.Phase != "result" || o.stops != 1 || o.startCalls != 0 || o.releases != 1 {
		t.Fatalf("rejection not settled: %+v %v stops=%d starts=%d releases=%d", p, err, o.stops, o.startCalls, o.releases)
	}
	decision, err := ParseResult(p.Result)
	if err != nil || decision.Action != "escalate" {
		t.Fatalf("missing human transfer: %+v %v", decision, err)
	}
}

func TestRecordedTopModelWithoutEffectiveIdentityIsRejected(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/orca-worker-start-reuse-terminal.json")
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct{ Result orca.Receipt }
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	c, s, o := setupController()
	if err = json.Unmarshal([]byte(`{"version":1,"attempt":3,"phase":"waiting","top":true}`), &s.record); err != nil {
		t.Fatal(err)
	}
	c.Orca = &topOrca{fakeOrca: o, launch: string(receipt.Result["launch"])}
	p, err := c.Step(context.Background())
	if err != nil || p.TopModelAccepted || p.TopLaunchError == "" {
		t.Fatalf("recorded null effective identity accepted: %+v %v", p, err)
	}
}

func TestTopModelWithoutResponseEscalates(t *testing.T) {
	c, s, o := setupController()
	if err := json.Unmarshal([]byte(`{"version":1,"attempt":4,"phase":"running","top":true,"top_launch_accepted":true,"run":"run_1","task":"task_1","dispatch":"ctx_1"}`), &s.record); err != nil {
		t.Fatal(err)
	}
	o.status = "failed"
	p, err := c.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := ParseResult(p.Result)
	if err != nil || proposal.Action != "escalate" || p.TopModelAccepted || o.startCalls != 0 {
		t.Fatalf("missing response retried or called accepted: %+v %v", p, err)
	}
}
