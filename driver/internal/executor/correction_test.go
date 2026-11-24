package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestCorrectionSessionSelectsModelFromRegistry(t *testing.T) {
	primary := modelhealth.Ref{Agent: "claude", Model: "claude-opus-5-5", Effort: "high"}
	fallback := modelhealth.Ref{Agent: "codex", Model: "gpt-6.2-astra", Effort: "high"}
	custom := modelhealth.Ref{Agent: "codex", Model: "configured-model", Effort: "medium"}
	for _, tc := range []struct {
		name, primaryEnv, fallbackEnv string
		unavailable, wantCalls        []modelhealth.Ref
		weeklyLimit                   bool
	}{
		{name: "primary available", wantCalls: []modelhealth.Ref{primary}},
		{name: "primary weekly limit", unavailable: []modelhealth.Ref{primary}, wantCalls: []modelhealth.Ref{fallback}},
		{name: "all unavailable", unavailable: []modelhealth.Ref{primary, fallback}},
		{name: "session weekly limit retries with fallback", weeklyLimit: true, wantCalls: []modelhealth.Ref{primary, fallback}},
		{name: "configured primary", primaryEnv: custom.String(), wantCalls: []modelhealth.Ref{custom}},
		{name: "ordered configured fallbacks", fallbackEnv: fallback.String() + " " + custom.String(),
			unavailable: []modelhealth.Ref{primary, fallback}, wantCalls: []modelhealth.Ref{custom}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_CORRECTION", tc.primaryEnv)
			t.Setenv("AGENT_CORRECTION_FALLBACK", tc.fallbackEnv)
			state := t.TempDir()
			registry, err := modelhealth.Open(state)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			for _, ref := range tc.unavailable {
				if err := registry.Record(modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: now, Until: now.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			e, _, tools, _, _, ticket := recoveryFixture(t)
			writeRole(t, e.Config.Repo, "correction.md", "CORRECTION ROLE\n")
			var calls []runx.Spec
			commands := &Commands{StateRoot: state, Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
				calls = append(calls, spec)
				if tc.weeklyLimit && len(calls) == 1 {
					return []byte("You've hit your weekly limit"), errors.New("exit status 1")
				}
				return nil, nil
			}}
			if len(tc.wantCalls) == 0 {
				tools.gateReds = 1
				recovery := &fallbackRecovery{correction: commands.Correction}
				e.Recovery = recovery
				result, err := e.Execute(context.Background(), ticket)
				if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Corrections != 1 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if len(calls) != 0 || len(recovery.requests) != 1 || len(recovery.specs) != 1 {
					t.Fatalf("model calls=%d correction requests=%d child tasks=%d", len(calls), len(recovery.requests), len(recovery.specs))
				}
				req := recovery.requests[0]
				data, err := os.ReadFile(req.Path)
				if err != nil || !strings.Contains(string(data), "执行器代写：") || !strings.Contains(string(data), req.Cause) || !strings.Contains(recovery.specs[0], string(data)) {
					t.Fatalf("correction file=%q err=%v child=%q", data, err, recovery.specs[0])
				}
				return
			}
			req := CorrectionRequest{Repo: e.Config.Repo, Path: filepath.Join(state, "correction.md"), Cause: "gate red", Number: 1}
			if err := commands.Correction(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if len(calls) != len(tc.wantCalls) {
				t.Fatalf("calls=%d want=%d", len(calls), len(tc.wantCalls))
			}
			for i, want := range tc.wantCalls {
				spec := calls[i]
				if spec.Path != want.Agent || spec.Timeout != 10*time.Minute || spec.Dir != req.Repo {
					t.Fatalf("call %d: path=%q timeout=%v dir=%q", i, spec.Path, spec.Timeout, spec.Dir)
				}
				mode, flags := "-p", map[string]string{"--model": want.Model, "--effort": want.Effort}
				if want.Agent == "codex" {
					mode, flags = "exec", map[string]string{"-m": want.Model, "-c": "model_reasoning_effort=" + want.Effort}
				}
				if len(spec.Args) == 0 || spec.Args[0] != mode || !strings.Contains(strings.Join(spec.Args, " "), req.Path) {
					t.Errorf("expected %s and correction path in %q", mode, spec.Args)
				}
				for flag, value := range flags {
					pos := slices.Index(spec.Args, flag)
					if pos < 0 || pos+1 >= len(spec.Args) || spec.Args[pos+1] != value {
						t.Errorf("expected %s %s in %q", flag, value, spec.Args)
					}
				}
			}
			if tc.weeklyLimit {
				registry, err = modelhealth.Open(state)
				if err != nil {
					t.Fatal(err)
				}
				if status := registry.Status(primary, time.Now()); status.State != modelhealth.Unavailable || status.Reason != "usage_limit" {
					t.Errorf("primary after weekly limit: %+v", status)
				}
			}
		})
	}
}
