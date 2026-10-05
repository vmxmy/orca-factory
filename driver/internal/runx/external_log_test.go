package runx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestExternalCallLog(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		timeout      time.Duration
		code         float64
		timedOut     bool
	}{
		{"failure", `printf 'failure detail' >&2; exit 7`, time.Second, 7, false},
		{"bounded stderr", `head -c 3000 /dev/zero | tr '\000' x >&2; exit 2`, time.Second, 2, false},
		{"timeout", `sleep 10`, 20 * time.Millisecond, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			args := []string{"-c", tc.script, "--token", "private-credential", "--password=hidden-value"}
			_, err := Run(context.Background(), Spec{Path: "/bin/sh", Args: args, Timeout: tc.timeout, Logger: slog.New(slog.NewJSONHandler(&b, nil))})
			if err == nil {
				t.Fatal("expected command failure")
			}
			if tc.name == "failure" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || string(exit.Stderr) != "failure detail" {
					t.Fatal("logger changed command error stderr")
				}
			}
			if tc.timedOut && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(b.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["executable"] != "sh" || record["exit_code"] != tc.code || record["timeout"] != tc.timedOut {
				t.Fatalf("record: %s", &b)
			}
			if record["duration_ms"].(float64) < 0 {
				t.Fatal("negative duration")
			}
			stderr := record["stderr"].(string)
			if tc.name == "failure" && stderr != "failure detail" {
				t.Fatalf("stderr: %q", stderr)
			}
			if tc.name == "bounded stderr" && len(stderr) != 2048 {
				t.Fatalf("stderr length: %d", len(stderr))
			}
			for _, secret := range []string{"private-credential", "hidden-value"} {
				if strings.Contains(b.String(), secret) {
					t.Fatalf("credential leaked: %s", &b)
				}
			}
			if args[3] != "private-credential" {
				t.Fatal("arguments mutated")
			}
		})
	}
}
