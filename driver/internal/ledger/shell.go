package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
)

const ShellContractVersion = "ledger-shell-v1"

// RunShell preserves the installed ledger entry's synchronous Orca and jq contract.
func RunShell(args []string, out, warnings io.Writer) int {
	if len(args) == 1 && args[0] == "--contract-version" {
		if _, err := fmt.Fprintln(out, ShellContractVersion); err != nil {
			return 1
		}
		return 0
	}
	backend := os.Getenv("AGENT_ISSUE_BACKEND")
	if backend != "" && backend != "linear" && backend != "github" && backend != "feishu" {
		_, _ = fmt.Fprintln(warnings, "ledger: unknown backend")
		return 2
	}
	if len(args) > 0 && args[0] == "append" {
		release, err := maintenance.Write()
		if err != nil {
			_, _ = fmt.Fprintln(warnings, err)
			return 1
		}
		defer release()
	}
	if backend == "github" || backend == "feishu" {
		binary, err := os.Executable()
		if err != nil {
			_, _ = fmt.Fprintln(warnings, err)
			return 1
		}
		forward := args
		if len(args) == 1 && args[0] == "append" {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				_, _ = fmt.Fprintln(warnings, err)
				return 1
			}
			forward = []string{"append", string(raw)}
		}
		return shellProcess(binary, append([]string{"ledger"}, forward...), "", out, warnings)
	}
	for _, key := range []string{"AGENT_LINEAR_TEAM", "AGENT_LINEAR_PROJECT"} {
		if os.Getenv(key) == "" {
			_, _ = fmt.Fprintf(warnings, "ledger: run via agent-graph.sh: %s required\n", key)
			return 1
		}
	}
	shell := shellLedger{out: out, warnings: warnings}
	if len(args) == 0 || (args[0] != "append" && args[0] != "stats") {
		_, _ = fmt.Fprintln(warnings, "usage: ledger.sh append '<json>' | ledger.sh stats (Linear comment storage)")
		return 2
	}
	row := ""
	if args[0] == "append" {
		input := ""
		if len(args) > 1 {
			input = args[1]
		}
		var code int
		row, code = shell.jq(input, warnings, "-c", ".")
		if code != 0 || row == "" {
			_, _ = fmt.Fprintln(warnings, "ledger: invalid json row")
			return 2
		}
	}
	issue, code := shell.findIssue()
	if code != 0 {
		return code
	}
	if args[0] == "stats" {
		return shell.stats(issue)
	}
	return shell.append(issue, row)
}

type shellLedger struct{ out, warnings io.Writer }

func shellProcess(path string, args []string, input string, out, warnings io.Writer) int {
	command := exec.Command(path, args...)
	if errors.Is(command.Err, exec.ErrDot) {
		command.Err = nil
	}
	command.Stdin = os.Stdin
	if input != "" {
		command.Stdin = strings.NewReader(input)
	}
	command.Stdout, command.Stderr = out, warnings
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return 128 + int(status.Signal())
			}
			return exit.ExitCode()
		}
		_, _ = fmt.Fprintf(warnings, "ledger: %s: %v\n", path, err)
		if os.IsPermission(err) {
			return 126
		}
		return 127
	}
	return 0
}

func (s shellLedger) jq(input string, warnings io.Writer, args ...string) (string, int) {
	var out bytes.Buffer
	code := shellProcess("jq", args, input+"\n", &out, warnings)
	return strings.TrimRight(out.String(), "\n"), code
}

