package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ledgerGitHubFake serves one managed ledger issue and keeps its comments in memory.
type ledgerGitHubFake struct {
	mu       sync.Mutex
	comments []string
}

func (f *ledgerGitHubFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
		body := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"","project":"o/r","state":"Backlog","complete":true,"blockers":[],"related":[]} -->`
		_, _ = fmt.Fprintf(w, `[{"number":42,"node_id":"I_ledger","title":"[ledger] agent-graph","body":%q,"state":"open","labels":[],"updated_at":"2026-10-06T00:00:00Z"}]`, body)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/42/comments":
		rows := []map[string]any{}
		for i, body := range f.comments {
			rows = append(rows, map[string]any{"id": i + 1, "body": body, "created_at": "2026-10-06T00:00:00Z", "updated_at": "2026-10-06T00:00:00Z"})
		}
		_ = json.NewEncoder(w).Encode(rows)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/42/comments":
		var input struct{ Body string }
		raw, _ := io.ReadAll(r.Body)
		if json.Unmarshal(raw, &input) != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		f.comments = append(f.comments, input.Body)
		_, _ = fmt.Fprintf(w, `{"id":%d,"body":%q,"created_at":"2026-10-06T00:00:00Z","updated_at":"2026-10-06T00:00:00Z"}`, len(f.comments), input.Body)
	default:
		http.NotFound(w, r)
	}
}

func shellLedgerCommand(t *testing.T, env []string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	command := exec.Command(os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--", "ledger-shell"}, args...)...)
	command.Env = append(os.Environ(), env...)
	command.Stdin = strings.NewReader(stdin)
	var out, warnings bytes.Buffer
	command.Stdout, command.Stderr = &out, &warnings
	code := 0
	if err := command.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return code, out.String(), warnings.String()
}

// isolatedGitHubEnv points every backend read at fake services and a temporary HOME.
func isolatedGitHubEnv(t *testing.T, api string) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nprintf test-token\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{"DRIVER_TEST_PROCESS=1", "HOME=" + dir, "AGENT_DRIVER_STATE_ROOT=" + filepath.Join(dir, "state"), "PATH=" + dir + ":" + os.Getenv("PATH"), "AGENT_ISSUE_BACKEND=github", "AGENT_GH_REPO=o/r", "AGENT_GH_API_URL=" + api}
}

func TestLedgerShellForwardsToDriverLedger(t *testing.T) {
	fake := &ledgerGitHubFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	env := isolatedGitHubEnv(t, server.URL)
	code, out, warnings := shellLedgerCommand(t, env, "", "--contract-version")
	if code != 0 || out != "ledger-shell-v1\n" || warnings != "" {
		t.Fatalf("probe: %d %q %q", code, out, warnings)
	}
	code, out, warnings = shellLedgerCommand(t, env, `{"ticket":"A","outcome":"merged","corrections":1}`, "append")
	fake.mu.Lock()
	comments := append([]string(nil), fake.comments...)
	fake.mu.Unlock()
	if code != 0 || out != "" || len(comments) != 1 || comments[0] != `ledger {"ticket":"A","outcome":"merged","corrections":1}` {
		t.Fatalf("append: %d %q %q comments=%q", code, out, warnings, comments)
	}
	code, out, warnings = shellLedgerCommand(t, env, "", "stats", "--all")
	if code != 0 || !strings.HasPrefix(out, "runs=1 merged=1 ") {
		t.Fatalf("stats: %d %q %q", code, out, warnings)
	}
}

func TestLedgerShellRequiresStoreBackend(t *testing.T) {
	for _, backend := range []string{"", "linear", "jira"} {
		code, out, warnings := shellLedgerCommand(t, []string{"DRIVER_TEST_PROCESS=1", "HOME=" + t.TempDir(), "AGENT_ISSUE_BACKEND=" + backend}, "", "stats")
		if code != 2 || out != "" || !strings.Contains(warnings, "AGENT_ISSUE_BACKEND must be github or feishu") {
			t.Fatalf("backend %q: %d %q %q", backend, code, out, warnings)
		}
	}
}
