package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type pruneFakeWorkspaces struct {
	trees   []orca.Worktree
	removed []string
}

func (f *pruneFakeWorkspaces) WorktreeList(context.Context, string) ([]orca.Worktree, error) {
	return f.trees, nil
}
func (f *pruneFakeWorkspaces) WorktreeRemove(_ context.Context, path string) error {
	f.removed = append(f.removed, path)
	return nil
}
func (f *pruneFakeWorkspaces) TerminalList(context.Context) ([]orca.Terminal, error) {
	return []orca.Terminal{}, nil
}

// --dry-run lists what would be removed and what is kept, and removes nothing.
func TestPruneWorkspacesDryRun(t *testing.T) {
	fake := &pruneFakeWorkspaces{trees: []orca.Worktree{{Path: "/ws/OF-4-resolver-1"}, {Path: "/ws/OF-4-resolver-2"}, {Path: "/ws/OF-5-resolver-1"}, {Path: "/ws/OF-5-resolver-2"}}}
	runtime := &scheduler.Runtime{Repositories: map[string]config.Repository{"/repo": {Path: "/repo"}}, Workspaces: fake}
	runtime.Run = func(_ context.Context, s runx.Spec) ([]byte, error) {
		switch {
		case s.Args[0] == "status" && filepath.Base(s.Dir) == "OF-5-resolver-1":
			return []byte("?? notes.txt\n"), nil
		case s.Args[0] == "rev-list":
			return []byte("0\n"), nil
		}
		return nil, nil
	}
	var out bytes.Buffer
	if err := pruneWorkspaces(context.Background(), runtime, nil, true, &out, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	want := "would-remove /ws/OF-4-resolver-1 ticket=OF-4 reason=\"superseded by resolver round 2\"\n" +
		"keep /ws/OF-5-resolver-1 ticket=OF-5 reason=\"superseded by resolver round 2\" blocks=dirty\n"
	if out.String() != want || len(fake.removed) != 0 {
		t.Fatalf("out=%q removed=%v", out.String(), fake.removed)
	}
}
