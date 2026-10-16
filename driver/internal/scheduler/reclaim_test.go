package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type fakeWorkspaces struct {
	trees     []orca.Worktree
	terminals []orca.Terminal
	removed   []string
}

func (f *fakeWorkspaces) WorktreeList(context.Context, string) ([]orca.Worktree, error) {
	return f.trees, nil
}
func (f *fakeWorkspaces) WorktreeRemove(_ context.Context, path string) error {
	f.removed = append(f.removed, filepath.Base(path))
	f.trees = slices.DeleteFunc(f.trees, func(tree orca.Worktree) bool { return tree.Path == path })
	return nil
}
func (f *fakeWorkspaces) TerminalList(context.Context) ([]orca.Terminal, error) {
	return f.terminals, nil
}

// Terminal tickets lose every workspace, active tickets lose only superseded
// resolver rounds, and each safety condition keeps a workspace with one WARN.
func TestReclaimWorkspaces(t *testing.T) {
	const merged = "1111111111111111111111111111111111111111"
	ticket := func(id, state string) task.Issue {
		return task.Issue{ID: id, Identifier: id, Project: task.NamedID{ID: "p"}, State: task.NamedID{Name: state}}
	}
	lc := &fakeIssues{
		issues:        []task.Issue{ticket("OF-1", "Done"), ticket("OF-2", "Canceled"), ticket("OF-3", "Canceled"), ticket("OF-4", "In Progress"), ticket("OF-5", "Done"), ticket("OF-6", "Done")},
		issueComments: []task.Comment{{ID: "c", Body: `merge-handoff-v1: {"queue_id":"q","head":"` + merged + `"}`}},
	}
	ws := func(name string) orca.Worktree { return orca.Worktree{Path: "/ws/" + name} }
	fake := &fakeWorkspaces{
		trees:     []orca.Worktree{{Path: "/repo", Main: true}, ws("OF-1-executor"), ws("OF-1-resolver-1"), ws("OF-2-executor"), ws("OF-3-executor"), ws("OF-4-executor"), ws("OF-4-resolver-1"), ws("OF-4-resolver-2"), ws("OF-5-executor"), {Path: "/ws/OF-6-executor", Pinned: true}, ws("shearwater")},
		terminals: []orca.Terminal{{Handle: "term_1", WorktreeID: "repo::/ws/OF-5-executor", LastOutputAt: 1}},
	}
	repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p"}}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": lc}, Repositories: map[string]config.Repository{"/repo": repo}, Workspaces: fake}
	r.Run = func(_ context.Context, s runx.Spec) ([]byte, error) {
		name := filepath.Base(s.Dir)
		switch s.Args[0] {
		case "status":
			if name == "OF-2-executor" {
				return []byte(" M main.go\n"), nil
			}
			return nil, nil
		case "rev-parse":
			if name == "OF-1-executor" {
				return []byte(merged + "\n"), nil
			}
			return []byte("2222222222222222222222222222222222222222\n"), nil
		case "rev-list":
			if name == "OF-3-executor" || name == "OF-1-executor" {
				return []byte("1\n"), nil
			}
			return []byte("0\n"), nil
		}
		t.Fatalf("unexpected git %v", s.Args)
		return nil, nil
	}
	if _, err := r.Snapshot(context.Background(), []config.Repository{repo}); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))

	verdicts, err := r.ReclaimWorkspaces(context.Background(), true, logger)
	if err != nil || len(fake.removed) != 0 {
		t.Fatalf("dry-run err=%v removed=%v", err, fake.removed)
	}
	got := map[string]string{}
	for _, v := range verdicts {
		got[filepath.Base(v.Path)] = strings.Join(v.Blocks, ",")
	}
	want := map[string]string{"OF-1-executor": "", "OF-1-resolver-1": "", "OF-2-executor": "dirty", "OF-3-executor": "unpushed", "OF-4-resolver-1": "", "OF-5-executor": "terminal-running", "OF-6-executor": "pinned"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("verdicts=%v want %v", got, want)
	}

	for range 2 {
		if _, err := r.ReclaimWorkspaces(context.Background(), false, logger); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(fake.removed)
	if want := []string{"OF-1-executor", "OF-1-resolver-1", "OF-4-resolver-1"}; !reflect.DeepEqual(fake.removed, want) {
		t.Fatalf("removed=%v want %v", fake.removed, want)
	}
	if n := strings.Count(log.String(), "workspace kept"); n != 4 {
		t.Fatalf("kept warnings=%d want 4 (one per kept workspace)\n%s", n, log.String())
	}
}
