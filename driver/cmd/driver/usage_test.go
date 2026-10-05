package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The driver has no coordinator, so run worktree and coordinator sessions do not exist.
// driver usage must still report worker/reviewer/session usage without them.
func TestUsageWithoutCoordinatorMetadata(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "unit")
	claude := filepath.Join(root, "claude")
	codex := filepath.Join(root, "codex")
	for _, dir := range []string{unit, claude, codex} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	line := `{"type":"assistant","sessionId":"s1","cwd":"` + unit + `","message":{"id":"m1","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}` + "\n"
	if err := os.WriteFile(filepath.Join(claude, "s1.jsonl"), []byte(line), 0400); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() int {
		return runUsage([]string{"ZII-9", "--json", "--worktree", unit, "--claude-dir", claude, "--codex-dir", codex})
	})
	if out.code != 0 {
		t.Fatalf("exit code %d without coordinator metadata", out.code)
	}
	var got struct {
		Items []struct {
			Role         string  `json:"role"`
			InputTokens  float64 `json:"input_tokens"`
			OutputTokens float64 `json:"output_tokens"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.text)), &got); err != nil || len(got.Items) != 1 || got.Items[0].Role != "session" || got.Items[0].InputTokens != 100 {
		t.Fatalf("output=%q err=%v", out.text, err)
	}
	// Partial coordinator metadata is still a mistake and is reported by the collector.
	partial := captureStdout(t, func() int {
		return runUsage([]string{"ZII-9", "--json", "--worktree", unit, "--claude-dir", claude, "--codex-dir", codex, "--run-id", "run_x"})
	})
	if partial.code == 0 {
		t.Fatal("run ID without run worktree and coordinator sessions must fail")
	}
}

type captured struct {
	code int
	text string
}

func captureStdout(t *testing.T, run func() int) captured {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	code := run()
	_ = w.Close()
	os.Stdout = orig
	return captured{code: code, text: <-done}
}

// Real `orca orchestration worker-show` and `orca worktree list` output, trimmed: see CONTRACTS.md.
func TestRecordedOrcaUsageMetadata(t *testing.T) {
	read := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var show orcaWorkerShow
	if err := decodeOrcaResult(read("orca-worker-show.json"), &show); err != nil {
		t.Fatal(err)
	}
	_, path, ok := strings.Cut(show.Worker.WorktreeID, "::")
	if show.Dispatch.RunID != "run_d17780cec9ec" || !ok || path != "/home/laoxu/orca/workspaces/orca-factory/ZII-893-executor" {
		t.Fatalf("worker-show: run=%q worktree=%q", show.Dispatch.RunID, show.Worker.WorktreeID)
	}
	var list orcaWorktreeList
	if err := decodeOrcaResult(read("orca-worktree-list.json"), &list); err != nil {
		t.Fatal(err)
	}
	linked := map[string]string{}
	for _, tree := range list.Worktrees {
		linked[tree.Ticket] = tree.Path
	}
	if list.Truncated || len(list.Worktrees) != 4 || linked["ZII-804"] != "/home/laoxu/orca/workspaces/orca-factory/board-3" {
		t.Fatalf("worktree list: truncated=%v linked=%v", list.Truncated, linked)
	}
}
