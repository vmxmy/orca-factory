package mergechannel

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestPreparedGateKeepsFrozenAssertionSpec(t *testing.T) {
	h := merge.Handoff{Unit: "executor", Lane: 1,
		GateSpec:     "UNIT: executor\nCHECK: true\nSCOPE: test/*\nCHECK_IS_NEW_TEST: yes\nCHECK_TEST_FILES: test/probe.sh test/assertion.json\nCHECK_ASSERTION: test/assertion.json\n",
		GateOutput:   "UNIT executor\nVERDICT green\nSTEP all\nREASON passed\nEVIDENCE test\nSCOPE test/*\nLANE 1",
		ReviewOutput: `{"verdict":"green","step":"review"}`}
	original := h.GateSpec
	a := Gate{Handoff: h, Env: map[string]string{},
		Command: func(_ context.Context, _ string, command string, args ...string) ([]byte, error) {
			if command != "git" || !slices.Equal(args, []string{"show", "trusted-main:scripts/agent-gates/gate.sh"}) {
				t.Fatalf("untrusted gate source: %s %v", command, args)
			}
			return []byte("#!/bin/bash\n"), nil
		},
		Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
			raw, err := os.ReadFile(spec.Args[2])
			if err != nil || string(raw) != original || !slices.Contains(spec.Env, "GATE_PROJECT_SRC=trusted-main") || !slices.Contains(spec.Env, "GATE_BASE=trusted-main") {
				t.Fatalf("lost frozen assertion or trusted refs: %s %v", raw, err)
			}
			return []byte(h.GateOutput), nil
		}}
	run, err := a.Prepare(context.Background(), merge.Candidate{Main: "trusted-main"})
	if err != nil {
		t.Fatal(err)
	}
	a.Handoff.GateSpec = strings.ReplaceAll(original, "test/assertion.json", "test/changed.json")
	if err := run(context.Background(), filepath.Join(t.TempDir(), "wt")); err != nil {
		t.Fatal(err)
	}
}
