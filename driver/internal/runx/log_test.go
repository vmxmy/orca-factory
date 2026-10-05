package runx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid log: %v", err)
		}
		if d, ok := record["duration"].(float64); !ok || d < 0 {
			t.Fatalf("missing duration: %v", record)
		}
		records = append(records, record)
	}
	return records
}

func TestExternalCallLog(t *testing.T) {
	t.Run("failure and no logger", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))
		spec := runx.Spec{Path: "sh", Args: []string{"-c", "printf stdout; echo boom >&2; exit 3", "sh", "--token=SECRET123"}, Timeout: time.Second}
		args := append([]string(nil), spec.Args...)
		plain, plainErr := runx.Run(context.Background(), spec)
		if buf.Len() != 0 || string(plain) != "stdout" || plainErr == nil || plainErr.Error() != "command failed" {
			t.Fatalf("plain call: %q, %v, %s", plain, plainErr, &buf)
		}
		got, err := runx.Run(runx.WithLogger(context.Background(), logger), spec)
		var plainExit, exit *exec.ExitError
		if !bytes.Equal(got, plain) || !errors.As(plainErr, &plainExit) || !errors.As(err, &exit) || exit.ExitCode() != 3 || !bytes.Equal(exit.Stderr, plainExit.Stderr) || err.Error() != plainErr.Error() {
			t.Fatalf("command contract changed: %q, %v", got, err)
		}
		records := logRecords(t, &buf)
		if len(records) != 1 || records[0]["exe"] != "sh" || records[0]["exit_code"] != float64(3) || records[0]["stderr"] != "boom\n" || records[0]["timeout"] != false {
			t.Fatalf("failure fields: %v", records)
		}
		if !strings.Contains(buf.String(), "--token=[REDACTED]") || strings.Contains(buf.String(), "SECRET123") || strings.Contains(buf.String(), `"stdout"`) || !reflect.DeepEqual(spec.Args, args) {
			t.Fatalf("unsafe or mutated arguments: %s", &buf)
		}
		buf.Reset()
		ctx := runx.WithLogger(runx.WithLogger(context.Background(), logger), nil)
		_, err = runx.Run(ctx, spec)
		if buf.Len() != 0 || err.Error() != plainErr.Error() {
			t.Fatal("nil logger did not disable logging")
		}
	})
	t.Run("credentials in args and stderr", func(t *testing.T) {
		for _, args := range [][]string{
			{"--token", "--PRIVATE123", "--key=KEY123", "--password", "PASS123", "--secret=SECRET123", "--authorization", "AUTH123"},
			{"https://user:PASS123@example.com/path", "ghp_PRIVATE123", "gho_PRIVATE123", "github_pat_PRIVATE123", "lin_api_PRIVATE123", "Bearer PRIVATE123"},
			{`{"token":"PRIVATE123"}`, "Authorization: Bearer PRIVATE123", "--token", "PRIVATE", "--secret", "PRIVATE123"},
		} {
			var buf bytes.Buffer
			spec := runx.Spec{Path: "sh", Args: append([]string{"-c", `printf '%s\n' "$@" >&2; exit 3`, "sh"}, args...), Timeout: time.Second}
			_, err := runx.Run(runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil))), spec)
			if err == nil {
				t.Fatal("expected failure")
			}
			for _, secret := range []string{"PRIVATE", "KEY123", "PASS123", "SECRET123", "AUTH123", "user:"} {
				if strings.Contains(buf.String(), secret) {
					t.Fatalf("credential leaked: %s", &buf)
				}
			}
			if !strings.Contains(buf.String(), "[REDACTED]") {
				t.Fatalf("missing redaction: %s", &buf)
			}
		}
	})
	t.Run("diagnostics and truncation", func(t *testing.T) {
		for _, secret := range []string{"private-credential", "private-" + strings.Repeat("c", 70000)} {
			var buf bytes.Buffer
			spec := runx.Spec{Path: "sh", Args: []string{"-c", `printf stdout; head -c 2040 /dev/zero | tr '\000' x >&2; printf '%s\n' "$2" >&2; head -c 70000 /dev/zero | tr '\000' x >&2; printf '\nTLS connection failed\n' >&2; exit 9`, "sh", "--token", secret}, Timeout: time.Second}
			plain, plainErr := runx.Run(context.Background(), spec)
			got, err := runx.Run(runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil))), spec)
			var a, b *exec.ExitError
			if !bytes.Equal(got, plain) || !errors.As(plainErr, &a) || !errors.As(err, &b) || !bytes.Equal(a.Stderr, b.Stderr) || b.ExitCode() != 9 || !bytes.HasSuffix(b.Stderr, []byte("TLS connection failed\n")) {
				t.Fatal("logger changed diagnostics")
			}
			record := logRecords(t, &buf)[0]
			stderr := record["stderr"].(string)
			if len(stderr) > 2048 || !strings.HasPrefix(stderr, strings.Repeat("x", 2040)) || strings.Contains(buf.String(), "private-") {
				t.Fatalf("stderr limit or credential boundary: %s", &buf)
			}
		}
	})
	t.Run("timeout", func(t *testing.T) {
		var buf bytes.Buffer
		_, err := runx.Run(runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil))), runx.Spec{Path: "sleep", Args: []string{"10"}, Timeout: 30 * time.Millisecond})
		record := logRecords(t, &buf)[0]
		if !errors.Is(err, context.DeadlineExceeded) || record["timeout"] != true || record["exit_code"] != float64(-1) {
			t.Fatalf("timeout: %v, %v", err, record)
		}
	})
	t.Run("GitHub", func(t *testing.T) {
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf ghp_AUTH123\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer ghp_AUTH123" {
				t.Error("authentication token did not reach GitHub")
			}
			w.Header().Set("X-GitHub-Request-Id", "request-123")
			_, _ = w.Write([]byte("[]"))
		}))
		defer server.Close()
		c, err := github.New(github.Options{BaseURL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, err = c.FindOpenPR(runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil))), "owner", "repo", "private-query")
		if err != nil {
			t.Fatal(err)
		}
		records := logRecords(t, &buf)
		if len(records) != 2 || records[0]["exe"] != "gh" || records[0]["exit_code"] != float64(0) {
			t.Fatalf("auth command log: %s", &buf)
		}
		record := records[1]
		if record["method"] != "GET" || record["path"] != "/repos/owner/repo/pulls" || record["status"] != float64(200) || record["request_id"] != "request-123" || strings.Contains(buf.String(), "private-query") || strings.Contains(buf.String(), "AUTH123") {
			t.Fatalf("GitHub fields: %s", &buf)
		}
	})
	t.Run("Orca", func(t *testing.T) {
		for _, tc := range []struct {
			body, result string
			failed       bool
		}{
			{`{"id":"response-id","ok":true,"result":{"id":"run"}}`, "ok", false},
			{`{"ok":false,"error":{"code":"not_found"}}`, "not_found", true},
		} {
			path := filepath.Join(t.TempDir(), "orca")
			if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '"+tc.body+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			c := orca.Client{Path: path}
			var buf bytes.Buffer
			requestID := "07a77606-cef4-4541-a451-76c90fe19226"
			_, err := c.RunCreate(runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil))), requestID, "private-objective")
			if (err != nil) != tc.failed {
				t.Fatalf("Orca result: %v", err)
			}
			records := logRecords(t, &buf)
			if len(records) != 2 {
				t.Fatalf("Orca log count: %v", records)
			}
			record := records[1]
			if record["command"] != "orchestration run-create" || record["result"] != tc.result || record["request_id"] != requestID || strings.Contains(buf.String(), "private-objective") {
				t.Fatalf("Orca fields: %s", &buf)
			}
		}
	})
}
