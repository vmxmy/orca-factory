package main

import (
	"bufio"
	"bytes"
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

func putLogFixture(t *testing.T, path, body string) {
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
	t.Setenv("AGENT_GH_REPO", "")
	const repo = "vmxmy/orca-factory"
	now := time.Now().UTC()
	row := func(age time.Duration, component, ticketKey, ticket, message string) string {
		fields := map[string]any{"time": now.Add(-age).Format(time.RFC3339Nano), "component": component, "level": "INFO", "event": "progress", "msg": message, "attempt": 2}
		if ticketKey != "" {
			fields[ticketKey] = ticket
		}
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(data) + "\n"
	}
	schedulerOld := row(40*time.Minute, "scheduler", "ticket_id", "ZII-901", "claimed")
	schedulerNew := row(10*time.Minute, "scheduler", "issue", "ZII-901", "observed")
	unrelated := row(time.Minute, "scheduler", "ticket", "ZII-100", "unrelated ZII-901")
	rotation2 := row(2*time.Hour, "executor", "", "", "rotation-two")
	rotation1 := row(50*time.Minute, "executor", "ticket", "inherited-context", "rotation-one")
	executorFirst := row(20*time.Minute, "executor", "", "", "started")
	executorLast := row(5*time.Minute, "executor", "", "", "finished")
	putLogFixture(t, filepath.Join(root, "scheduler.log"), schedulerNew+unrelated+schedulerOld)
	dir := filepath.Join(root, repo, "ZII-901")
	putLogFixture(t, filepath.Join(dir, "executor.log"), executorLast+executorFirst)
	putLogFixture(t, filepath.Join(dir, "executor.log.1"), rotation1)
	putLogFixture(t, filepath.Join(dir, "executor.log.2"), rotation2)

	var output bytes.Buffer
	if code := writeLogs(&output, []string{"--repo", repo, "ZII-901"}); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	human := output.String()
	lines := strings.Split(strings.TrimSpace(human), "\n")

	t.Run("time order", func(t *testing.T) {
		if len(lines) != 6 {
			t.Fatalf("got %d lines, want 6: %s", len(lines), human)
		}
		var previous time.Time
		for _, line := range lines {
			at, err := time.Parse(time.RFC3339, strings.Fields(line)[0])
			if err != nil {
				t.Fatal(err)
			}
			if at.Before(previous) {
				t.Fatalf("%s precedes %s", at, previous)
			}
			previous = at
		}
	})
	t.Run("only requested scheduler ticket", func(t *testing.T) {
		if strings.Contains(human, "ZII-100") || strings.Contains(human, "unrelated") {
			t.Fatalf("other ticket included: %s", human)
		}
		if !strings.Contains(human, "ZII-901") {
			t.Fatalf("requested ticket missing: %s", human)
		}
		if !strings.Contains(human, "claimed") || !strings.Contains(human, "observed") {
			t.Fatalf("ticket aliases missing: %s", human)
		}
		if !strings.Contains(human, "inherited-context") {
			t.Fatalf("executor context filtered: %s", human)
		}
	})
	t.Run("sources interleave", func(t *testing.T) {
		var components []string
		for _, line := range lines {
			components = append(components, strings.Fields(line)[1])
		}
		want := []string{"executor", "executor", "scheduler", "executor", "scheduler", "executor"}
		if !reflect.DeepEqual(components, want) {
			t.Fatalf("components=%v, want %v", components, want)
		}
	})
	t.Run("all rotations", func(t *testing.T) {
		if !strings.Contains(human, "rotation-one") {
			t.Fatal("executor.log.1 missing")
		}
		if !strings.Contains(human, "rotation-two") {
			t.Fatal("executor.log.2 missing")
		}
	})
	t.Run("human fields", func(t *testing.T) {
		for _, line := range lines {
			if !strings.Contains(line, " INFO progress attempt=2 ") || !strings.Contains(line, "msg=") {
				t.Fatalf("headings or sorted fields wrong: %s", line)
			}
			previous := ""
			for _, field := range strings.Fields(line)[4:] {
				key, _, _ := strings.Cut(field, "=")
				if key < previous {
					t.Fatalf("fields not sorted: %s", line)
				}
				previous = key
			}
			for _, duplicate := range []string{"time=", "component=", "level=", "event="} {
				if strings.Contains(line, duplicate) {
					t.Fatalf("duplicate heading %s: %s", duplicate, line)
				}
			}
		}
	})
	t.Run("since 30m", func(t *testing.T) {
		var recent bytes.Buffer
		if code := writeLogs(&recent, []string{"ZII-901", "--repo", repo, "--since", "30m"}); code != 0 {
			t.Fatalf("exit=%d", code)
		}
		got := recent.String()
		if strings.Contains(got, "rotation-") || strings.Contains(got, "claimed") {
			t.Fatalf("old rows included: %s", got)
		}
		if !strings.Contains(got, "started") || !strings.Contains(got, "observed") || !strings.Contains(got, "finished") {
			t.Fatalf("recent rows missing: %s", got)
		}
		if strings.Count(got, "\n") != 3 {
			t.Fatalf("want three recent rows: %s", got)
		}
	})
	t.Run("JSON preserves fields", func(t *testing.T) {
		var raw bytes.Buffer
		if code := writeLogs(&raw, []string{"ZII-901", "--json", "--repo", repo}); code != 0 {
			t.Fatalf("exit=%d", code)
		}
		jsonLines := strings.Split(strings.TrimSpace(raw.String()), "\n")
		if len(jsonLines) != 6 {
			t.Fatalf("JSON rows=%d, want 6", len(jsonLines))
		}
		fixtures := []string{rotation2, rotation1, schedulerOld, executorFirst, schedulerNew, executorLast}
		for i, line := range jsonLines {
			var got, want map[string]any
			if err := json.Unmarshal([]byte(line), &got); err != nil {
				t.Fatal(err)
			}
			if _, ok := got["time"]; !ok {
				t.Fatalf("time missing: %s", line)
			}
			if err := json.Unmarshal([]byte(fixtures[i]), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("changed JSON fields: got %v, want %v", got, want)
			}
		}
	})
	t.Run("malformed rows skipped", func(t *testing.T) {
		putLogFixture(t, filepath.Join(dir, "executor.log"), "not JSON\n{\"msg\":\"missing-time\"}\n{\"time\":\"bad\"}\n"+executorFirst)
		var got bytes.Buffer
		if code := writeLogs(&got, []string{"--repo", repo, "ZII-901"}); code != 0 {
			t.Fatalf("exit=%d", code)
		}
		if strings.Count(got.String(), "\n") != 5 || !strings.Contains(got.String(), "started") {
			t.Fatalf("bad-row handling: %s", got.String())
		}
	})
	t.Run("arbitrary numeric rotations", func(t *testing.T) {
		isolated := t.TempDir()
		t.Setenv("AGENT_DRIVER_STATE_ROOT", isolated)
		for _, name := range []string{"executor.log.2", "executor.log.10", "executor.log"} {
			putLogFixture(t, filepath.Join(isolated, repo, "ZII-901", name), row(time.Minute, "executor", "", "", name))
		}
		putLogFixture(t, filepath.Join(isolated, repo, "ZII-901", "executor.log.backup"), row(time.Minute, "executor", "", "", "backup"))
		var got bytes.Buffer
		if code := writeLogs(&got, []string{"ZII-901", "--repo", repo, "--json"}); code != 0 {
			t.Fatalf("exit=%d", code)
		}
		var messages []string
		for _, line := range strings.Split(strings.TrimSpace(got.String()), "\n") {
			var fields map[string]any
			if err := json.Unmarshal([]byte(line), &fields); err != nil {
				t.Fatal(err)
			}
			messages = append(messages, fields["msg"].(string))
		}
		if !reflect.DeepEqual(messages, []string{"executor.log.10", "executor.log.2", "executor.log"}) {
			t.Fatalf("rotation order=%v", messages)
		}
	})
	t.Run("usage errors", func(t *testing.T) {
		for _, args := range [][]string{nil, {"ZII-901"}, {"--repo", repo}, {"--repo", repo, "../ZII-901"}, {"--repo", repo, "ZII-901", "--since", "bad"}, {"--repo", repo, "ZII-901", "--since", "-1m"}, {"--repo", repo, "ZII-901", "--unknown"}} {
			var got bytes.Buffer
			if code := writeLogs(&got, args); code != 2 {
				t.Fatalf("args=%v exit=%d, want 2", args, code)
			}
		}
	})
	t.Run("environment repo fallback", func(t *testing.T) {
		t.Setenv("AGENT_GH_REPO", repo)
		var got bytes.Buffer
		if code := writeLogs(&got, []string{"ZII-901", "--since", "30m"}); code != 0 {
			t.Fatalf("exit=%d", code)
		}
		if !strings.Contains(got.String(), "started") {
			t.Fatalf("environment repo not used: %s", got.String())
		}
	})
	t.Run("missing files", func(t *testing.T) {
		t.Setenv("AGENT_DRIVER_STATE_ROOT", filepath.Join(t.TempDir(), "missing"))
		var got bytes.Buffer
		if code := writeLogs(&got, []string{"--repo", repo, "ZII-901"}); code != 0 || got.Len() != 0 {
			t.Fatalf("exit=%d output=%s", code, got.String())
		}
	})
	t.Run("read errors", func(t *testing.T) {
		isolated := t.TempDir()
		t.Setenv("AGENT_DRIVER_STATE_ROOT", isolated)
		if err := os.Mkdir(filepath.Join(isolated, "scheduler.log"), 0700); err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		if code := writeLogs(&got, []string{"--repo", repo, "ZII-901"}); code != 1 {
			t.Fatalf("exit=%d, want read failure 1", code)
		}
	})
	t.Run("write errors", func(t *testing.T) {
		if code := writeLogs(logFailWriter{}, []string{"--repo", repo, "ZII-901"}); code != 1 {
			t.Fatalf("exit=%d, want write failure 1", code)
		}
	})
	t.Run("follow rotation and interrupt", func(t *testing.T) {
		isolated := t.TempDir()
		t.Setenv("AGENT_DRIVER_STATE_ROOT", isolated)
		path := filepath.Join(isolated, repo, "ZII-901", "executor.log")
		putLogFixture(t, path, executorFirst)
		reader, writer := io.Pipe()
		defer reader.Close()
		done := make(chan int, 1)
		go func() {
			code := writeLogs(writer, []string{"--repo", repo, "ZII-901", "--json", "--follow"})
			writer.Close()
			done <- code
		}()
		records := make(chan string, 8)
		go func() {
			defer close(records)
			scanner := bufio.NewScanner(reader)
			for scanner.Scan() {
				records <- scanner.Text()
			}
		}()
		defer func() {
			reader.Close()
			select {
			case code := <-done:
				if code != 0 {
					t.Errorf("follow exit=%d", code)
				}
			case <-time.After(2 * time.Second):
				t.Error("follow worker did not stop")
			}
		}()
		expect := func(message string) {
			t.Helper()
			select {
			case line := <-records:
				var fields map[string]any
				if err := json.Unmarshal([]byte(line), &fields); err != nil {
					t.Fatal(err)
				}
				if fields["msg"] != message {
					t.Fatalf("message=%v want %s", fields["msg"], message)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("follow output timed out")
			}
		}
		expect("started")
		if err := os.Rename(path, path+".1"); err != nil {
			t.Fatal(err)
		}
		putLogFixture(t, path, executorLast)
		expect("finished")
		process, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if err := process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		select {
		case line, ok := <-records:
			if ok {
				t.Fatalf("replayed row: %s", line)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("interrupt did not stop follow")
		}
	})
}

type logFailWriter struct{}

func (logFailWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }
