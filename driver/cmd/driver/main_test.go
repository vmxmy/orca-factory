package main

import (
	"bytes"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/testsupport"
)

func TestMain(m *testing.M) {
	if os.Getenv("DRIVER_TEST_PROCESS") == "1" {
		if !slices.Contains(os.Args, "--") {
			// os.Executable re-invoked the test binary as the driver itself.
			os.Exit(run(os.Args[1:]))
		}
		os.Exit(m.Run())
	}
	cleanup := testsupport.IsolateStatus()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestCLIProcess(t *testing.T) {
	if os.Getenv("DRIVER_TEST_PROCESS") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"driver"}, os.Args[i+1:]...)
				main()
				return
			}
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"version", []string{"version"}, 0},
		{"unknown", []string{"missing"}, 2},
		{"no arguments", nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-test.run=^TestCLIProcess$", "--"}, tc.args...)
			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(os.Environ(), "DRIVER_TEST_PROCESS=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code {
				t.Fatalf("exit = %d; want %d; stderr = %q", code, tc.code, stderr.String())
			}
			if tc.code == 0 {
				if stdout.String() != "orca-factory-driver dev\n" || stderr.Len() != 0 {
					t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
				}
			} else if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage:") {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestTopLevelHelpListsEveryCommand(t *testing.T) {
	if os.Getenv("DRIVER_TEST_PROCESS") == "1" {
		t.Skip("parent process only")
	}
	for _, flag := range []string{"--help", "-h", "help"} {
		t.Run(flag, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestCLIProcess$", "--", flag)
			cmd.Env = append(os.Environ(), "DRIVER_TEST_PROCESS=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("exit error = %v; stderr = %q", err, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("help wrote stderr %q", stderr.String())
			}
			listed := map[string]bool{}
			for _, line := range strings.Split(stdout.String(), "\n") {
				if name, ok := strings.CutPrefix(line, "  "); ok && name != "" && name[0] != ' ' {
					listed[strings.Fields(name)[0]] = true
				}
			}
			for name := range commands {
				if !listed[name] {
					t.Errorf("help does not list command %q", name)
				}
			}
		})
	}
}

func TestRegisterDispatch(t *testing.T) {
	var got []string
	Register("test-command", func(args []string) int { got = args; return 7 })
	defer delete(commands, "test-command")
	if code := run([]string{"test-command", "one", "two"}); code != 7 {
		t.Fatalf("exit = %d; want 7", code)
	}
	if !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("args = %v", got)
	}
}
