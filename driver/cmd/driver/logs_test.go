package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestLogsCommand(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_DRIVER_STATE_ROOT", root)
	dir := filepath.Join(root, "repo", "ZII-901")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := `{"time":"` + now.Add(-2*time.Hour).Format(time.RFC3339Nano) + `","component":"executor","level":"INFO","event":"old","msg":"old executor"}`
	scheduled := `{"time":"` + now.Add(-2*time.Minute).Format(time.RFC3339Nano) + `","component":"scheduler","level":"INFO","event":"scheduled","ticket":"ZII-901","msg":"scheduled executor"}`
	other := `{"time":"` + now.Add(-90*time.Second).Format(time.RFC3339Nano) + `","component":"scheduler","level":"INFO","event":"other","ticket":"ZII-902","msg":"ZII-901 is mentioned but not this ticket"}`
	live := `{"time":"` + now.Add(-time.Minute).Format(time.RFC3339Nano) + `", "component":"executor","level":"INFO","event":"live","msg":"live\nexecutor"}`
	for path, content := range map[string]string{
		filepath.Join(root, "scheduler.log"): scheduled + "\n" + other + "\n",
		filepath.Join(dir, "executor.log.1"): old + "\n",
		filepath.Join(dir, "executor.log"):   live + "\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	capture := func(args ...string) string {
		t.Helper()
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stdout
		os.Stdout = writer
		defer func() { os.Stdout = previous }()
		code := run(append([]string{"logs"}, args...))
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		output, err := io.ReadAll(reader)
		if closeErr := reader.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if code != 0 {
			t.Fatalf("logs exit = %d; output = %q", code, output)
		}
		return string(output)
	}
	output := capture("ZII-901")
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "executor INFO old") || !strings.Contains(lines[1], "scheduler INFO scheduled") || !strings.Contains(lines[2], "executor INFO live") || strings.Contains(output, "ZII-902") {
		t.Fatalf("want only this ticket's three records in time order; got %q", output)
	}
	output = capture("ZII-901", "--since", "1h")
	if strings.Contains(output, "old") || !strings.Contains(output, "scheduled") || !strings.Contains(output, "live") || strings.Count(output, "\n") != 2 {
		t.Fatalf("since did not keep only the recent records: %q", output)
	}
	if output = capture("--json", "ZII-901"); output != old+"\n"+scheduled+"\n"+live+"\n" {
		t.Fatalf("JSON output changed original lines: %q", output)
	}
}

func TestLogsFollowRotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "scheduler.log")
		record := func(event string) string {
			return `{"time":"2026-10-05T12:00:00Z","ticket":"ZII-901","event":"` + event + `"}` + "\n"
		}
		if err := os.WriteFile(path, []byte(record("first")), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var output bytes.Buffer
		done := make(chan error, 1)
		go func() { done <- streamLogs(ctx, &output, root, "ZII-901", time.Time{}, true, true) }()
		synctest.Wait()
		if err := os.WriteFile(path, []byte(record("first")+record("before-rotation")), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, path+".1"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(record("after-rotation")), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		for _, event := range []string{"first", "before-rotation", "after-rotation"} {
			if strings.Count(output.String(), record(event)) != 1 {
				t.Fatalf("event %s missing or repeated: %s", event, output.String())
			}
		}
	})
}
