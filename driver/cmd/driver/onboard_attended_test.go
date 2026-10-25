package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// withTerminal runs body with os.Stdin replaced by the slave side of a fresh
// Linux pseudo-terminal whose input queue already holds script. The line
// discipline stays canonical, so each read returns one line, as when a person
// answers a prompt.
func withTerminal(t *testing.T, script string, body func()) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = master.Close() }()
	unlock := int32(0)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Fatal(errno)
	}
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); errno != 0 {
		t.Fatal(errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slave.Close() }()
	if _, err := master.WriteString(script); err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = slave
	defer func() { os.Stdin = original }()
	body()
}

// TestOnboardAttendedAgentOwnsTerminal drives the wizard (OF-201) on a
// pseudo-terminal through a fresh session. The onboarding agent is an
// interactive Claude Code on that terminal: AskUserQuestion under default
// permissions, the same deny settings, the factory root added, no
// credentials, and the driver still holding the onboarding lock while it
// runs. The PR it opens is read back from gh.
func TestOnboardAttendedAgentOwnsTerminal(t *testing.T) {
	fx := newWizardFixture(t)
	t.Setenv("TERM", "xterm-256color")
	lock := filepath.Join(os.Getenv("AGENT_DRIVER_STATE_ROOT"), "onboard", onboardKey(onboardRemote)+".lock")
	record := filepath.Join(t.TempDir(), "claude.record")
	claude := filepath.Join(t.TempDir(), "claude")
	writeExecutable(t, claude, "#!/bin/sh\n"+
		"for a in \"$@\"; do printf '%s\\0' \"$a\"; done > "+record+".args\n"+
		"{ if [ -t 0 ]; then echo tty=yes; else echo tty=no; fi; printf 'term=%s\\n' \"${TERM-unset}\"; "+
		"printf 'secret=%s\\n' \"${AGENT_FEISHU_APP_SECRET-unset}\"; flock -n "+lock+" true; printf 'flock=%s\\n' \"$?\"; } > "+record+"\n"+
		"printf '%s' "+shQuote(fx.prOpen)+" > "+fx.ghMarker+"\n")
	t.Setenv("AGENT_INTAKE_CLAUDE", claude)

	var out []byte
	var code int
	out = captureStdout(t, func() {
		withTerminal(t, "1\n\nq\n", func() { code = runOnboard(nil) })
	})
	if code != 0 || !strings.Contains(string(out), "pull/7") {
		t.Fatalf("exit=%d, want 0 after quitting at wait-merge on pull/7\noutput=%s", code, out)
	}
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, want := range []string{"tty=yes", "term=xterm-256color", "secret=unset", "flock=1"} {
		if !slices.Contains(lines, want) {
			t.Fatalf("record=%v\nwant %q", lines, want)
		}
	}
	rawArgs, err := os.ReadFile(record + ".args")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(rawArgs), "\x00"), "\x00")
	if slices.Contains(args, "-p") || slices.Contains(args, "--output-format") || !strings.Contains(args[0], "交互模式") {
		t.Fatalf("args=%v, want the 交互模式 prompt first and neither -p nor --output-format", args)
	}
	flag := func(name string) string {
		i := slices.Index(args, name)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("args=%v, missing %s", args, name)
		}
		return args[i+1]
	}
	const tools = "Bash,Read,Edit,Write,Grep,Glob,AskUserQuestion"
	for name, want := range map[string]string{
		"--permission-mode": "default",
		"--tools":           tools,
		"--allowedTools":    tools,
		"--settings":        onboardSettings,
		"--add-dir":         filepath.Join(filepath.Dir(fx.repo), "orca-factory"),
	} {
		if got := flag(name); got != want {
			t.Errorf("%s=%q, want %q", name, got, want)
		}
	}
}

// TestOnboardAttendedAgentRefusesWithoutTerminal covers OF-201's failure
// path: with stdin a pipe the wizard refuses the session stage before it
// writes the ledger row or starts the agent, and points unattended
// onboarding at the Feishu card.
func TestOnboardAttendedAgentRefusesWithoutTerminal(t *testing.T) {
	fx := newWizardFixture(t)
	var out []byte
	var code int
	out = captureStdout(t, func() {
		withStdin(t, "1\n\n", func() { code = runOnboard(nil) })
	})
	if code != 1 || !strings.Contains(string(out), "需要终端") {
		t.Fatalf("exit=%d, want 1 with 需要终端\noutput=%s", code, out)
	}
	if runs := fx.claudeRunCount(t); runs != 0 {
		t.Fatalf("claude runs=%d, want 0", runs)
	}
	if fx.ledger.projectCreates != 0 {
		t.Fatalf("项目表 writes=%d, want 0", fx.ledger.projectCreates)
	}
}
