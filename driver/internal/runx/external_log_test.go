package runx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestExternalCallLog(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		args         []string
		timeout      time.Duration
		code         float64
		timedOut     bool
	}{
		{"failure", "echo boom-stderr >&2; exit 7", []string{"--token=ghp_SECRET123"}, time.Second, 7, false},
		{"redacted limit", "i=0; while [ $i -lt 500 ]; do printf 'gho_x ' >&2; i=$((i+1)); done; exit 1", nil, time.Second, 1, false},
		{"truncate", "i=0; while [ $i -lt 2500 ]; do printf '界' >&2; i=$((i+1)); done; exit 1", nil, time.Second, 1, false},
		{"timeout", "sleep 5", nil, 50 * time.Millisecond, -1, true},
		{"credentials", "echo 'ghp_SECRET123 lin_api_SECRET github_pat_SECRET Bearer PRIVATE --password=PASSWORD https://user:pass@example.com' >&2; exit 7", []string{"--api-key", "PRIVATEKEY", "--secret=SECRET", "--password", "PASSWORD", "--authorization", "Bearer PRIVATE", "https://user:pass@example.com"}, time.Second, 7, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
			_, err := Run(ctx, Spec{Path: "/bin/sh", Args: append([]string{"-c", tc.script, "sh"}, tc.args...), Timeout: tc.timeout})
			if err == nil || (tc.timedOut && !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatalf("error = %v", err)
			}
			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("log %q: %v", buf.String(), err)
			}
			if entry["exit_code"] != tc.code || entry["timed_out"] != tc.timedOut || entry["executable"] != "sh" || entry["level"] != "WARN" {
				t.Fatalf("log %s", buf.String())
			}
			if entry["duration"] == nil {
				t.Fatal("missing duration")
			}
			stderr, ok := entry["stderr"].(string)
			if !ok || len(stderr) > 2048 || !utf8.ValidString(stderr) {
				t.Fatalf("stderr %q", stderr)
			}
			if tc.name == "failure" && !strings.Contains(stderr, "boom-stderr") {
				t.Fatalf("stderr %q", stderr)
			}
			if tc.name == "truncate" && len(stderr) != 2046 {
				t.Fatalf("stderr length %d", len(stderr))
			}
			for _, secret := range []string{"ghp_SECRET123", "lin_api_SECRET", "github_pat_SECRET", "PRIVATEKEY", "PASSWORD", "Bearer PRIVATE", "user:pass@"} {
				if strings.Contains(buf.String(), secret) {
					t.Fatalf("credential leaked: %s", buf.String())
				}
			}
			if tc.name == "failure" && !strings.Contains(buf.String(), "--token=[REDACTED]") {
				t.Fatalf("args %s", buf.String())
			}
		})
	}
	t.Run("without logger", func(t *testing.T) {
		var buf bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
		defer slog.SetDefault(previous)
		out, err := Run(context.Background(), Spec{Path: "/bin/sh", Args: []string{"-c", "printf unchanged"}, Timeout: time.Second})
		if err != nil || string(out) != "unchanged" || buf.Len() != 0 {
			t.Fatalf("out=%q err=%v log=%q", out, err, buf.String())
		}
	})
	t.Run("success", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
		out, err := Run(ctx, Spec{Path: "/bin/sh", Args: []string{"-c", "printf unchanged; echo diagnostic >&2"}, Timeout: time.Second})
		if !strings.Contains(buf.String(), `"stderr":"diagnostic\n"`) {
			t.Fatalf("missing successful stderr: %s", &buf)
		}
		if err != nil || string(out) != "unchanged" || !strings.Contains(buf.String(), `"exit_code":0`) || !strings.Contains(buf.String(), `"level":"INFO"`) {
			t.Fatalf("out=%q err=%v log=%q", out, err, buf.String())
		}
	})
}

func TestExternalCallLogUsesLogx(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	_, err := Run(ctx, Spec{Path: "/bin/sh", Args: []string{"-c", "echo gho_PRIVATE >&2; exit 3"}, Timeout: time.Second})
	if err == nil || strings.Contains(buf.String(), "gho_PRIVATE") || !strings.Contains(buf.String(), "[REDACTED]") {
		t.Fatalf("unredacted call: %s", &buf)
	}
}

func TestExternalCallLogAuthorization(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	_, err := Run(ctx, Spec{Path: "/bin/sh", Args: []string{"-c", "echo 'Authorization: Basic dXNlcjpwYXNz' >&2; exit 1", "sh", "-H", "Authorization: Basic dXNlcjpwYXNz"}, Timeout: time.Second})
	if err == nil || strings.Contains(buf.String(), "dXNlcjpwYXNz") {
		t.Fatalf("authorization leaked: %s", &buf)
	}
}

func TestExternalCallLogHidesClaudePrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	// Echo full argv and a partial prompt, as a failing provider may do.
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >&2\nprintf 'opaque-private-value' >&2\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-p", "--print", "--print="} {
		t.Run(flag, func(t *testing.T) {
			prompt := "Correction instructions\nBrief: database password: opaque-private-value\n" + strings.Repeat("private-ticket-detail", 5000)
			args := []string{flag, prompt, "--permission-mode", "acceptEdits"}
			if flag == "--print=" {
				args = []string{flag + prompt, "--permission-mode", "acceptEdits"}
			}
			var logs bytes.Buffer
			ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
			_, err := Run(ctx, Spec{Path: path, Args: args, Timeout: time.Second})
			if err == nil {
				t.Fatal("expected provider failure")
			}
			for _, private := range []string{"opaque-private-value", "private-ticket-detail", "Correction instructions"} {
				if strings.Contains(logs.String(), private) {
					t.Errorf("private prompt text leaked: %s", private)
				}
			}
			if !strings.Contains(logs.String(), `"exit_code":7`) || !strings.Contains(logs.String(), "[REDACTED]") {
				t.Fatal("missing exit code or redaction marker")
			}
		})
	}
}
