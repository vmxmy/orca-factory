package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type pruneFakeWorkspaces struct {
	trees     []orca.Worktree
	terminals []orca.Terminal
	removed   []string
	closed    []string
}

func (f *pruneFakeWorkspaces) WorktreeList(context.Context, string) ([]orca.Worktree, error) {
	return f.trees, nil
}
func (f *pruneFakeWorkspaces) WorktreeRemove(_ context.Context, path string) error {
	f.removed = append(f.removed, path)
	return nil
}
func (f *pruneFakeWorkspaces) TerminalList(context.Context) ([]orca.Terminal, error) {
	return f.terminals, nil
}
func (f *pruneFakeWorkspaces) TerminalClose(_ context.Context, handle string) error {
	f.closed = append(f.closed, handle)
	return nil
}

func TestPruneWorkspacesIdleTerminals(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		name := "remove"
		if dryRun {
			name = "dry-run"
		}
		t.Run(name, func(t *testing.T) {
			const path = "/ws/OF-1-executor"
			fake := &pruneFakeWorkspaces{
				trees:     []orca.Worktree{{Path: path}},
				terminals: []orca.Terminal{{Handle: "term_idle", WorktreePath: path, LastOutputAt: 1}},
			}
			repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p"}}
			ledger := &serveMergeIssues{issue: task.Issue{ID: "OF-1", Identifier: "OF-1", Project: task.NamedID{ID: "p"}, State: task.NamedID{Name: "Done"}}}
			runtime := &scheduler.Runtime{
				Clients:      map[string]issues.Store{"/repo": ledger},
				Repositories: map[string]config.Repository{"/repo": repo},
				Workspaces:   fake,
				Now:          func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) },
				Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
					if spec.Args[0] == "rev-list" {
						return []byte("0\n"), nil
					}
					return nil, nil
				},
			}
			var out bytes.Buffer
			if err := pruneWorkspaces(context.Background(), runtime, []config.Repository{repo}, dryRun, &out, slog.New(slog.DiscardHandler)); err != nil {
				t.Fatal(err)
			}
			action := "removed"
			if dryRun {
				action = "would-remove"
				if len(fake.closed) != 0 || len(fake.removed) != 0 {
					t.Fatalf("dry-run closed=%v removed=%v", fake.closed, fake.removed)
				}
			} else if !slices.Equal(fake.closed, []string{"term_idle"}) || !slices.Equal(fake.removed, []string{path}) {
				t.Fatalf("closed=%v removed=%v", fake.closed, fake.removed)
			}
			if want := action + " " + path + " ticket=OF-1 reason=\"ticket Done\"\n"; out.String() != want {
				t.Fatalf("out=%q want %q", out.String(), want)
			}
		})
	}
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
