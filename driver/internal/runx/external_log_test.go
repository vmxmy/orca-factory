package runx_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestExternalCallLog(t *testing.T) {
	t.Run("overlapping credentials", func(t *testing.T) {
		var line string
		args := []string{"-c", `printf '%s %s' "$2" "$4" >&2; exit 7`, "sh", "--token", "private-value", "--key", "private-value-extended"}
		_, err := runx.Run(context.Background(), runx.Spec{Path: "/bin/sh", Args: args, Timeout: time.Second, Logger: func(s string) { line = s }})
		if err == nil || !strings.Contains(line, "***") || strings.Contains(line, "private-value") || strings.Contains(line, "extended") {
			t.Fatalf("partial credential leaked: %s", line)
		}
	})
	t.Run("adapters", testAdapterLogs)
	t.Run("nil logger", func(t *testing.T) {
		out, err := runx.Run(context.Background(), runx.Spec{Path: "/bin/sh", Args: []string{"-c", "printf hello"}, Timeout: time.Second})
		if err != nil || string(out) != "hello" {
			t.Fatalf("stdout=%q err=%v", out, err)
		}
	})
	t.Run("sensitive flags and stderr", func(t *testing.T) {
		for _, flag := range []string{"--token", "--key", "--secret", "--password", "--Authorization"} {
			var line string
			args := []string{"-c", `printf 'secret\n%s' "$2" >&2; exit 7`, "sh", flag, "super-secret"}
			_, err := runx.Run(context.Background(), runx.Spec{Path: "/bin/sh", Args: args, Timeout: time.Second, Logger: func(s string) { line = s }})
			if err == nil || !strings.Contains(line, "exit=7") || !strings.Contains(line, "secret") || !strings.Contains(line, "***") || strings.Contains(line, "super-secret") {
				t.Fatalf("flag=%s log=%s err=%v", flag, line, err)
			}
		}
	})
	t.Run("logger preserves large error diagnostics", func(t *testing.T) {
		args := []string{"-c", `printf 'unchanged output'; printf 'diagnostic start\n' >&2; head -c 70000 /dev/zero | tr '\000' x >&2; printf '\nTLS connection failed\n' >&2; exit 7`}
		spec := runx.Spec{Path: "/bin/sh", Args: args, Timeout: time.Second}
		plainOutput, plainErr := runx.Run(context.Background(), spec)
		var b bytes.Buffer
		spec.Logger = func(line string) { b.WriteString(line) }
		loggedOutput, loggedErr := runx.Run(context.Background(), spec)
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
		record := commandLog(t, b.String())
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
			plainOutput, plainErr := runx.Run(context.Background(), runx.Spec{Path: "/bin/sh", Args: args, Timeout: time.Second})
			output, err := runx.Run(context.Background(), runx.Spec{Path: "/bin/sh", Args: args, Timeout: time.Second, Logger: func(line string) { b.WriteString(line) }})
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
			record := commandLog(t, b.String())
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
			_, err := runx.Run(context.Background(), runx.Spec{Path: "/bin/sh", Args: args, Timeout: tc.timeout, Logger: func(line string) { b.WriteString(line) }})
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
			record := commandLog(t, b.String())
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

// Read fields independently so assertions cover the emitted callback contract.
func commandLog(t *testing.T, line string) map[string]any {
	t.Helper()
	record := map[string]any{}
	for _, field := range []string{"executable", "stderr"} {
		match := regexp.MustCompile(field + `=("(?:[^"\\]|\\.)*")`).FindStringSubmatch(line)
		if len(match) != 2 {
			t.Fatalf("missing %s in %s", field, line)
		}
		value, err := strconv.Unquote(match[1])
		if err != nil {
			t.Fatal(err)
		}
		record[field] = value
	}
	for _, field := range []string{"exit", "duration_ms"} {
		match := regexp.MustCompile(field + `=(-?[0-9]+)`).FindStringSubmatch(line)
		if len(match) != 2 {
			t.Fatalf("missing %s in %s", field, line)
		}
		value, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatal(err)
		}
		if field == "exit" {
			field = "exit_code"
		}
		record[field] = float64(value)
	}
	if !strings.Contains(line, "timeout=false") && !strings.Contains(line, "timeout=true") {
		t.Fatalf("missing timeout: %s", line)
	}
	record["timeout"] = strings.Contains(line, "timeout=true")
	return record
}

// Adapter coverage lives here so the ticket's CHECK runs it too.
func testAdapterLogs(t *testing.T) {
	t.Run("GitHub", func(t *testing.T) {
		for _, status := range []int{200, 500} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-GitHub-Request-Id", "request-123")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"number":17}`)
			}))
			defer server.Close()
			var line string
			c, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "private-credential", nil }, Logger: func(s string) { line = s }})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.GetPR(context.Background(), "acme", "app", 17)
			if (err == nil) != (status == 200) {
				t.Fatal(err)
			}
			for _, want := range []string{"GET", "/repos/acme/app/pulls/17", fmt.Sprintf("status=%d", status), "request-123", "duration_ms="} {
				if !strings.Contains(line, want) {
					t.Fatalf("missing %s: %s", want, line)
				}
			}
			if strings.Contains(line, "private-credential") {
				t.Fatal("credential logged")
			}
		}
	})
	t.Run("Orca", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "orca")
		id := orca.RequestID("ZII-900", "executor", "start", 1)
		for _, tc := range []struct{ name, envelope, result string }{
			{"success", `{"ok":true,"result":{}}`, "ok=true"},
			{"rejected", `{"ok":false,"error":{"code":"rejected"}}`, "ok=false"},
			{"invalid JSON", `invalid`, "error"},
		} {
			if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '"+tc.envelope+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			var line string
			c := orca.Client{Path: path, Logger: func(s string) { line = s }}
			_, err := c.RunCreate(context.Background(), id, "private-objective")
			if (err == nil) != (tc.name == "success") {
				t.Fatalf("%s: %v", tc.name, err)
			}
			for _, want := range []string{"orchestration run-create", tc.result, id, "duration_ms="} {
				if !strings.Contains(line, want) {
					t.Fatalf("missing %s: %s", want, line)
				}
			}
			if strings.Contains(line, "private-objective") {
				t.Fatal("objective logged")
			}
		}
		envelope := `{"id":"07a77606-cef4-4541-a451-76c90fe19226","ok":true,"result":{}}`
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '"+envelope+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		var line string
		c := orca.Client{Path: path, Logger: func(s string) { line = s }}
		if _, err := c.WorktreeSet(context.Background(), "/worktrees/ZII-1-impl", "ZII-1", "in-progress", "gate"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(line, `request_id=""`) {
			t.Fatalf("request ID must be empty without retry-request: %s", line)
		}
	})
}
