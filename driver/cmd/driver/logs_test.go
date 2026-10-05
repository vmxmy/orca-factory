package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func logsProcess(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--", "logs"}, args...)...)
	cmd.Env = append(os.Environ(), "DRIVER_TEST_PROCESS=1")
	return cmd
}

func writeLogs(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLogsCommand(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_DRIVER_STATE_ROOT", root)
	now := time.Now().UTC()
	row := func(age time.Duration, component, ticket, event string) string {
		return fmt.Sprintf(`{ "time":%q,"component":%q,"level":"INFO","event":%q,"ticket":%q,"attempt":2 }`+"\n", now.Add(-age).Format(time.RFC3339Nano), component, event, ticket)
	}
	old := row(2*time.Hour, "executor", "ZII-901", "old")
	first := row(40*time.Minute, "scheduler", "ZII-901", "claimed")
	second := row(20*time.Minute, "executor", "ZII-901", "started")
	last := row(10*time.Minute, "scheduler", "ZII-901", "observed")
	writeLogs(t, filepath.Join(root, "scheduler.log"), last+row(time.Minute, "scheduler", "ZII-9010", "unrelated")+first)
	writeLogs(t, filepath.Join(root, "repo", "ZII-901", "executor.log.1"), old)
	writeLogs(t, filepath.Join(root, "repo", "ZII-901", "executor.log"), second)
	writeLogs(t, filepath.Join(root, "repo", "ZII-902", "executor.log"), row(time.Minute, "executor", "ZII-902", "unrelated"))
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"merged raw JSON", []string{"ZII-901", "--json"}, old + first + second + last},
		{"since", []string{"ZII-901", "--since", "1h", "--json"}, first + second + last},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := logsProcess(tc.args...).CombinedOutput()
			if err != nil || string(got) != tc.want {
				t.Fatalf("logs: %v\ngot %s\nwant %s", err, got, tc.want)
			}
		})
	}
	t.Run("human", func(t *testing.T) {
		got, err := logsProcess("ZII-901", "--since", "30m").CombinedOutput()
		want := fmt.Sprintf("%s executor INFO started attempt=2 ticket=\"ZII-901\"\n%s scheduler INFO observed attempt=2 ticket=\"ZII-901\"\n", now.Add(-20*time.Minute).Format(time.RFC3339Nano), now.Add(-10*time.Minute).Format(time.RFC3339Nano))
		if err != nil || string(got) != want {
			t.Fatalf("logs: %v\ngot %s\nwant %s", err, got, want)
		}
	})
	for _, args := range [][]string{nil, {"../ZII-901"}, {"ZII-901", "--since", "bad"}, {"ZII-901", "--since", "-1m"}, {"ZII-901", "--unknown"}} {
		if err := logsProcess(args...).Run(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	t.Run("follow rotation and partial writes", func(t *testing.T) {
		cmd := logsProcess("ZII-901", "--json", "--follow", "--since", "30m")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()
		lines := make(chan string, 20)
		go func() {
			defer close(lines)
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				lines <- scanner.Text() + "\n"
			}
		}()
		expect := func(want string) {
			t.Helper()
			select {
			case got := <-lines:
				if got != want {
					t.Fatalf("got %q want %q", got, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("follow timed out")
			}
		}
		expect(second)
		expect(last)
		path := filepath.Join(root, "repo", "ZII-901", "executor.log")
		if err := os.Rename(path, path+".1"); err != nil {
			t.Fatal(err)
		}
		next := row(0, "executor", "ZII-901", "finished")
		writeLogs(t, path, strings.TrimSuffix(next, "\n"))
		select {
		case got := <-lines:
			t.Fatalf("emitted partial or duplicate row %q", got)
		case <-time.After(350 * time.Millisecond):
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.WriteString("\n"); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		expect(next)
		if err = cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err = <-done:
			if err != nil {
				t.Fatalf("interrupt: %v %s", err, stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("interrupt did not stop follow")
		}
	})
}
