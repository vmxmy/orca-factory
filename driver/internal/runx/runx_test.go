package runx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunTimeout(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "spec", true: "context"}[parent], func(t *testing.T) {
			ctx := context.Background()
			timeout := 40 * time.Millisecond
			if parent {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
				timeout = time.Second
			}
			start := time.Now()
			_, err := Run(ctx, Spec{Path: "/bin/sleep", Args: []string{"10"}, Timeout: timeout})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("command not killed promptly")
			}
		})
	}
}
func TestRunOutputAndSafeError(t *testing.T) {
	got, err := Run(context.Background(), Spec{Path: "/bin/sh", Args: []string{"-c", "printf hello"}, Timeout: time.Second})
	if err != nil || string(got) != "hello" {
		t.Fatalf("got %q, %v", got, err)
	}
	_, err = Run(context.Background(), Spec{Path: "/bin/sh", Args: []string{"-c", "echo credential >&2; exit 7"}, Timeout: time.Second})
	if err == nil || err.Error() != "command failed" {
		t.Fatalf("unsafe error: %v", err)
	}
	_, err = Run(context.Background(), Spec{Path: "/bin/sleep", Args: []string{"10"}})
	if err == nil {
		t.Fatal("missing timeout accepted")
	}
}

func TestRunGitDisablesTerminalPrompt(t *testing.T) {
	t.Setenv("GIT_TERMINAL_PROMPT", "")
	home := t.TempDir()
	for _, tc := range []struct {
		name   string
		path   string
		env    []string
		prompt string
		home   string
	}{
		{name: "git inherited environment", path: "git", prompt: "0", home: os.Getenv("HOME")},
		{name: "git explicit environment", path: "git", env: []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, prompt: "0", home: home},
		{name: "non git", path: "other", home: os.Getenv("HOME")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.path)
			if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n%s\\n' \"$GIT_TERMINAL_PROMPT\" \"$HOME\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			got, err := Run(context.Background(), Spec{Path: path, Env: tc.env, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.prompt + "\n" + tc.home + "\n"; string(got) != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestOrcaPathIgnoresPATH(t *testing.T) {
	home := t.TempDir()
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "orca"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", fake)
	t.Setenv("AGENT_ORCA_BIN", "")
	got, err := OrcaPath()
	want := filepath.Join(home, ".local/bin/orca")
	if err != nil || got != want || !filepath.IsAbs(got) {
		t.Fatalf("got %q %v", got, err)
	}
	t.Setenv("AGENT_ORCA_BIN", filepath.Join(home, "custom/orca"))
	got, err = OrcaPath()
	if err != nil || got != filepath.Join(home, "custom/orca") {
		t.Fatalf("got %q %v", got, err)
	}
	t.Setenv("AGENT_ORCA_BIN", "relative/orca")
	if _, err = OrcaPath(); err == nil {
		t.Fatal("relative override accepted")
	}
}
