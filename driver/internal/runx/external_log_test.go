package runx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
		out, err := Run(ctx, Spec{Path: "/bin/sh", Args: []string{"-c", "printf unchanged"}, Timeout: time.Second})
		if err != nil || string(out) != "unchanged" || !strings.Contains(buf.String(), `"exit_code":0`) || !strings.Contains(buf.String(), `"level":"INFO"`) {
			t.Fatalf("out=%q err=%v log=%q", out, err, buf.String())
		}
	})
}
