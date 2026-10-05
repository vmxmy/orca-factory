package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func putLogFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

// logRecord renders one slog-style JSON line like logx writes.
func logRecord(at, component, level, event, ticket string) string {
	record := `{"time":"` + at + `","level":"` + level + `","msg":"` + event + `","component":"` + component + `","event":"` + event + `"`
	if ticket != "" {
		record += `,"ticket":"` + ticket + `"`
	}
	return record + "}\n"
}

func TestLogsCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "orca-factory")
	dir := filepath.Join(root, "test-repo", "ZII-901")
	putLogFixture(t, filepath.Join(root, "scheduler.log"),
		logRecord("2026-10-05T10:10:00Z", "scheduler", "INFO", "finished", "ZII-901")+
			logRecord("2026-10-05T10:07:00Z", "scheduler", "INFO", "other", "ZII-902")+
			`{"time":"2026-10-05T10:08:00Z","level":"INFO","msg":"skip","component":"scheduler","event":"skip","ticket":"ZII-902","blockers":["ZII-901"]}`+"\n"+
			`{"time":"2026-10-05T10:09:00Z","level":"INFO","msg":"summary","component":"scheduler","event":"summary","err":"merge waiting ZII-901: receipt unconfirmed"}`+"\n"+
			logRecord("2026-10-05T10:05:00Z", "scheduler", "WARN", "review", "ZII-901")+
			logRecord("2026-10-05T10:00:00Z", "scheduler", "INFO", "start", "ZII-901"))
	putLogFixture(t, filepath.Join(root, "scheduler.log.1"),
		logRecord("2026-10-05T09:30:00Z", "scheduler", "INFO", "rotated-claim", "ZII-901")+
			logRecord("2026-10-05T09:31:00Z", "scheduler", "INFO", "rotated-other", "ZII-902"))
	putLogFixture(t, filepath.Join(root, "scheduler.log.old"), logRecord("2026-10-05T07:00:00Z", "scheduler", "INFO", "not-rotation", "ZII-901"))
	// executor.log is per ticket: records without a ticket field (such as the exit record) still belong to it.
	putLogFixture(t, filepath.Join(dir, "executor.log"),
		logRecord("2026-10-05T10:12:00Z", "executor", "INFO", "exit", "")+
			logRecord("2026-10-05T10:02:00Z", "executor", "INFO", "work", "ZII-901"))
	putLogFixture(t, filepath.Join(dir, "executor.log.1"), logRecord("2026-10-05T09:00:00Z", "executor", "INFO", "rotated", "ZII-901"))
	putLogFixture(t, filepath.Join(dir, "executor.log.backup"), logRecord("2026-10-05T08:00:00Z", "executor", "INFO", "backup", "ZII-901"))
	now := time.Date(2026, 10, 5, 10, 15, 0, 0, time.UTC)
	invoke := func(args []string, at time.Time) (int, string, string) {
		var out, stderr bytes.Buffer
		code := runLogsWithContext(context.Background(), args, root, at, &out, &stderr)
		return code, out.String(), stderr.String()
	}
	times := func(output string) []string {
		var result []string
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			if line != "" {
				result = append(result, strings.Fields(line)[0])
			}
		}
		return result
	}
	allTimes := []string{"2026-10-05T09:00:00Z", "2026-10-05T09:30:00Z", "2026-10-05T10:00:00Z", "2026-10-05T10:02:00Z", "2026-10-05T10:05:00Z", "2026-10-05T10:09:00Z", "2026-10-05T10:10:00Z", "2026-10-05T10:12:00Z"}
	t.Run("merge rotations and filter ticket", func(t *testing.T) {
		code, output, stderr := invoke([]string{"ZII-901"}, now)
		if code != 0 || !reflect.DeepEqual(times(output), allTimes) {
			t.Fatalf("code=%d stderr=%s output=%s", code, stderr, output)
		}
		if strings.Contains(output, "ZII-902") || strings.Contains(output, "event=backup") {
			t.Fatalf("unrelated records leaked: %s", output)
		}
		if !strings.Contains(output, "component=scheduler  level=WARN  event=review  msg=review  ticket=ZII-901") {
			t.Fatalf("missing readable fields: %s", output)
		}
		if !strings.Contains(output, "event=exit") {
			t.Fatalf("executor record without a ticket field was dropped: %s", output)
		}
	})
	t.Run("since strict cutoff and zero means all", func(t *testing.T) {
		for _, tc := range []struct {
			args []string
			at   time.Time
			want []string
		}{
			{[]string{"ZII-901", "--since", "30m"}, now, allTimes[2:]},
			{[]string{"ZII-901", "--since=30m"}, now.Add(19*time.Minute + 59*time.Second), allTimes[4:]},
			{[]string{"--since", "10m", "ZII-901"}, now, allTimes[5:]},
			{[]string{"ZII-901", "--since", "0s"}, now, allTimes},
		} {
			code, output, stderr := invoke(tc.args, tc.at)
			if code != 0 || !reflect.DeepEqual(times(output), tc.want) {
				t.Fatalf("args=%v code=%d stderr=%s output=%s", tc.args, code, stderr, output)
			}
		}
	})
	t.Run("json prints original lines in time order", func(t *testing.T) {
		code, output, stderr := invoke([]string{"ZII-901", "--json"}, now)
		lines := strings.Split(strings.TrimSpace(output), "\n")
		if code != 0 || len(lines) != len(allTimes) {
			t.Fatalf("code=%d stderr=%s output=%s", code, stderr, output)
		}
		for i, line := range lines {
			var entry struct{ Time string }
			if json.Unmarshal([]byte(line), &entry) != nil || entry.Time != allTimes[i] {
				t.Fatalf("line %d is not the original record in order: %s", i, line)
			}
		}
	})
	t.Run("real root scheduler JSON preserved", func(t *testing.T) {
		isolated := filepath.Join(t.TempDir(), "orca-factory")
		scheduler := `{"time":"2026-10-05T10:00:00.123Z","component":"scheduler","level":"INFO","event":"claim","ticket":"ZII-901","attempt":9007199254740993,"fields":{"reason":"ready"}}`
		executor := `{"time":"2026-10-05T10:02:00Z","component":"executor","level":"WARN","event":"retry","ticket":"ZII-901","msg":"with spaces"}`
		putLogFixture(t, filepath.Join(isolated, "scheduler.log"), scheduler+"\n"+`{"time":"2026-10-05T10:01:00Z","ticket":"ZII-9010"}`+"\n")
		putLogFixture(t, filepath.Join(isolated, "test-repo", "ZII-901", "executor.log"), executor+"\ninvalid ZII-901\n")
		var output, stderr bytes.Buffer
		code := runLogsWithContext(context.Background(), []string{"ZII-901", "--json"}, isolated, now, &output, &stderr)
		if code != 0 || output.String() != scheduler+"\n"+executor+"\n" {
			t.Fatalf("code=%d stderr=%s JSON changed: %s", code, &stderr, &output)
		}
	})
	t.Run("registered command discovers home without repo config", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("AGENT_DRIVER_STATE_ROOT", "")
		t.Setenv("AGENT_GH_REPO", "")
		putLogFixture(t, filepath.Join(home, ".local", "state", "orca-factory", "test-repo", "ZII-901", "executor.log"), logRecord("2026-10-05T10:00:00Z", "executor", "INFO", "discovered", "ZII-901"))
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		original := os.Stdout
		os.Stdout = writer
		defer func() { os.Stdout = original }()
		code := run([]string{"logs", "ZII-901", "--json"})
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		output, err := io.ReadAll(reader)
		if closeErr := reader.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil || code != 0 || !strings.Contains(string(output), "discovered") {
			t.Fatalf("code=%d err=%v output=%s", code, err, output)
		}
	})
	t.Run("invalid arguments", func(t *testing.T) {
		for _, args := range [][]string{nil, {"ZII-901", "extra"}, {"../ZII-901"}, {"ZII-901", "--since"}, {"ZII-901", "--since", "nonsense"}, {"ZII-901", "--since=-1m"}, {"ZII-901", "--unknown"}} {
			code, _, stderr := invoke(args, now)
			if code != 2 || !strings.Contains(stderr, "Usage:") {
				t.Fatalf("args=%v code=%d stderr=%s", args, code, stderr)
			}
		}
	})
	t.Run("empty logs and output errors", func(t *testing.T) {
		var output, stderr bytes.Buffer
		code := runLogsWithContext(context.Background(), []string{"ZII-903", "--json"}, root, now, &output, &stderr)
		if code != 0 || output.String() != "" {
			t.Fatalf("code=%d output=%s stderr=%s", code, &output, &stderr)
		}
		if code := runLogsWithContext(context.Background(), []string{"ZII-901"}, root, now, logFailWriter{}, &stderr); code != 1 {
			t.Fatalf("output error exit=%d", code)
		}
	})
	t.Run("follow new rows rotation partial writes and cancellation", func(t *testing.T) {
		isolated := filepath.Join(t.TempDir(), "orca-factory")
		path := filepath.Join(isolated, "test-repo", "ZII-901", "executor.log")
		putLogFixture(t, path, logRecord("2026-10-05T10:12:00Z", "executor", "INFO", "initial", "ZII-901"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rows := make(chan string, 20)
		result := make(chan int, 1)
		var stderr bytes.Buffer
		go func() {
			result <- runLogsWithContext(ctx, []string{"ZII-901", "--follow", "--json"}, isolated, now, logRowWriter{rows}, &stderr)
		}()
		read := func(event string) {
			t.Helper()
			select {
			case line := <-rows:
				var entry map[string]any
				if json.Unmarshal([]byte(line), &entry) != nil || entry["event"] != event {
					t.Fatalf("expected %s, got %s", event, line)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("follow did not emit %s", event)
			}
		}
		appendLine := func(line string) {
			t.Helper()
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteString(line)
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("write=%v close=%v", writeErr, closeErr)
			}
		}
		read("initial")
		appendLine(logRecord("2026-10-05T10:12:00Z", "executor", "INFO", "same-time", "ZII-901") + logRecord("2026-10-05T10:02:00Z", "executor", "INFO", "late", "ZII-901"))
		read("late")
		read("same-time")
		if err := os.Rename(path, path+".1"); err != nil {
			t.Fatal(err)
		}
		putLogFixture(t, path, logRecord("2026-10-05T10:13:00Z", "executor", "INFO", "after-rotation", "ZII-901"))
		read("after-rotation")
		appendLine(strings.TrimSuffix(logRecord("2026-10-05T10:14:00Z", "executor", "INFO", "partial", "ZII-901"), "\n"))
		select {
		case line := <-rows:
			t.Fatalf("replayed or incomplete line: %s", line)
		case <-time.After(600 * time.Millisecond):
		}
		appendLine("\n")
		read("partial")
		cancel()
		select {
		case code := <-result:
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation did not stop follow")
		}
		select {
		case line := <-rows:
			t.Fatalf("replayed row: %s", line)
		default:
		}
	})
}

type logFailWriter struct{}

func (logFailWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

type logRowWriter struct{ rows chan<- string }

func (w logRowWriter) Write(data []byte) (int, error) { w.rows <- string(data); return len(data), nil }
