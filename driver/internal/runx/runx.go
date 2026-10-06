// Package runx bounds external command execution and resolves the trusted Orca binary.
package runx

import (
	"context"
	"errors"
	"fmt"
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
	var captured stderrBuffer
	defer func() {
		code, stderr, level := 0, string(captured.Bytes()), slog.LevelInfo
		if err != nil {
			code, level = -1, slog.LevelWarn
			if exit != nil {
				code = exit.ExitCode()
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
	cmd.Stderr = &captured
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
	if errors.As(err, &exit) {
		exit.Stderr = captured.Bytes()
	}
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

// Match exec.Output's bounded prefix/suffix diagnostics, also on success.
// Retain the tail for callers that classify errors from stderr.
type stderrBuffer struct {
	prefix, suffix []byte
	total          int
}

func (b *stderrBuffer) Write(p []byte) (int, error) {
	const half = 32 << 10
	n := len(p)
	b.total += n
	take := min(half-len(b.prefix), len(p))
	b.prefix = append(b.prefix, p[:take]...)
	p = p[take:]
	if len(p) >= half {
		b.suffix = append(b.suffix[:0], p[len(p)-half:]...)
	} else {
		if len(b.suffix)+len(p) > half {
			b.suffix = b.suffix[len(b.suffix)+len(p)-half:]
		}
		b.suffix = append(b.suffix, p...)
	}
	return n, nil
}
func (b *stderrBuffer) Bytes() []byte {
	out := append([]byte(nil), b.prefix...)
	if omitted := b.total - len(b.prefix) - len(b.suffix); omitted > 0 {
		out = fmt.Appendf(out, "\n... omitting %d bytes ...\n", omitted)
	}
	return append(out, b.suffix...)
}
