// Package runx bounds external command execution and resolves the trusted Orca binary.
package runx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Spec describes a command. Timeout must be positive. Env, when non-nil,
// replaces the inherited environment. Args and output are never included in errors.
type Spec struct {
	Logger  func(string) // Optional; nil disables logging.
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
func Run(ctx context.Context, spec Spec) ([]byte, error) {
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
	logger := LoggerFromContext(ctx)
	var stderr *stderrCapture
	var args, secrets []string
	if spec.Logger != nil || logger != nil {
		args, secrets = safeArgs(spec.Args)
		stderr = newStderrCapture(secrets)
		cmd.Stderr = stderr
	}
	started := time.Now()
	output, err := cmd.Output()
	if spec.Logger != nil || logger != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			exit.Stderr = stderr.Bytes()
		}
		code := 0
		if err != nil {
			code = -1
			if exit != nil {
				code = exit.ExitCode()
			}
		}
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		if timedOut {
			code = -1
		}
		diagnostic := stderr.logText(secrets)
		elapsed := time.Since(started).Milliseconds()
		if spec.Logger != nil {
			spec.Logger(fmt.Sprintf("runx executable=%q args=%q exit=%d duration_ms=%d stderr=%q timeout=%t", SafeText(filepath.Base(spec.Path)), args, code, elapsed, diagnostic, timedOut))
		}
		if logger != nil {
			flags := []string{}
			for _, arg := range args {
				if arg == "--" {
					break
				}
				flag, _, _ := strings.Cut(arg, "=")
				if strings.HasPrefix(flag, "-") && flag != "-" && flag != "--" && !strings.ContainsAny(flag, " \t\r\n") {
					flags = append(flags, SafeText(flag))
				}
			}
			fields := []any{"executable", SafeText(filepath.Base(spec.Path)), "args", flags, "arg_count", len(spec.Args), "exit_code", code, "elapsed_ms", elapsed, "stderr", diagnostic, "timeout", timedOut}
			if timedOut {
				logger.Warn("runx external call", fields...)
			} else {
				logger.Info("runx external call", fields...)
			}
		}
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
