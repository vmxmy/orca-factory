package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func shellLedgerCommand(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	command := exec.Command(os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--", "ledger-shell"}, args...)...)
	command.Dir = dir
	command.Env = append(os.Environ(), "DRIVER_TEST_PROCESS=1", "PATH=.:"+os.Getenv("PATH"), "AGENT_LINEAR_TEAM=TEST", "AGENT_LINEAR_PROJECT=project", "AGENT_LINEAR_COUNT_FILE="+filepath.Join(dir, "count"), "LEDGER_TEST_DIR="+dir)
	var out, warnings bytes.Buffer
	command.Stdout, command.Stderr = &out, &warnings
	code := 0
	if err := command.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return code, out.String(), warnings.String()
}

func shellLedgerFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$LEDGER_TEST_DIR/calls"
case "$2" in
list-issues) printf '%s\n' '{"ok":true,"result":{"issues":[{"title":"mentions [ledger] agent-graph","identifier":"WRONG"},{"title":"[ledger] agent-graph","identifier":"TEST-1"}]}}' ;;
comment) printf '%s' "$6" > "$LEDGER_TEST_DIR/body"; printf '%s\n' '{"ok":true}' ;;
issue) printf '{"ok":true,"result":{"comments":'; /bin/cat "$LEDGER_TEST_DIR/comments"; printf ',"meta":{"sections":{"comments":{"hasMore":true}}}}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "orca"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLedgerShellContract(t *testing.T) {
	dir := shellLedgerFixture(t)
	code, out, warnings := shellLedgerCommand(t, dir, "--contract-version")
	if code != 0 || out != "ledger-shell-v1\n" || warnings != "" {
		t.Fatalf("probe: %d %q %q", code, out, warnings)
	}
	code, out, warnings = shellLedgerCommand(t, dir, "append", `null {"unknown":"中文","n":1}`, "ignored")
	body, err := os.ReadFile(filepath.Join(dir, "body"))
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || out != "" || warnings != "" || string(body) != "ledger null\n{\"unknown\":\"中文\",\"n\":1}" {
		t.Fatalf("append: %d %q %q body=%q", code, out, warnings, body)
	}
	comments := `[{"body":"ledger {\"ticket\":\"A\",\"lane\":[2],\"tier\":{},\"outcome\":\"merged\",\"first_gate\":\"green\",\"corrections\":1,\"ci_reds\":2,\"minutes\":3}"},{"body":"ledger {\"ticket\":\"A\",\"lane\":0,\"outcome\":\"escalated\"}"},{"body":"ledger {\"ticket\":\"B\",\"lane\":[10],\"tier\":false,\"outcome\":\"no_ticket\"}"},{"body":"ledger null"},{"body":"ledger {}\n{}"},{"body":"discussion"}]`
	if err = os.WriteFile(filepath.Join(dir, "comments"), []byte(comments), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, warnings = shellLedgerCommand(t, dir, "stats", "--all")
	want := "runs=2 merged=1 escalated=0 no_ticket=1\navg_corrections=1 ci_reds=2 avg_minutes=3\nlane=[2] runs=1 first_gate_green_pct=100\nlane=[10] runs=1 first_gate_green_pct=0\ntier={} runs=1 first_gate_green_pct=100\n"
	if code != 0 || out != want || strings.Count(warnings, "invalid ledger comment skipped") != 2 || !strings.Contains(warnings, "500-comment") {
		t.Fatalf("stats: %d %q %q", code, out, warnings)
	}
	count, err := os.ReadFile(filepath.Join(dir, "count"))
	if err != nil || strings.Count(string(count), " ledger\n") != 4 {
		t.Fatalf("count=%q error=%v", count, err)
	}
}

func TestLedgerShellFailures(t *testing.T) {
	dir := shellLedgerFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "comments"), []byte(`[{"body":"ledger {\"corrections\":\"bad\"}"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, warnings := shellLedgerCommand(t, dir, "stats")
	if code != 5 || out != "runs=1 merged=0 escalated=0 no_ticket=0\n" || !strings.Contains(warnings, "jq: error") {
		t.Fatalf("type failure: %d %q %q", code, out, warnings)
	}
	code, out, warnings = shellLedgerCommand(t, dir, "append", "{bad")
	if code != 2 || out != "" || !strings.Contains(warnings, "ledger: invalid json row") || !strings.Contains(warnings, "parse error") {
		t.Fatalf("parse failure: %d %q %q", code, out, warnings)
	}
	for name, script := range map[string]string{"orca": "#!/bin/sh\nprintf 'orca diagnostic\\n' >&2\nprintf '{\"ok\":true}'\nexit 7\n", "sleep": "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$LEDGER_TEST_DIR/sleeps\"\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	code, out, warnings = shellLedgerCommand(t, dir, "stats")
	sleeps, err := os.ReadFile(filepath.Join(dir, "sleeps"))
	if code != 1 || out != "" || strings.Count(warnings, "orca diagnostic") != 3 || !strings.Contains(warnings, "attempt 3/3, exit 7") || err != nil || string(sleeps) != "2\n2\n" {
		t.Fatalf("retry: %d %q %q sleeps=%q error=%v", code, out, warnings, sleeps, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"list-issues", "save-issue", "issue", "comment"} {
		script := fmt.Sprintf(`#!/bin/sh
if [ "$2" = %q ] && [ ! -f "$LEDGER_TEST_DIR/seen" ]; then
 touch "$LEDGER_TEST_DIR/seen"
 printf '%%s\n' '{"ok":false,"error":"transient"}'
 exit 0
fi
case "$2" in
list-issues) if [ %q = save-issue ]; then printf '%%s\n' '{"ok":true,"result":{"issues":[]}}'; else printf '%%s\n' '{"ok":true,"result":{"issues":[{"title":"[ledger] agent-graph","identifier":"TEST-1"}]}}'; fi ;;
save-issue) printf '%%s\n' '{"ok":true,"result":{"issue":{"identifier":"TEST-1"}}}' ;;
issue) printf '%%s\n' '{"ok":true,"result":{"comments":[]}}' ;;
comment) printf '%%s\n' '{"ok":true}' ;;
esac
`, operation, operation)
		if err := os.WriteFile(filepath.Join(dir, "orca"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		command, wantCode, wantOut := "append", 0, ""
		if operation == "list-issues" || operation == "issue" {
			command, wantOut = "stats", "runs=0\n"
		}
		if operation == "comment" {
			wantCode = 7
		}
		code, out, warnings = shellLedgerCommand(t, dir, command, "{}")
		if err := os.Remove(filepath.Join(dir, "seen")); err != nil {
			t.Fatal(err)
		}
		if code != wantCode || out != wantOut || !strings.Contains(warnings, "attempt 1/3, exit 0") {
			t.Fatalf("%s sleep failure: %d %q %q", operation, code, out, warnings)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "jq"), []byte("#!/bin/sh\nprintf 'jq boundary diagnostic\\n' >&2\nprintf 'partial\\n'\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, warnings = shellLedgerCommand(t, dir, "append", "{}")
	if code != 2 || out != "" || warnings != "jq boundary diagnostic\nledger: invalid json row\n" {
		t.Fatalf("jq failure: %d %q %q", code, out, warnings)
	}
}
