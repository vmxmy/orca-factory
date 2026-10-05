package logx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	for _, key := range []string{"Authorization", "x_api_key", "APIKEY", "access_token", "Password", "client_secret"} {
		var b bytes.Buffer
		New("test", &b).With(key, "private").Info("hello", slog.Group("nested", slog.String(key, "private")))
		if strings.Contains(b.String(), "private") || !strings.Contains(b.String(), "[REDACTED]") {
			t.Fatalf("unredacted %s: %s", key, &b)
		}
	}
	for _, prefix := range []string{"lin_api_", "ghp_", "gho_", "sk-"} {
		var b bytes.Buffer
		New("test", &b).Info("hello", "value", prefix+"private", "args", []string{prefix + "private"},
			"phase", "task-list", "repo", "orca-factory", "err", "disk-full",
			"detail", "request failed: "+prefix+"private+retry", "identifier", "project_"+prefix+"public")
		if strings.Contains(b.String(), prefix+"private") {
			t.Fatalf("unredacted value: %s", &b)
		}
		var row map[string]any
		if err := json.Unmarshal(b.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		if row["component"] != "test" || row["level"] != "INFO" || row["time"] == nil {
			t.Fatalf("missing fields: %v", row)
		}
		for key, want := range map[string]string{
			"value": "[REDACTED]", "phase": "task-list", "repo": "orca-factory", "err": "disk-full",
			"detail": "request failed: [REDACTED]+retry", "identifier": "project_" + prefix + "public",
		} {
			if row[key] != want {
				t.Fatalf("%s: got %q, want %q; output: %s", key, row[key], want, &b)
			}
		}
		if got := row["args"].([]any)[0]; got != "[REDACTED]" {
			t.Fatalf("args: got %q, want redaction", got)
		}
	}
}

func TestRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "test.log")
	w, err := RotatingFile(path, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"aaaa", "bbbb", "cccc", "dddd"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for suffix, want := range map[string]string{"": "dddd", ".1": "cccc", ".2": "bbbb"} {
		b, err := os.ReadFile(path + suffix)
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", suffix, b, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("extra archive: %v", err)
	}
	w, err = RotatingFile(path, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := w.Write([]byte("eeee")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path + ".1")
	if err != nil || string(b) != "dddd" {
		t.Fatalf("reopen: %q %v", b, err)
	}
}

func TestExecErrorLogged(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "driver")
	cmd := exec.Command("go", "build", "-o", binary, "../../cmd/driver")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", b, err)
	}
	repo := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(repo, "scripts", "agent-gates"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "init", repo)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git: %s %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scripts", "agent-gates", "agent-graph.env"), []byte("AGENT_GH_REPO=owner/demo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_DRIVER_STATE_ROOT", filepath.Join(root, "state"))
	t.Setenv("ORCA_TERMINAL_HANDLE", "")
	cmd = exec.Command(binary, "exec", "ZII-897")
	cmd.Dir = repo
	b, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exec failure")
	}
	data, err := os.ReadFile(filepath.Join(root, "state", "demo", "ZII-897", "executor.log"))
	if err != nil {
		t.Fatalf("log missing: %v; output: %s", err, b)
	}
	if !strings.Contains(string(data), "driver exec requires a current Orca terminal") || !strings.Contains(string(data), `"exit_code":1`) || !strings.Contains(string(data), `"event":"start"`) {
		t.Fatalf("missing lifecycle/error: %s", data)
	}
	t.Run("log open failure preserves command exit", func(t *testing.T) {
		blocked := filepath.Join(root, "blocked")
		if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AGENT_DRIVER_STATE_ROOT", blocked)
		for _, command := range []string{"exec", "serve"} {
			cmd := exec.Command(binary, command, "extra", "argument")
			cmd.Dir = repo
			output, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 2 || !strings.Contains(string(output), "usage: driver "+command) {
				t.Fatalf("%s must retain usage failure: err=%v output=%s", command, err, output)
			}
			if !strings.Contains(string(output), `"event":"log_open_error"`) {
				t.Fatalf("%s did not report log failure: %s", command, output)
			}
		}
	})
}
