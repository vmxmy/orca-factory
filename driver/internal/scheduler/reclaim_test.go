package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type fakeWorkspaces struct {
	trees       []orca.Worktree
	terminals   []orca.Terminal
	removed     []string
	closed      []string
	closeErrors map[string]error
	listErr     error
	actions     []string
}

func (f *fakeWorkspaces) WorktreeList(context.Context, string) ([]orca.Worktree, error) {
	return f.trees, nil
}
func (f *fakeWorkspaces) WorktreeRemove(_ context.Context, path string) error {
	f.actions = append(f.actions, "remove "+path)
	f.removed = append(f.removed, filepath.Base(path))
	f.trees = slices.DeleteFunc(f.trees, func(tree orca.Worktree) bool { return tree.Path == path })
	return nil
}
func (f *fakeWorkspaces) TerminalList(context.Context) ([]orca.Terminal, error) {
	return f.terminals, f.listErr
}
func (f *fakeWorkspaces) TerminalClose(_ context.Context, handle string) error {
	f.closed = append(f.closed, handle)
	f.actions = append(f.actions, "close "+handle)
	err := f.closeErrors[handle]
	if err == nil {
		f.terminals = slices.DeleteFunc(f.terminals, func(terminal orca.Terminal) bool { return terminal.Handle == handle })
	}
	return err
}

func TestReclaimClosesIdleTerminals(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, state := range []string{"Done", "Canceled"} {
		t.Run(state, func(t *testing.T) {
			const path = "/ws/OF-1-executor"
			fake := &fakeWorkspaces{
				trees: []orca.Worktree{{Path: path}},
				terminals: []orca.Terminal{
					{Handle: "term_path", WorktreePath: path, LastOutputAt: now.Add(-time.Hour).UnixMilli()},
					{Handle: "term_id", WorktreeID: "repo::" + path, LastOutputAt: now.Add(-2 * time.Hour).UnixMilli()},
					{Handle: "term_other", WorktreePath: path + "-2", LastOutputAt: now.UnixMilli()},
				},
			}
			runtime := &Runtime{
				Repositories: map[string]config.Repository{"/repo": {Path: "/repo"}},
				Workspaces:   fake,
				closed:       map[string]map[string]closedTicket{"/repo": {"OF-1": {State: state}}},
				Now:          func() time.Time { return now },
			}
			runtime.Run = func(_ context.Context, spec runx.Spec) ([]byte, error) {
				if spec.Args[0] == "rev-list" {
					return []byte("0\n"), nil
				}
				return nil, nil
			}
			var log bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&log, nil))
			for _, dryRun := range []bool{true, false} {
				verdicts, err := runtime.ReclaimWorkspaces(context.Background(), dryRun, logger)
				if err != nil || len(verdicts) != 1 || len(verdicts[0].Blocks) != 0 {
					t.Fatalf("dryRun=%v verdicts=%+v err=%v", dryRun, verdicts, err)
				}
				if dryRun && len(fake.actions) != 0 {
					t.Fatalf("dry-run mutated workspace: %v", fake.actions)
				}
			}
			if want := []string{"close term_path", "close term_id", "remove " + path}; !slices.Equal(fake.actions, want) {
				t.Fatalf("actions=%v want %v", fake.actions, want)
			}
			for _, handle := range []string{"term_path", "term_id"} {
				if !strings.Contains(log.String(), "level=INFO msg=\"workspace terminal closed\" path="+path+" terminal="+handle) {
					t.Fatalf("missing close log for %s: %s", handle, log.String())
				}
			}
		})
	}
}