func (s shellLedger) count() {
	var timestamp bytes.Buffer
	shellProcess("date", []string{"+%s"}, "", &timestamp, io.Discard)
	path := os.Getenv("AGENT_LINEAR_COUNT_FILE")
	if path == "" {
		root := os.Getenv("TMPDIR")
		if root == "" {
			root = "/tmp"
		}
		path = root + "/orca-factory-linear-calls"
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(file, "%s ledger\n", strings.TrimRight(timestamp.String(), "\n"))
	_ = file.Close()
}

func (s shellLedger) linear(args ...string) (string, int) {
	for attempt := 1; attempt <= 3; attempt++ {
		s.count()
		var out bytes.Buffer
		status := shellProcess("orca", append([]string{"linear"}, args...), "", &out, s.warnings)
		response := strings.TrimRight(out.String(), "\n")
		if status == 0 {
			_, code := s.jq(response, io.Discard, "-e", ".ok == true")
			if code == 0 {
				return response, 0
			}
		}
		reason, code := s.jq(response, io.Discard, "-c", ".error // .message // .")
		if code != 0 {
			reason = response
		}
		if reason == "" {
			reason = "empty response"
		}
		_, _ = fmt.Fprintf(s.warnings, "ledger: orca linear %s failed (attempt %d/3, exit %d): %s\n", args[0], attempt, status, reason)
		if attempt != 3 {
			if code := shellProcess("sleep", []string{"2"}, "", s.out, s.warnings); code != 0 && args[0] == "comment" {
				return "", code
			}
		}
	}
	_, _ = fmt.Fprintf(s.warnings, "ledger: orca linear %s failed after 3 attempts; ledger operation aborted\n", args[0])
	return "", 1
}

func (s shellLedger) findIssue() (string, int) {
	response, code := s.linear("list-issues", "--team", os.Getenv("AGENT_LINEAR_TEAM"), "--project", os.Getenv("AGENT_LINEAR_PROJECT"), "--query", Title, "--json")
	if code != 0 {
		return "", 1
	}
	return s.jq(response, s.warnings, "-r", `[.result.issues[] | select(.title == "[ledger] agent-graph") | (.identifier // .id)][0] // empty`)
}

func (s shellLedger) append(issue, row string) int {
	if issue == "" {
		response, code := s.linear("save-issue", "--team", os.Getenv("AGENT_LINEAR_TEAM"), "--project", os.Getenv("AGENT_LINEAR_PROJECT"), "--title", Title, "--state", "Backlog", "--json")
		if code != 0 {
			return code
		}
		issue, code = s.jq(response, s.warnings, "-er", `.result.issue | .identifier // .id // empty`)
		if code != 0 {
			_, _ = fmt.Fprintln(s.warnings, "ledger: save-issue returned no ledger issue identifier")
			return 1
		}
	}
	_, code := s.linear("comment", "add", issue, "--body", "ledger "+row, "--json")
	return code
}

func (s shellLedger) stats(issue string) int {
	if issue == "" {
		if _, err := fmt.Fprintln(s.out, "runs=0"); err != nil {
			return 1
		}
		return 0
	}
	response, code := s.linear("issue", issue, "--comments", "--json")
	if code != 0 {
		return code
	}
	_, capCode := s.jq(response, s.warnings, "-e", `.result.meta.sections.comments | .hasMore == true or .capReached == true`)
	if capCode == 0 {
		_, _ = fmt.Fprintln(s.warnings, "ledger: warning: comments reached the 500-comment read cap or have more pages; stats may be incomplete")
	}
	rows, code := s.jq(response, s.warnings, "-c", shellRows)
	if code != 0 {
		return code
	}
	if rows == "[]" {
		if _, err := fmt.Fprintln(s.out, "runs=0"); err != nil {
			return 1
		}
		return 0
	}
	return shellProcess("jq", []string{"-r", shellStats}, rows+"\n", s.out, s.warnings)
}

const shellRows = `
[.result.comments[].body | select(type == "string") | select(startswith("ledger "))
 | ltrimstr("ledger ")
 | try (fromjson | if type == "object" then . else error("row is not an object") end)
   catch ("ledger: warning: invalid ledger comment skipped" | . + "\n" | stderr | empty)]
| unique_by([.ticket, .started_at])`

const shellStats = `
def avg: if length == 0 then 0 else (add / length * 100 | round) / 100 end;
def pct: if length == 0 then 0 else (map(select(. == "green")) | length) * 100 / length | round end;
"runs=\(length) merged=\(map(select(.outcome == "merged")) | length) escalated=\(map(select(.outcome == "escalated")) | length) no_ticket=\(map(select(.outcome == "no_ticket")) | length)",
"avg_corrections=\(map(.corrections // empty) | avg) ci_reds=\(map(.ci_reds // 0) | add // 0) avg_minutes=\(map(.minutes // empty) | avg)",
(map(select(.lane != null)) | group_by(.lane)[]
 | "lane=\(.[0].lane) runs=\(length) first_gate_green_pct=\(map(.first_gate // empty) | pct)"),
(map(select(.outcome != "no_ticket")) | group_by(.tier // "unknown")[]
 | "tier=\(.[0].tier // "unknown") runs=\(length) first_gate_green_pct=\(map(.first_gate // empty) | pct)")`
