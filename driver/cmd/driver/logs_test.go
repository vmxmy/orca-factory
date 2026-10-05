package main

import (
	"bufio"
	"bytes"
	"encoding/json"
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
	mentioned := fmt.Sprintf(`{ "time":%q,"level":"WARN","msg":"retry ZII-901","event":"retry" }`+"\n", now.Add(-15*time.Minute).Format(time.RFC3339Nano))
	isolated := row(5*time.Minute, "executor", "inherited-context", "isolated")
	writeLogs(t, filepath.Join(root, "scheduler.log"), last+row(time.Minute, "scheduler", "ZII-9010", "unrelated")+first+mentioned)
	writeLogs(t, filepath.Join(root, "repo", "ZII-901", "executor.log.1"), old)
	writeLogs(t, filepath.Join(root, "repo", "ZII-901", "executor.log"), second+isolated)
	writeLogs(t, filepath.Join(root, "repo", "ZII-902", "executor.log"), row(time.Minute, "executor", "ZII-902", "unrelated"))
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"merged raw JSON", []string{"ZII-901", "--json"}, old + first + second + mentioned + last + isolated},
		{"since", []string{"ZII-901", "--since", "1h", "--json"}, first + second + mentioned + last + isolated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := logsProcess(tc.args...).CombinedOutput()
			if err != nil || string(got) != tc.want {
				t.Fatalf("logs: %v\ngot %s\nwant %s", err, got, tc.want)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(got)), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("not JSON: %q", line)
				}
			}
		})
	}
	t.Run("human", func(t *testing.T) {
		got, err := logsProcess("ZII-901", "--since", "30m").CombinedOutput()
		want := fmt.Sprintf("%s [executor] INFO  event=started attempt=2 ticket=\"ZII-901\"\n%s [scheduler] WARN retry ZII-901 event=retry\n%s [scheduler] INFO  event=observed attempt=2 ticket=\"ZII-901\"\n%s [executor] INFO  event=isolated attempt=2 ticket=\"inherited-context\"\n", now.Add(-20*time.Minute).Format(time.RFC3339Nano), now.Add(-15*time.Minute).Format(time.RFC3339Nano), now.Add(-10*time.Minute).Format(time.RFC3339Nano), now.Add(-5*time.Minute).Format(time.RFC3339Nano))
		if err != nil || string(got) != want {
			t.Fatalf("logs: %v\ngot %s\nwant %s", err, got, want)
		}
	})
	t.Run("since with fixed clock", func(t *testing.T) {
		clockRoot := t.TempDir()
		t.Setenv("AGENT_DRIVER_STATE_ROOT", clockRoot)
		oldLine := `{"time":"2026-10-06T08:59:59Z","msg":"old"}` + "\n"
		newLine := `{"time":"2026-10-06T09:00:01Z","msg":"new"}` + "\n"
		writeLogs(t, filepath.Join(clockRoot, "test-repo", "ZII-901", "executor.log"), newLine+oldLine)
		output, err := os.CreateTemp(t.TempDir(), "stdout")
		if err != nil {
			t.Fatal(err)
		}
		defer output.Close()
		stdout := os.Stdout
		os.Stdout = output
		defer func() { os.Stdout = stdout }()
		code := showLogsAt([]string{"ZII-901", "--since", "1h", "--json"}, time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC))
		os.Stdout = stdout
		got, err := os.ReadFile(output.Name())
		if err != nil || code != 0 || string(got) != newLine {
			t.Fatalf("fixed-clock since: exit %d, %v\ngot %s\nwant %s", code, err, got, newLine)
		}
	})
	for _, args := range [][]string{nil, {"../ZII-901"}, {"ZII-901", "--since", "bad"}, {"ZII-901", "--since", "-1m"}, {"ZII-901", "--unknown"}} {
		if err := logsProcess(args...).Run(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	t.Run("only supported rotation files", func(t *testing.T) {
		rotationRoot := t.TempDir()
		t.Setenv("AGENT_DRIVER_STATE_ROOT", rotationRoot)
		writeLogs(t, filepath.Join(rotationRoot, "scheduler.log.5"), first)
		writeLogs(t, filepath.Join(rotationRoot, "other-repo", "ZII-901", "executor.log.5"), second)
		for _, suffix := range []string{".6", ".backup", ".1.tmp"} {
			writeLogs(t, filepath.Join(rotationRoot, "scheduler.log"+suffix), "not a log\n")
			writeLogs(t, filepath.Join(rotationRoot, "other-repo", "ZII-901", "executor.log"+suffix), "not a log\n")
		}
		got, err := logsProcess("ZII-901", "--json").CombinedOutput()
		if err != nil || string(got) != first+second {
			t.Fatalf("rotation selection: %v\ngot %s\nwant %s", err, got, first+second)
		}
	})
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
		expect(mentioned)
		expect(last)
		expect(isolated)
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
