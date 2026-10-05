package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

func TestUsageInvalidArguments(t *testing.T) {
	if code := runUsage([]string{"ZII-894", "--unknown"}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestUsageJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	for _, dir := range []string{".claude/projects", ".codex/sessions"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	originalDiscovery := discoverUsageWorktrees
	discoverUsageWorktrees = func(context.Context, string) ([]usage.Worktree, error) {
		return []usage.Worktree{{Path: home}}, nil
	}
	t.Cleanup(func() { discoverUsageWorktrees = originalDiscovery })
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	stdout := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = stdout })
	code := runUsage([]string{"ZII-894", "--json"})
	os.Stdout = stdout
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var result usage.Result
	if err := json.NewDecoder(reader).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Ticket != "ZII-894" || result.PriceVersion != "2026-10-06-standard-v2" || len(result.Items) != 0 || len(result.Skipped) != 0 {
		t.Fatalf("JSON result = %+v", result)
	}
}
