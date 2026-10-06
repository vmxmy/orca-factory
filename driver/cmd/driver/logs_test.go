package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestLogsCommand(t *testing.T) {
	type record struct {
		Time      time.Time      `json:"time"`
		Source    string         `json:"source"`
		Component string         `json:"component"`
		Level     string         `json:"level"`
		Event     string         `json:"event"`
		Fields    map[string]any `json:"fields"`
		Raw       string         `json:"raw"`
	}
	for _, tc := range []struct {
		name       string
		launch     string
		fail       string
		args       []string
		want       []string
		customRoot bool
	}{
		{
			name:   "merge real ticket sources",
			launch: "2000-01-01T00:00:03Z INFO launched pid=42\n2000-01-01T00:00:01Z INFO starting\n",
			fail:   "2000-01-01T00:00:02Z FAIL: approach=head=abc | reason=worker failed\n",
			args:   []string{"ZII-901", "--json"},
			want:   []string{"2000-01-01T00:00:01Z INFO starting", "2000-01-01T00:00:02Z FAIL: approach=head=abc | reason=worker failed", "2000-01-01T00:00:03Z INFO launched pid=42"},
		},
		{
			name:   "since filters timestamped records only",
			launch: "2000-01-01T00:00:01Z old launch\n2099-01-01T00:00:00Z future launch\npermission denied\n",
			fail:   "2000-01-01T00:00:02Z FAIL: old failure\n",
			args:   []string{"ZII-901", "--json", "--since", "1h"},
			want:   []string{"permission denied", "2099-01-01T00:00:00Z future launch"},
		},
		{
			name:   "free text and unterminated history",
			launch: "cannot start executor",
			fail:   "FAIL: approach=head=abc | reason=reviewer red\n",
			args:   []string{"--json", "ZII-901"},
			want:   []string{"cannot start executor", "FAIL: approach=head=abc | reason=reviewer red"},
		},
		{
			name:       "override fail root keeps launch under HOME",
			launch:     "2000-01-01T00:00:01Z home launch\n",
			fail:       "2000-01-01T00:00:02Z FAIL: overridden root\n",
			args:       []string{"ZII-901", "--json"},
			want:       []string{"2000-01-01T00:00:01Z home launch", "2000-01-01T00:00:02Z FAIL: overridden root"},
			customRoot: true,
		},
		{
			name:   "common timestamp formats and original JSON",
			launch: "2000-01-01 00:00:03 INFO third\n2000/01/01 00:00:01 first\n",
			fail:   "{\"time\":\"2000-01-01T00:00:02.125Z\",\"level\":\"ERROR\",\"event\":\"failure\",\"ticket\":\"ZII-901\"}\n",
			args:   []string{"ZII-901", "--json"},
			want:   []string{"2000/01/01 00:00:01 first", "{\"time\":\"2000-01-01T00:00:02.125Z\",\"level\":\"ERROR\",\"event\":\"failure\",\"ticket\":\"ZII-901\"}", "2000-01-01 00:00:03 INFO third"},
		},
		{name: "missing logs is an empty array", args: []string{"ZII-901", "--json"}, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("AGENT_DRIVER_STATE_ROOT", "")
			homeRoot := filepath.Join(home, ".local", "state", "orca-factory")
			root := homeRoot
			if tc.customRoot {
				root = t.TempDir()
				t.Setenv("AGENT_DRIVER_STATE_ROOT", root)
			}
			for path, content := range map[string]string{
				filepath.Join(homeRoot, "launch-ZII-901.log"):       tc.launch,
				filepath.Join(root, "repo1", "ZII-901", "fail.log"): tc.fail,
				filepath.Join(homeRoot, "launch-ZII-902.log"):       "2099-01-01T00:00:00Z other launch\n",
				filepath.Join(root, "repo1", "ZII-902", "fail.log"): "2099-01-01T00:00:00Z other failure\n",
			} {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			previous := os.Stdout
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = writer
			defer func() { os.Stdout = previous }()
			code := commands["logs"](tc.args)
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			output, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if code != 0 {
				t.Fatalf("exit = %d, output = %q", code, output)
			}
			if !bytes.HasPrefix(bytes.TrimSpace(output), []byte("[")) {
				t.Fatalf("want JSON array, got %q", output)
			}
			var got []record
			if err := json.Unmarshal(output, &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("want %d records, got %s", len(tc.want), output)
			}
			for i, entry := range got {
				if entry.Raw != tc.want[i] {
					t.Errorf("record %d raw = %q, want %q", i, entry.Raw, tc.want[i])
				}
				if !strings.Contains(entry.Source, "ZII-901") || strings.Contains(entry.Source, "ZII-902") {
					t.Errorf("unscoped source: %q", entry.Source)
				}
				if entry.Component == "" || entry.Level == "" || entry.Event == "" || entry.Fields == nil {
					t.Errorf("missing structured fields: %+v", entry)
				}
				if i > 0 && entry.Time.Before(got[i-1].Time) {
					t.Errorf("time order: %s", output)
				}
			}
		})
	}
}

func TestLogsFollowRotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		root := filepath.Join(home, ".local", "state", "orca-factory")
		path := filepath.Join(root, "launch-ZII-901.log")
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("2000-01-01T00:00:00Z first\n"), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var output bytes.Buffer
		done := make(chan error, 1)
		go func() { done <- streamLogs(ctx, &output, root, "ZII-901", time.Time{}, true, true) }()
		synctest.Wait()
		appendLine := func(text string) {
			t.Helper()
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := f.WriteString(text)
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("append: %v; close: %v", writeErr, closeErr)
			}
		}
		tick := func() { time.Sleep(time.Second); synctest.Wait() }
		appendLine("2000-01-01T00:00:01Z partial")
		tick()
		if strings.Contains(output.String(), "partial") {
			t.Fatal("unfinished line emitted before newline")
		}
		appendLine(" done\n")
		tick()
		appendLine("2000-01-01T00:00:02Z before-replacement\n")
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("2000-01-01T00:00:03Z after-replacement\n"), 0600); err != nil {
			t.Fatal(err)
		}
		tick()
		if err := os.WriteFile(path, []byte("truncated\n"), 0600); err != nil {
			t.Fatal(err)
		}
		failPath := filepath.Join(root, "repo1", "ZII-901", "fail.log")
		if err := os.MkdirAll(filepath.Dir(failPath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(failPath, []byte("FAIL: created while following\n"), 0600); err != nil {
			t.Fatal(err)
		}
		tick()
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(&output)
		var got []string
		for {
			var batch []struct {
				Raw string `json:"raw"`
			}
			err := decoder.Decode(&batch)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range batch {
				got = append(got, record.Raw)
			}
		}
		want := []string{
			"2000-01-01T00:00:00Z first",
			"2000-01-01T00:00:01Z partial done",
			"2000-01-01T00:00:02Z before-replacement",
			"2000-01-01T00:00:03Z after-replacement",
			"truncated",
			"FAIL: created while following",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("follow records = %q, want %q", got, want)
		}
	})
}
