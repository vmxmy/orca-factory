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

func TestLogsCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "orca-factory")
	dir := filepath.Join(root, "test-repo", "ZII-901")
	putLogFixture(t, filepath.Join(dir, "scheduler.log"),
		"2026-10-05T10:10:00Z component=scheduler level=info event=finished ticket=ZII-901\n"+
			"2026-10-05T10:07:00Z component=scheduler level=info event=other ticket=ZII-902\n"+
			"2026-10-05T10:05:00Z component=scheduler level=warn event=review ticket=ZII-901\n"+
			"2026-10-05T10:00:00Z component=scheduler level=info event=start ticket=ZII-901\n")
	putLogFixture(t, filepath.Join(dir, "executor.log"),
		"2026-10-05T10:12:00Z component=executor level=info event=done ticket=ZII-901\n"+
			"2026-10-05T10:02:00Z component=executor level=info event=work ticket=ZII-901\n")
	putLogFixture(t, filepath.Join(dir, "executor.log.1"), "2026-10-05T09:00:00Z component=executor level=info event=rotated ticket=ZII-901\n")
	putLogFixture(t, filepath.Join(dir, "executor.log.backup"), "2026-10-05T08:00:00Z ticket=ZII-901 event=backup\n")
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
	allTimes := []string{"2026-10-05T09:00:00Z", "2026-10-05T10:00:00Z", "2026-10-05T10:02:00Z", "2026-10-05T10:05:00Z", "2026-10-05T10:10:00Z", "2026-10-05T10:12:00Z"}
	t.Run("merge rotations and filter ticket", func(t *testing.T) {
		code, output, stderr := invoke([]string{"ZII-901"}, now)
		if code != 0 || !reflect.DeepEqual(times(output), allTimes) {
			t.Fatalf("code=%d stderr=%s output=%s", code, stderr, output)
		}
		if strings.Contains(output, "ZII-902") || strings.Contains(output, "event=backup") {
			t.Fatalf("unrelated records leaked: %s", output)
		}
		if !strings.Contains(output, "component=scheduler  level=warn  event=review  ticket=ZII-901") {
			t.Fatalf("missing readable fields: %s", output)
		}
	})
	t.Run("since strict cutoff and zero means all", func(t *testing.T) {
		for _, tc := range []struct {
			args []string
			at   time.Time
			want []string
		}{
			{[]string{"ZII-901", "--since", "30m"}, now, allTimes[1:]},
			{[]string{"ZII-901", "--since=30m"}, now.Add(19*time.Minute + 59*time.Second), allTimes[3:]},
			{[]string{"--since", "10m", "ZII-901"}, now, allTimes[4:]},
			{[]string{"ZII-901", "--since", "0s"}, now, allTimes},
		} {
			code, output, stderr := invoke(tc.args, tc.at)
			if code != 0 || !reflect.DeepEqual(times(output), tc.want) {
				t.Fatalf("args=%v code=%d stderr=%s output=%s", tc.args, code, stderr, output)
			}
		}
	})
	t.Run("json array retains raw lines and fields", func(t *testing.T) {
		code, output, stderr := invoke([]string{"ZII-901", "--json"}, now)
		var entries []map[string]json.RawMessage
		if code != 0 || json.Unmarshal([]byte(output), &entries) != nil || len(entries) != 6 {
			t.Fatalf("code=%d stderr=%s output=%s", code, stderr, output)
		}
		for i, entry := range entries {
			for _, key := range []string{"time", "component", "level", "event", "fields", "raw"} {
				if _, ok := entry[key]; !ok {
					t.Fatalf("entry missing %s: %s", key, output)
				}
			}
			var at, raw string
			if err := json.Unmarshal(entry["time"], &at); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(entry["raw"], &raw); err != nil {
				t.Fatal(err)
			}
			if at != allTimes[i] || !strings.HasPrefix(raw, at) {
				t.Fatalf("incorrect ordering or raw line: %s", output)
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
		var records []json.RawMessage
		if code != 0 || json.Unmarshal(output.Bytes(), &records) != nil || len(records) != 2 {
			t.Fatalf("code=%d stderr=%s output=%s", code, &stderr, &output)
		}
		if string(records[0]) != scheduler || string(records[1]) != executor {
			t.Fatalf("JSON changed: %s", &output)
		}
	})
	t.Run("registered command discovers home without repo config", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("AGENT_DRIVER_STATE_ROOT", "")
		t.Setenv("AGENT_GH_REPO", "")
		putLogFixture(t, filepath.Join(home, ".local", "state", "orca-factory", "test-repo", "ZII-901", "executor.log"), "2026-10-05T10:00:00Z ticket=ZII-901 event=discovered\n")
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
		if code != 0 || strings.TrimSpace(output.String()) != "[]" {
			t.Fatalf("code=%d output=%s stderr=%s", code, &output, &stderr)
		}
		if code := runLogsWithContext(context.Background(), []string{"ZII-901"}, root, now, logFailWriter{}, &stderr); code != 1 {
			t.Fatalf("output error exit=%d", code)
		}
	})
	t.Run("follow new rows rotation partial writes and cancellation", func(t *testing.T) {
		isolated := filepath.Join(t.TempDir(), "orca-factory")
		path := filepath.Join(isolated, "test-repo", "ZII-901", "executor.log")
		putLogFixture(t, path, "2026-10-05T10:12:00Z component=executor ticket=ZII-901 event=initial\n")
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
		appendLine("2026-10-05T10:12:00Z ticket=ZII-901 event=same-time\n2026-10-05T10:02:00Z ticket=ZII-901 event=late\n")
		read("late")
		read("same-time")
		if err := os.Rename(path, path+".1"); err != nil {
			t.Fatal(err)
		}
		putLogFixture(t, path, "2026-10-05T10:13:00Z ticket=ZII-901 event=after-rotation\n")
		read("after-rotation")
		appendLine("2026-10-05T10:14:00Z ticket=ZII-901 event=partial")
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