func TestReclaimKeepsBlockedTerminals(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, identity, git, output, block string
		lastOutput                         int64
		pinned, openTicket, duplicate      bool
		listErr, gitErr                    error
	}{
		{name: "agent", identity: "codex", lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "terminal-running"},
		{name: "recent exec", lastOutput: now.Add(-time.Minute).UnixMilli(), block: "terminal-running"},
		{name: "before hour", lastOutput: now.Add(-time.Hour + time.Millisecond).UnixMilli(), block: "terminal-running"},
		{name: "unknown output", block: "terminal-running"},
		{name: "invalid output", lastOutput: -1, block: "terminal-running"},
		{name: "future output", lastOutput: now.Add(time.Hour).UnixMilli(), block: "terminal-running"},
		{name: "superseded resolver", openTicket: true, lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "terminal-running"},
		{name: "duplicate resolver", openTicket: true, duplicate: true, lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "terminal-running"},
		{name: "pinned", pinned: true, lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "pinned"},
		{name: "dirty", git: "status", output: "?? notes.txt\n", lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "dirty"},
		{name: "unpushed", git: "rev-list", output: "1\n", lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "unpushed"},
		{name: "terminal list failed", listErr: errors.New("unavailable"), lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "terminal-unknown"},
		{name: "git status failed", git: "status", gitErr: errors.New("unavailable"), lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "git-status-failed"},
		{name: "git rev-list failed", git: "rev-list", gitErr: errors.New("unavailable"), lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "git-rev-list-failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, next := "/ws/OF-1-executor", ""
			closed := map[string]closedTicket{"OF-1": {State: "Done"}}
			if tc.openTicket {
				closed = nil
				path, next = "/ws/OF-1-resolver-1", "/ws/OF-1-resolver-2"
				if tc.duplicate {
					next = "/ws/OF-1-resolver-1-2"
				}
			}
			fake := &fakeWorkspaces{
				trees: []orca.Worktree{{Path: path, Pinned: tc.pinned}},
				terminals: []orca.Terminal{
					{Handle: "term_idle", WorktreePath: path, LastOutputAt: now.Add(-2 * time.Hour).UnixMilli()},
					{Handle: "term_checked", WorktreeID: "repo::" + path, LastOutputAt: tc.lastOutput, AgentIdentity: tc.identity},
				},
				listErr: tc.listErr,
			}
			if next != "" {
				fake.trees = append(fake.trees, orca.Worktree{Path: next})
			}
			runtime := &Runtime{
				Repositories: map[string]config.Repository{"/repo": {Path: "/repo"}},
				Workspaces:   fake,
				closed:       map[string]map[string]closedTicket{"/repo": closed},
				Now:          func() time.Time { return now },
				Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
					if spec.Args[0] == tc.git {
						return []byte(tc.output), tc.gitErr
					}
					if spec.Args[0] == "rev-list" {
						return []byte("0\n"), nil
					}
					return nil, nil
				},
			}
			for _, dryRun := range []bool{true, false} {
				verdicts, err := runtime.ReclaimWorkspaces(context.Background(), dryRun, slog.New(slog.DiscardHandler))
				if err != nil || len(verdicts) != 1 || !slices.Equal(verdicts[0].Blocks, []string{tc.block}) {
					t.Fatalf("dryRun=%v verdicts=%+v err=%v want %s", dryRun, verdicts, err, tc.block)
				}
				if len(fake.actions) != 0 {
					t.Fatalf("blocked workspace mutated: %v", fake.actions)
				}
			}
		})
	}
}

func TestReclaimTerminalCloseFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		stale bool
	}{
		{"stale", fmt.Errorf("close: %w", &orca.RejectedError{Code: "terminal_handle_stale"}), true},
		{"unconfigured", &orca.RejectedError{Code: "agent_unconfigured"}, false},
		{"unavailable", errors.New("close failed"), false},
		{"message only", errors.New("terminal_handle_stale"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const path = "/ws/OF-1-executor"
			fake := &fakeWorkspaces{
				trees: []orca.Worktree{{Path: path}},
				terminals: []orca.Terminal{
					{Handle: "term_first", WorktreePath: path, LastOutputAt: 1},
					{Handle: "term_second", WorktreePath: path, LastOutputAt: 1},
				},
				closeErrors: map[string]error{"term_second": tc.err},
			}
			runtime := &Runtime{
				Repositories: map[string]config.Repository{"/repo": {Path: "/repo"}},
				Workspaces:   fake,
				closed:       map[string]map[string]closedTicket{"/repo": {"OF-1": {State: "Done"}}},
				Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
					if spec.Args[0] == "rev-list" {
						return []byte("0\n"), nil
					}
					return nil, nil
				},
			}
			var log bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&log, nil))
			verdicts, err := runtime.ReclaimWorkspaces(context.Background(), false, logger)
			if err != nil || len(verdicts) != 1 {
				t.Fatalf("verdicts=%+v err=%v", verdicts, err)
			}
			if tc.stale {
				if len(verdicts[0].Blocks) != 0 || !slices.Equal(fake.actions, []string{"close term_first", "close term_second", "remove " + path}) || strings.Count(log.String(), "workspace terminal closed") != 2 {
					t.Fatalf("verdicts=%+v actions=%v log=%s", verdicts, fake.actions, log.String())
				}
				return
			}
			if !slices.Equal(verdicts[0].Blocks, []string{"terminal-close-failed"}) || len(fake.removed) != 0 || strings.Count(log.String(), "workspace terminal closed") != 1 {
				t.Fatalf("verdicts=%+v removed=%v log=%s", verdicts, fake.removed, log.String())
			}
			delete(fake.closeErrors, "term_second")
			verdicts, err = runtime.ReclaimWorkspaces(context.Background(), false, logger)
			if err != nil || len(verdicts) != 1 || len(verdicts[0].Blocks) != 0 || !slices.Equal(fake.actions, []string{"close term_first", "close term_second", "close term_second", "remove " + path}) {
				t.Fatalf("retry verdicts=%+v err=%v actions=%v", verdicts, err, fake.actions)
			}
		})
	}
}

