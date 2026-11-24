package executor

import (
	"context"
	"errors"
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
	for _, tc := range []struct {
		name        string
		unavailable []modelhealth.Ref
		weeklyLimit bool
		wantAgent   string
		wantModel   string
	}{
		{name: "primary available", wantAgent: "claude", wantModel: "claude-opus-5-5"},
		{name: "primary weekly limit", unavailable: []modelhealth.Ref{primary}, wantAgent: "codex", wantModel: "gpt-6.2-astra"},
		{name: "all unavailable", unavailable: []modelhealth.Ref{primary, fallback}},
		{name: "session weekly limit selects fallback next round", weeklyLimit: true, wantAgent: "codex", wantModel: "gpt-6.2-astra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_CORRECTION", "")
			t.Setenv("AGENT_CORRECTION_FALLBACK", "")
			state := t.TempDir()
			registry, err := modelhealth.Open(state)
			if err != nil {
				t.Fatal(err)
			}
			for _, ref := range []modelhealth.Ref{primary, fallback} {
				status := modelhealth.Status{Ref: ref, State: modelhealth.OK, CheckedAt: time.Now()}
				if slices.Contains(tc.unavailable, ref) {
					status.State, status.Reason, status.Until = modelhealth.Unavailable, "weekly_limit", time.Now().Add(time.Hour)
				}
				if err := registry.Record(status); err != nil {
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
			if tc.wantAgent == "" {
				tools.gateReds = 1
				recovery := &fallbackRecovery{correction: commands.Correction}
				e.Recovery = recovery
				result, err := e.Execute(context.Background(), ticket)
				if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Corrections != 1 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if len(calls) != 0 {
					t.Errorf("all unavailable must not launch a session: calls=%d", len(calls))
				}
				want := "执行器代写：correction 会话未写出修复步骤（会话失败），按下列失败证据修复。"
				if len(recovery.specs) != 1 || !strings.Contains(recovery.specs[0], want) {
					t.Fatalf("expected OF-661 fallback in child task: specs=%q", recovery.specs)
				}
				return
			}
			req := CorrectionRequest{Repo: e.Config.Repo, Path: filepath.Join(state, "correction.md"), Cause: "gate red", Number: 1}
			if tc.weeklyLimit {
				if err := commands.Correction(context.Background(), req); err == nil {
					t.Fatal("weekly limit must return the session failure")
				}
				registry, err = modelhealth.Open(state)
				if err != nil {
					t.Fatal(err)
				}
				if status := registry.Status(primary, time.Now()); status.State != modelhealth.Unavailable {
					t.Errorf("weekly limit must persist primary unavailable: %+v", status)
				}
				req.Number++
			}
			if err := commands.Correction(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			wantCalls := 1
			if tc.weeklyLimit {
				wantCalls = 2
			}
			if len(calls) != wantCalls {
				t.Fatalf("calls=%d want=%d", len(calls), wantCalls)
			}
			spec := calls[len(calls)-1]
			if spec.Timeout != 10*time.Minute || spec.Dir != req.Repo {
				t.Errorf("timeout=%v dir=%q", spec.Timeout, spec.Dir)
			}
			if tc.wantAgent == "claude" {
				if spec.Path != "claude" || len(spec.Args) < 2 || spec.Args[0] != "-p" {
					t.Errorf("expected claude -p: path=%q args=%q", spec.Path, spec.Args)
				}
			} else if filepath.Base(spec.Path) != "orca" || len(spec.Args) == 0 || spec.Args[0] != "exec" || !slices.Contains(spec.Args, "codex") {
				t.Errorf("expected Codex through orca exec: path=%q args=%q", spec.Path, spec.Args)
			}
			for flag, want := range map[string]string{"--model": tc.wantModel, "--effort": "high"} {
				i := slices.Index(spec.Args, flag)
				if i < 0 || i+1 >= len(spec.Args) || spec.Args[i+1] != want {
					t.Errorf("expected %s %s: args=%q", flag, want, spec.Args)
				}
			}
		})
	}
}
