package ledger

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
)

const ShellContractVersion = "ledger-shell-v1"

// RunShell keeps the installed ledger entry's contract and forwards to `driver ledger`.
func RunShell(args []string, out, warnings io.Writer) int {
	if len(args) == 1 && args[0] == "--contract-version" {
		if _, err := fmt.Fprintln(out, ShellContractVersion); err != nil {
			return 1
		}
		return 0
	}
	if backend := os.Getenv("AGENT_ISSUE_BACKEND"); backend != "github" && backend != "feishu" {
		_, _ = fmt.Fprintln(warnings, "ledger: AGENT_ISSUE_BACKEND must be github or feishu")
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
	return shellProcess(binary, append([]string{"ledger"}, forward...), out, warnings)
}
func shellProcess(path string, args []string, out, warnings io.Writer) int {
	command := exec.Command(path, args...)
	command.Stdin = os.Stdin
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
