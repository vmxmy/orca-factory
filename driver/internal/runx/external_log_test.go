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

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

func TestExternalCallLog(t *testing.T) {
	t.Run("logger preserves large error diagnostics", func(t *testing.T) {
		args := []string{"-c", `printf 'unchanged output'; printf 'diagnostic start\n' >&2; head -c 70000 /dev/zero | tr '\000' x >&2; printf '\nTLS connection failed\n' >&2; exit 7`}
		spec := Spec{Path: "/bin/sh", Args: args, Timeout: time.Second}
		plainOutput, plainErr := Run(context.Background(), spec)
		var b bytes.Buffer
		spec.Logger = slog.New(slog.NewJSONHandler(&b, nil))
		loggedOutput, loggedErr := Run(context.Background(), spec)
		if string(plainOutput) != "unchanged output" || !bytes.Equal(loggedOutput, plainOutput) {
			t.Errorf("stdout changed: plain=%q logged=%q", plainOutput, loggedOutput)
		}
		var plainExit, loggedExit *exec.ExitError
		if !errors.As(plainErr, &plainExit) || !errors.As(loggedErr, &loggedExit) {
			t.Fatalf("missing ExitError: plain=%v logged=%v", plainErr, loggedErr)
		}
		if plainExit.ExitCode() != 7 || loggedExit.ExitCode() != plainExit.ExitCode() {
			t.Errorf("exit code changed: plain=%d logged=%d", plainExit.ExitCode(), loggedExit.ExitCode())
		}
		if !bytes.Contains(plainExit.Stderr, []byte("... omitting ")) || !bytes.HasPrefix(plainExit.Stderr, []byte("diagnostic start\n")) || !bytes.HasSuffix(plainExit.Stderr, []byte("\nTLS connection failed\n")) {
			t.Fatal("baseline does not exercise Output prefix/suffix diagnostics")
		}
		if !bytes.Equal(loggedExit.Stderr, plainExit.Stderr) {
			t.Errorf("ExitError.Stderr changed: plain=%d bytes logged=%d bytes; TLS tail retained=%v", len(plainExit.Stderr), len(loggedExit.Stderr), bytes.HasSuffix(loggedExit.Stderr, []byte("\nTLS connection failed\n")))
		}
		var plainTransient, loggedTransient *infra.Transient
		if !errors.As(infra.Classify(plainErr), &plainTransient) || !errors.As(infra.Classify(loggedErr), &loggedTransient) {
			t.Errorf("infra.Classify changed: plain=%T logged=%T", infra.Classify(plainErr), infra.Classify(loggedErr))
		}
		var record map[string]any
		if err := json.Unmarshal(b.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record["exit_code"] != float64(7) || len(record["stderr"].(string)) > 2048 {
			t.Errorf("unexpected log code or stderr limit: code=%v bytes=%d", record["exit_code"], len(record["stderr"].(string)))
		}
	})
	for _, secret := range []string{"private-credential", "private-" + strings.Repeat("c", 70000)} {
		name := "credential across log boundary"
		if len(secret) > 2048 {
			name = "credential across diagnostic capture boundary"
		}
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			args := []string{"-c", `printf 'unchanged output'; head -c 2040 /dev/zero | tr '\000' x >&2; printf '%s\nnon-sensitive diagnostic\n' "$2" >&2; exit 9`, "sh", "--token", secret}
			plainOutput, plainErr := Run(context.Background(), Spec{Path: "/bin/sh", Args: args, Timeout: time.Second})
			output, err := Run(context.Background(), Spec{Path: "/bin/sh", Args: args, Timeout: time.Second, Logger: slog.New(slog.NewJSONHandler(&b, nil))})
			var plainExit, exit *exec.ExitError
			if !errors.As(plainErr, &plainExit) || !errors.As(err, &exit) {
				t.Fatalf("missing ExitError: plain=%v logged=%v", plainErr, err)
			}
			if string(output) != "unchanged output" || !bytes.Equal(output, plainOutput) || exit.ExitCode() != 9 || !bytes.Equal(exit.Stderr, plainExit.Stderr) {
				t.Errorf("logger changed stdout, exit code or diagnostics: code=%d plain=%d bytes logged=%d bytes", exit.ExitCode(), len(plainExit.Stderr), len(exit.Stderr))
			}
			if !bytes.Contains(exit.Stderr, []byte("non-sensitive diagnostic")) {
				t.Error("non-sensitive diagnostic lost")
			}
			var record map[string]any
			if err := json.Unmarshal(b.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			stderr := record["stderr"].(string)
			if strings.Contains(b.String(), secret) || strings.Contains(b.String(), "private-") {
				t.Error("credential fragment leaked across stderr truncation boundary: private-")
			}
			if record["exit_code"] != float64(9) || len(stderr) > 2048 || !strings.HasPrefix(stderr, strings.Repeat("x", 2040)) {
				t.Errorf("log lost exit code, stderr limit or non-sensitive prefix: code=%v bytes=%d", record["exit_code"], len(stderr))
			}
			if args[4] != secret {
				t.Error("arguments mutated")
			}
		})
	}
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
