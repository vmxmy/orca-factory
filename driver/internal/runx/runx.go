// Package runx bounds external command execution and resolves the trusted Orca binary.
package runx

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Spec describes a command. Timeout must be positive. Env, when non-nil,
// replaces the inherited environment. Args and output are never included in errors.
type Spec struct {
	Path    string
	Args    []string
	Dir     string
	Env     []string
	Timeout time.Duration
}

type commandError struct{ cause error }

func (e *commandError) Error() string { return "command failed" }
func (e *commandError) Unwrap() error { return e.cause }

// Run returns stdout and kills the command's process group on cancellation.
// The parent's deadline takes precedence over a longer spec timeout.
func Run(ctx context.Context, spec Spec) (output []byte, err error) {
	started := time.Now()
	var exit *exec.ExitError
	defer func() {
		code, stderr, level := 0, "", slog.LevelInfo
		if err != nil {
			code, level = -1, slog.LevelWarn
			if exit != nil {
				code, stderr = exit.ExitCode(), string(exit.Stderr)
			}
		}
		LoggerFrom(ctx).Log(ctx, level, "external command", "executable", filepath.Base(spec.Path),
			"args", redactArgs(spec.Args), "exit_code", code, "duration", time.Since(started),
			"stderr", redactStderr(stderr, spec.Args), "timed_out", errors.Is(ctx.Err(), context.DeadlineExceeded))
	}()
	if spec.Timeout <= 0 {
		return nil, errors.New("command requires a positive timeout")
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 100 * time.Millisecond
	output, err = cmd.Output()
	errors.As(err, &exit)
	if ctx.Err() != nil {
		return output, ctx.Err()
	}
	if err != nil {
		return output, &commandError{cause: err}
	}
	return output, nil
}

// OrcaPath ignores PATH. An explicit override must be absolute; otherwise the
// binary is resolved beneath HOME, without requiring it to exist yet.
func OrcaPath() (string, error) {
	path := os.Getenv("AGENT_ORCA_BIN")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("cannot resolve home directory")
		}
		path = filepath.Join(home, ".local", "bin", "orca")
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("orca binary path must be absolute")
	}
	return filepath.Clean(path), nil
}
