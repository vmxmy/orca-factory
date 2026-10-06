package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

const reuseTerminal = "term_8366b1bf-eb23-46d0-a82c-d4ae2d471bff"

type reuseOrca struct {
	recordedEffects json.RawMessage
	uncertain       string
	*recoveryOrca
	requests []string
	events   []string
	reject   *orca.Client
}

func (o *reuseOrca) WorkerStart(ctx context.Context, id string, opts orca.StartOptions) (orca.Receipt, error) {
	o.requests = append(o.requests, id)
	terminal := opts.Terminal
	if terminal != "" {
		o.events = append(o.events, "reuse:"+terminal)
		if o.reject != nil {
			return o.reject.WorkerStart(ctx, id, opts)
		}
	} else {
		o.events = append(o.events, "agent:"+opts.Agent)
	}
	r, err := o.recoveryOrca.WorkerStart(ctx, id, opts)
	r["effects"] = o.recordedEffects
	if terminal != "" {
		switch o.uncertain {
		case "timeout":
			return r, context.DeadlineExceeded
		case "missing-dispatch":
			delete(r, "dispatchId")
		}
	}
	return r, err
}
func (o *reuseOrca) WorkerRelease(ctx context.Context, id, dispatch string) (orca.Receipt, error) {
	o.events = append(o.events, "release:"+dispatch)
	return o.recoveryOrca.WorkerRelease(ctx, id, dispatch)
}
func reuseFixture(t *testing.T) (*Executor, *reuseOrca, Ticket) {
	t.Helper()
	e, base, _, _, _, ticket := recoveryFixture(t)
	base.failFirst = true
	var envelope struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(recorded(t, "orca-worker-start-reuse-terminal.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	o := &reuseOrca{recoveryOrca: base, recordedEffects: envelope.Result["effects"]}
	e.Orca = o
	e.Config.Escalate = "0"
	return e, o, ticket
}
func TestCorrectionReusesWorkerTerminal(t *testing.T) {
	e, o, ticket := reuseFixture(t)
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := []string{"agent:codex", "reuse:" + reuseTerminal, "release:ctx_2"}
	if !reflect.DeepEqual(o.events, want) {
		t.Fatalf("events=%v want=%v", o.events, want)
	}
	child := o.starts[1]
	if child.Task != "child_task" || child.Worktree != "path:/original/worktree" || child.Agent != "" || child.Model != "" || child.Effort != "" {
		t.Fatalf("child=%+v", child)
	}
}
func TestCorrectionEscalationStartsFreshAgent(t *testing.T) {
	e, o, ticket := reuseFixture(t)
	e.Config.Escalate = "1"
	e.Config.Tier = "fast"
	e.Config.TierSettings = map[string]string{"default": "claude:next-model:high"}
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := []string{"agent:codex", "release:ctx_one", "agent:claude", "release:ctx_2"}
	if !reflect.DeepEqual(o.events, want) {
		t.Fatalf("events=%v want=%v", o.events, want)
	}
	if o.starts[1].Model != "next-model" || o.starts[1].Effort != "high" {
		t.Fatalf("child=%+v", o.starts[1])
	}
}
func TestCorrectionReuseRejectedFallsBack(t *testing.T) {
	e, o, ticket := reuseFixture(t)
	fixture, err := filepath.Abs("../../testdata/recorded/orca-worker-start-reuse-released.json")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	o.reject = &orca.Client{Path: script}
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(o.requests) != 3 || o.requests[1] == o.requests[2] {
		t.Fatalf("fallback reused mutation ID: %v", o.requests)
	}
	if e.Recovery.(*recoveryService).children != 1 || o.starts[1].Task != "child_task" {
		t.Fatal("fallback must start the same child task")
	}
	want := []string{"agent:codex", "reuse:" + reuseTerminal, "release:ctx_one", "agent:codex", "release:ctx_2"}
	if !reflect.DeepEqual(o.events, want) {
		t.Fatalf("events=%v want=%v", o.events, want)
	}
}

func TestCorrectionReuseUncertainDoesNotReplay(t *testing.T) {
	for _, scenario := range []string{"timeout", "missing-dispatch"} {
		t.Run(scenario, func(t *testing.T) {
			e, o, ticket := reuseFixture(t)
			o.uncertain = scenario
			result, err := e.Execute(context.Background(), ticket)
			if err == nil || result.State.Owner == machine.OwnerMerge {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if scenario == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost timeout: %v", err)
			}
			want := []string{"agent:codex", "reuse:" + reuseTerminal}
			if !reflect.DeepEqual(o.events, want) {
				t.Fatalf("uncertain attempt replayed/released: %v", o.events)
			}
			cp, err := readCheckpoint(e.Config, ticket)
			if err != nil || !cp.Pending || cp.Settled {
				t.Fatalf("checkpoint=%+v err=%v", cp, err)
			}
		})
	}
}

func TestCorrectionAtStrongTierReusesTerminal(t *testing.T) {
	e, o, ticket := reuseFixture(t)
	e.Config.Escalate = "1"
	e.Config.Tier = "strong"
	e.Config.Agent = "claude"
	e.Config.Model = "strong-model"
	e.Config.Effort = "high"
	e.Config.TierSettings = map[string]string{"strong": "claude:strong-model:high"}
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := []string{"agent:claude", "reuse:" + reuseTerminal, "release:ctx_2"}
	if !reflect.DeepEqual(o.events, want) {
		t.Fatalf("events=%v want=%v", o.events, want)
	}
}
