package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		ticket string
	}{
		{"empty", []string{"exec"}, 2, "unknown"},
		{"ordinary", []string{"exec", "ENG-1"}, 1, "ENG-1"},
		{"merging", []string{"exec", "--merging", "ENG-1"}, 1, "ENG-1"},
		{"missing ticket", []string{"exec", "--merging"}, 2, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--"}, tc.args...)...)
			cmd.Dir = home
			cmd.Env = append(os.Environ(), "DRIVER_TEST_PROCESS=1", "HOME="+home, "ORCA_PANE_KEY=", "ORCA_TERMINAL_HANDLE=test-terminal", "PATH="+home)
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					code = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code {
				t.Fatalf("exit=%d want=%d output=%s", code, tc.code, out)
			}
			if tc.code == 2 && !strings.Contains(string(out), "usage:") {
				t.Fatalf("missing usage: %s", out)
			}
			if tc.code == 1 && strings.Contains(string(out), "usage:") {
				t.Fatalf("valid args rejected: %s", out)
			}
			path := filepath.Join(home, ".local", "state", "orca-factory", "unknown", tc.ticket, "executor.log")
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.code == 1 && !strings.Contains(string(raw), `"executable":"git"`) {
				t.Fatalf("missing external command log: %s", raw)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("wrong log path: %v", err)
			}
		})
	}
}
