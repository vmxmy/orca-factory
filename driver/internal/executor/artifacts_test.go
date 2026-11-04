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

func TestArtifactsKeepTicketFields(t *testing.T) {
	check := `grep -q "\[worker\].*gate\.spec" driver.log && printf "\"%s\"\n" "$CHECK"`
	scope := `driver/internal/executor/\*.go`
	testFiles := `driver/internal/executor/parse\_escape_test.go`
	description := "## 要做\n\n1. preserve ticket fields\n\nCHECK: " + check + "\nSCOPE: " + scope + "\nCHECK_TEST_FILES: " + testFiles + "\n"
	ticket, err := ParseTicket("id", "ZY-366", "t", description)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Root: t.TempDir(), Repository: "demo", Unit: "executor"}
	brief, specPath, err := writeArtifacts(config, ticket, "agent/ZY-366/executor")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"brief": brief, "spec": string(spec)} {
		for field, want := range map[string]string{"CHECK": check, "SCOPE": scope, "CHECK_TEST_FILES": testFiles} {
			if !strings.Contains(content, "\n"+field+": "+want+"\n") {
				t.Errorf("%s does not preserve %s=%q: %s", name, field, want, content)
			}
		}
	}
}
