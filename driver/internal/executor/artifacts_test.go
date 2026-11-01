package executor

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestWorkerBriefDoesNotPredictLane(t *testing.T) {
	for _, lane := range []int{1, 2} {
		t.Run(fmt.Sprintf("lane_%d", lane), func(t *testing.T) {
			config := Config{Root: t.TempDir(), Repository: "demo", Unit: "executor"}
			ticket := Ticket{Identifier: "OF-349", Lane: lane}
			brief, specPath, err := writeArtifacts(config, ticket, "agent/OF-349/executor")
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(brief, "\n") {
				if strings.HasPrefix(line, "LANE:") {
					t.Errorf("worker brief predicts a lane: %q", line)
				}
			}
			spec, err := os.ReadFile(specPath)
			if err != nil {
				t.Fatal(err)
			}
			wantSpecLane := fmt.Sprintf("\nLANE: %d\n", lane)
			if !strings.Contains(string(spec), wantSpecLane) {
				t.Errorf("gate spec lacks %q: %s", wantSpecLane, spec)
			}
		})
	}
}
