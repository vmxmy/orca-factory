package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

func TestDiscoverUsageWorktrees(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"-c", "user.name=Usage Test", "-c", "user.email=usage@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "initial"},
		{"worktree", "add", "-b", "executor", root + "/ZII-894-executor"},
		{"worktree", "add", "-b", "other", root + "/OTHER-1-executor"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	t.Chdir(repo)
	got, err := discoverUsageWorktrees(context.Background(), "ZII-894")
	want := []usage.Worktree{{Path: root + "/ZII-894-executor"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("worktrees = %+v, error = %v; want %+v", got, err, want)
	}
}

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
