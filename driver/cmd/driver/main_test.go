package main

import (
	"bytes"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/testsupport"
)

func TestMain(m *testing.M) {
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
