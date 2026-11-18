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
	description := "## 要做\n\n1. preserve ticket fields\n\nCHECK: " + check + "\nSCOPE: " + scope + "\nCHECK_TEST_FILES: " + testFiles + "\nCHECK_ASSERTION: test/assertion.json\n"
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
		for field, want := range map[string]string{"CHECK": check, "SCOPE": scope, "CHECK_TEST_FILES": testFiles, "CHECK_ASSERTION": "test/assertion.json"} {
			if !strings.Contains(content, "\n"+field+": "+want+"\n") {
				t.Errorf("%s does not preserve %s=%q: %s", name, field, want, content)
			}
		}
	}
}

func TestParseTicketRejectsRepeatedGateFields(t *testing.T) {
	valid := "## 要做\n\n1. fix it\n\n## 验收\n\nCHECK: make test\n\n## SCOPE\n\ndriver/internal/executor/artifacts.go\n"
	for field, smuggled := range map[string]string{
		"SCOPE":             "SCOPE: core/gate.sh",
		"CHECK":             "CHECK: true",
		"CHECK_TEST_FILES":  "CHECK_TEST_FILES: a_test.go",
		"CHECK_IS_NEW_TEST": "CHECK_IS_NEW_TEST: yes",
		"CHECK_ASSERTION":   "CHECK_ASSERTION: a.json",
	} {
		description := "## 现状\n\n" + smuggled + "\n" + valid + "CHECK_TEST_FILES: b_test.go\nCHECK_IS_NEW_TEST: no\nCHECK_ASSERTION: b.json\n"
		if _, err := ParseTicket("id", "OF-556", "t", description); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("repeated %s accepted or not named: %v", field, err)
		}
	}
	got, err := ParseTicket("id", "OF-556", "t", valid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Todo != "1. fix it" || got.Check != "make test" || got.Scope != "driver/internal/executor/artifacts.go" {
		t.Fatalf("single-field ticket changed: %+v", got)
	}
}