// Terminal tickets lose every workspace, active tickets lose only superseded
// resolver rounds, and each safety condition keeps a workspace with one WARN.
func TestReclaimWorkspaces(t *testing.T) {
	const merged = "1111111111111111111111111111111111111111"
	ticket := func(id, state string) task.Issue {
		return task.Issue{ID: id, Identifier: id, Project: task.NamedID{ID: "p"}, State: task.NamedID{Name: state}}
	}
	lc := &fakeIssues{
		issues:        []task.Issue{ticket("OF-1", "Done"), ticket("OF-2", "Canceled"), ticket("OF-3", "Canceled"), ticket("OF-4", "In Progress"), ticket("OF-5", "Done"), ticket("OF-6", "Done"), ticket("OF-7", "Done"), ticket("OF-8", "In Progress")},
		issueComments: []task.Comment{{ID: "c", Body: `merge-handoff-v1: {"queue_id":"q","head":"` + merged + `"}`}},
	}
	ws := func(name string) orca.Worktree { return orca.Worktree{Path: "/ws/" + name} }
	fake := &fakeWorkspaces{
		trees:     []orca.Worktree{{Path: "/repo", Main: true}, ws("OF-1-executor"), ws("OF-1-resolver-1"), ws("OF-2-executor"), ws("OF-3-executor"), ws("OF-4-executor"), ws("OF-4-resolver-1"), ws("OF-4-resolver-2"), ws("OF-5-executor"), {Path: "/ws/OF-6-executor", Pinned: true}, ws("OF-7-executor"), ws("OF-8-resolver-3"), ws("OF-8-resolver-3-3"), ws("OF-8-resolver-3-2"), ws("shearwater")},
		terminals: []orca.Terminal{{Handle: "term_1", WorktreeID: "repo::/ws/OF-5-executor", LastOutputAt: 1, AgentIdentity: "codex"}},
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
		case "merge-base":
			if (name == "OF-1-executor" || name == "OF-7-executor") && slices.Equal(s.Args[1:], []string{"--is-ancestor", "HEAD", merged}) {
				return nil, nil
			}
			return nil, errors.New("exit status 1")
		case "rev-list":
			if name == "OF-3-executor" || name == "OF-1-executor" || name == "OF-7-executor" {
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
	reasons := map[string]string{}
	for _, v := range verdicts {
		got[filepath.Base(v.Path)] = strings.Join(v.Blocks, ",")
		reasons[filepath.Base(v.Path)] = v.Reason
	}
	want := map[string]string{"OF-1-executor": "", "OF-1-resolver-1": "", "OF-2-executor": "dirty", "OF-3-executor": "unpushed", "OF-4-resolver-1": "", "OF-5-executor": "terminal-running", "OF-6-executor": "pinned", "OF-7-executor": "", "OF-8-resolver-3": "", "OF-8-resolver-3-2": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("verdicts=%v want %v", got, want)
	}
	if reasons["OF-8-resolver-3"] != "superseded by duplicate of resolver round 3" {
		t.Fatalf("duplicate reason=%q", reasons["OF-8-resolver-3"])
	}

	for range 2 {
		if _, err := r.ReclaimWorkspaces(context.Background(), false, logger); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(fake.removed)
	if want := []string{"OF-1-executor", "OF-1-resolver-1", "OF-4-resolver-1", "OF-7-executor", "OF-8-resolver-3", "OF-8-resolver-3-2"}; !reflect.DeepEqual(fake.removed, want) {
		t.Fatalf("removed=%v want %v", fake.removed, want)
	}
	if n := strings.Count(log.String(), "workspace kept"); n != 4 {
		t.Fatalf("kept warnings=%d want 4 (one per kept workspace)\n%s", n, log.String())
	}
}
