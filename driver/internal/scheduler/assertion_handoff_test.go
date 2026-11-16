package scheduler

import (
	"context"
	"strings"
	"testing"
)

func TestFrozenHandoffAssertionMigration(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		wantError    bool
	}{
		{"legacy-no", "CHECK_IS_NEW_TEST: no\n", false},
		{"legacy-yes", "CHECK_IS_NEW_TEST: yes\nCHECK_TEST_FILES: test/probe.sh\n", true},
		{"new-yes", "CHECK_IS_NEW_TEST: yes\nCHECK_TEST_FILES: test/probe.sh test/assertion.json\nCHECK_ASSERTION: test/assertion.json\n", false},
		{"undeclared", "CHECK_IS_NEW_TEST: yes\nCHECK_TEST_FILES: test/probe.sh\nCHECK_ASSERTION: test/assertion.json\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := validHandoffForResolver()
			h.GateSpec += "\n" + tc.fields
			before := h
			err := h.Validated()
			if (err != nil) != tc.wantError || err != nil && !strings.Contains(err.Error(), "CHECK_ASSERTION") {
				t.Fatalf("validation = %v", err)
			}
			if h != before {
				t.Fatal("validation rewrote frozen history")
			}
		})
	}
}

func TestLegacyNewTestMergeReturnsMigrationDiagnostic(t *testing.T) {
	f := newMergeFixture(t)
	rewriteHandoff(t, f, func(h map[string]any) {
		h["gate_spec"] = h["gate_spec"].(string) + "\nCHECK_IS_NEW_TEST: yes\nCHECK_TEST_FILES: test/probe.sh\n"
	})
	err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CHECK_ASSERTION") || !strings.Contains(err.Error(), "fresh reviewed handoff") || f.puts != 0 {
		t.Fatalf("legacy frozen work lost migration diagnosis: %v puts=%d", err, f.puts)
	}
}
