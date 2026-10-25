package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// onboardTermGrace is how long the attended session gets to exit after a
// forwarded SIGTERM before it is killed.
const onboardTermGrace = 10 * time.Second

// runAttendedAgent runs the onboarding agent as an interactive Claude Code
// session that owns this terminal until the person exits it (OF-201). It is
// not runx: runx's own process group would leave claude in a background
// group that stops on its first tty read, and its timeout does not fit a
// person who steps away. The driver stays the parent and keeps the
// onboarding lock, whose descriptor is close-on-exec, until claude exits.
// It returns no PR: the wizard reads that back from gh.
func (o onboarder) runAttendedAgent(_ context.Context, plan onboardPlan) (string, error) {
	cmd := exec.Command(o.claude, onboardAgentArgs(plan, o.cfg.FactoryRoot, true)...)
	cmd.Dir, cmd.Env = plan.Worktree, onboardAgentEnv(true)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The terminal already sends SIGINT, SIGQUIT and SIGHUP to claude in the
	// foreground group; the driver only has to survive them. signal.Ignore
	// would not do: SIG_IGN survives exec and claude would inherit it.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("接入会话启动失败：%w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var kill <-chan time.Time
	for {
		select {
		case sig := <-signals:
			if sig == syscall.SIGTERM && kill == nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				kill = time.After(onboardTermGrace)
			}
		case <-kill:
			_ = cmd.Process.Kill()
		case err := <-exited:
			if err == nil {
				return "", nil
			}
			// A claude killed by a signal never restores the raw mode its TUI set.
			if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				stty := exec.Command("stty", "sane")
				stty.Stdin = os.Stdin
				err = errors.Join(err, stty.Run())
			}
			return "", fmt.Errorf("接入会话异常结束：%w", err)
		}
	}
}
