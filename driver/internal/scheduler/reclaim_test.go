package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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
	repoTrees   map[string][]orca.Worktree
	treeErrors  map[string]error
	terminals   []orca.Terminal
	removed     []string
	closed      []string
	closeErrors map[string]error
	listErr     error
	actions     []string
	onRemove    func(string) error
}

func (f *fakeWorkspaces) WorktreeList(_ context.Context, repo string) ([]orca.Worktree, error) {
	return f.repoTrees[repo], f.treeErrors[repo]
}
func (f *fakeWorkspaces) WorktreeRemove(_ context.Context, path string) error {
	if f.onRemove != nil {
		if err := f.onRemove(path); err != nil {
			return err
		}
	}
	f.actions = append(f.actions, "remove "+path)
	f.removed = append(f.removed, filepath.Base(path))
	for repo, trees := range f.repoTrees {
		f.repoTrees[repo] = slices.DeleteFunc(trees, func(tree orca.Worktree) bool { return tree.Path == path })
	}
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
				repoTrees: map[string][]orca.Worktree{"/repo": {{Path: path}}},
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
		{name: "dirty and pinned", pinned: true, git: "status", output: "?? notes.txt\n", lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "pinned,dirty"},
		{name: "unpushed and pinned", pinned: true, git: "rev-list", output: "1\n", lastOutput: now.Add(-2 * time.Hour).UnixMilli(), block: "pinned,unpushed"},
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
				repoTrees: map[string][]orca.Worktree{"/repo": {{Path: path, Pinned: tc.pinned}}},
				terminals: []orca.Terminal{
					{Handle: "term_idle", WorktreePath: path, LastOutputAt: now.Add(-2 * time.Hour).UnixMilli()},
					{Handle: "term_checked", WorktreeID: "repo::" + path, LastOutputAt: tc.lastOutput, AgentIdentity: tc.identity},
				},
				listErr: tc.listErr,
			}
			if next != "" {
				fake.repoTrees["/repo"] = append(fake.repoTrees["/repo"], orca.Worktree{Path: next})
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
				if err != nil || len(verdicts) != 1 || !slices.Equal(verdicts[0].Blocks, strings.Split(tc.block, ",")) {
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
				repoTrees: map[string][]orca.Worktree{"/repo": {{Path: path}}},
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
		repoTrees: map[string][]orca.Worktree{"/repo": {
			{Path: "/repo", Main: true}, ws("OF-1-executor"), ws("OF-1-resolver-1"),
			{Path: "/ws/OF-2-executor", Pinned: true}, {Path: "/ws/OF-3-executor", Pinned: true},
			ws("OF-4-executor"), ws("OF-4-resolver-1"), ws("OF-4-resolver-2"), ws("OF-5-executor"),
			{Path: "/ws/OF-6-executor", Pinned: true}, ws("OF-7-executor"), ws("OF-8-resolver-3"),
			ws("OF-8-resolver-3-3"), ws("OF-8-resolver-3-2"), ws("shearwater"),
		}},
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
	want := map[string]string{"OF-1-executor": "", "OF-1-resolver-1": "", "OF-2-executor": "pinned,dirty", "OF-3-executor": "pinned,unpushed", "OF-4-resolver-1": "", "OF-5-executor": "terminal-running", "OF-6-executor": "pinned", "OF-7-executor": "", "OF-8-resolver-3": "", "OF-8-resolver-3-2": ""}
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

func TestReclaimBacksUpDirtyAndUnpushedClosedWorkspace(t *testing.T) {
	const submoduleNotes = "notes\x00inside gitlink\n"
	for _, scenario := range []string{
		"restore", "clean-submodule", "submodule-local-file", "populated-submodule", "open-ticket", "dry-run",
		"state-inside", "state-inside-peer", "state-inside-other-repo", "other-repo-list-failed",
		"unwritable", "reset-failed", "terminal-close-failed",
	} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			git := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			repo := filepath.Join(root, "repo")
			git(root, "init", "--initial-branch=main", repo)
			git(repo, "config", "user.email", "test@example.com")
			git(repo, "config", "user.name", "Test")
			git(repo, "config", "core.autocrlf", "input")
			git(repo, "config", "color.diff", "always")
			git(repo, "config", "diff.context", "0")
			write(filepath.Join(repo, "crlf"), "a\nb\nc\n")
			write(filepath.Join(repo, "text"), "first\nbefore\nlast\n")
			write(filepath.Join(repo, "tracked"), "original\n")
			if scenario == "restore" {
				if err := os.Mkdir(filepath.Join(repo, "old"), 0700); err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(repo, "old", "tracked"), "removed with its directory")
			}
			write(filepath.Join(repo, ".gitignore"), ".omc/\n")
			git(repo, "add", ".")
			git(repo, "commit", "-m", "base")
			if scenario == "clean-submodule" || scenario == "submodule-local-file" || scenario == "populated-submodule" {
				base := git(repo, "rev-parse", "HEAD")
				git(repo, "update-index", "--add", "--cacheinfo", "160000,"+base+",sub")
				git(repo, "commit", "-m", "track submodule")
			}
			origin := filepath.Join(root, "origin.git")
			git(root, "clone", "--bare", repo, origin)
			git(repo, "remote", "add", "origin", origin)
			git(repo, "fetch", "origin")
			name := "OF-642-executor"
			if scenario == "open-ticket" {
				name = "OF-642-resolver-1"
			}
			workspace := filepath.Join(root, name)
			git(repo, "worktree", "add", "-b", "local", workspace)
			committed := "local commit\x00bytes\n"
			tracked := "modified\x00tracked\n"
			untracked := "untracked\x00bytes\n"
			write(filepath.Join(workspace, "committed"), committed)
			git(workspace, "add", "committed")
			git(workspace, "commit", "-m", "local only")
			head := git(workspace, "rev-parse", "HEAD")
			crlf := "a\r\nb\r\nc\r\n"
			write(filepath.Join(workspace, "crlf"), crlf)
			write(filepath.Join(workspace, "text"), "first\nafter\nlast\n")
			write(filepath.Join(workspace, "tracked"), tracked)
			write(filepath.Join(workspace, "-untracked\nfile"), untracked)
			if err := os.Mkdir(filepath.Join(workspace, ".omc"), 0700); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(workspace, ".omc", "ignored"), "excluded")
			state := filepath.Join(root, "state")
			fake := &fakeWorkspaces{repoTrees: map[string][]orca.Worktree{repo: {{Path: workspace}}}}
			repos := map[string]config.Repository{repo: {Path: repo}}
			repoB := filepath.Join(root, "repo-b")
			if scenario == "state-inside-other-repo" || scenario == "other-repo-list-failed" {
				git(root, "init", repoB)
				repos[repoB] = config.Repository{Path: repoB}
			}
			var listErr error
			switch scenario {
			case "restore":
				if err := os.RemoveAll(filepath.Join(workspace, "old")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(".", filepath.Join(workspace, "old")); err != nil {
					t.Fatal(err)
				}
			case "unwritable":
				write(state, "a regular file prevents creating the backup directory")
			case "populated-submodule":
				git(filepath.Join(workspace, "sub"), "init")
			case "submodule-local-file":
				if err := os.MkdirAll(filepath.Join(workspace, "sub"), 0700); err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(workspace, "sub", "notes"), submoduleNotes)
			case "state-inside":
				state = filepath.Join(workspace, ".omc", "state")
			case "state-inside-peer", "state-inside-other-repo":
				peer := filepath.Join(root, "OF-643-executor")
				if scenario != "state-inside-other-repo" {
					git(repo, "worktree", "add", "--detach", peer)
					fake.repoTrees[repo] = append(fake.repoTrees[repo], orca.Worktree{Path: peer})
				} else {
					if err := os.Mkdir(peer, 0700); err != nil {
						t.Fatal(err)
					}
					fake.repoTrees[repoB] = []orca.Worktree{{Path: peer}}
				}
				if err := os.Symlink(peer, state); err != nil {
					t.Fatal(err)
				}
			case "other-repo-list-failed":
				listErr = errors.New("repo-b worktree list failed")
				fake.treeErrors = map[string]error{repoB: listErr}
			case "reset-failed":
				write(git(workspace, "rev-parse", "--path-format=absolute", "--git-path", "index.lock"), "")
			case "terminal-close-failed":
				fake.closeErrors = map[string]error{"idle": errors.New("close failed")}
			}
			backupRoot := filepath.Join(state, "branch-backups")
			closed := map[string]closedTicket{"OF-642": {State: "Done"}}
			if scenario == "open-ticket" {
				closed = nil
				fake.repoTrees[repo] = append(fake.repoTrees[repo], orca.Worktree{Path: filepath.Join(root, "OF-642-resolver-2")})
			} else {
				fake.terminals = []orca.Terminal{{Handle: "idle", WorktreePath: workspace, LastOutputAt: 1}}
			}
			removeCalled := false
			var backup string
			fake.onRemove = func(path string) error {
				removeCalled = true
				entries, err := os.ReadDir(backupRoot)
				if err != nil || len(entries) != 1 {
					return fmt.Errorf("backup before delete: %v, %v", entries, err)
				}
				if want := "20261121T063342Z-" + name; entries[0].Name() != want {
					return fmt.Errorf("backup name %q want %q", entries[0].Name(), want)
				}
				backup = filepath.Join(backupRoot, entries[0].Name())
				for _, name := range []string{"workspace.bundle", "tracked.patch", "worktree.tar"} {
					if _, err := os.Stat(filepath.Join(backup, name)); err != nil {
						t.Error(err)
					}
				}
				if got, err := os.ReadFile(filepath.Join(path, ".omc", "ignored")); err != nil || string(got) != "excluded" {
					t.Errorf("ignored file before delete: %q, %v", got, err)
				}
				return exec.Command("git", "-C", repo, "worktree", "remove", path).Run()
			}
			runtime := &Runtime{StateRoot: state, Now: func() time.Time { return time.Date(2026, 11, 21, 14, 33, 42, 0, time.FixedZone("CST", 8*3600)) }, Repositories: repos, Workspaces: fake, closed: map[string]map[string]closedTicket{repo: closed}}
			runtime.Run = func(ctx context.Context, spec runx.Spec) ([]byte, error) {
				if spec.Path == "git" && len(spec.Args) >= 2 && spec.Args[0] == "bundle" && spec.Args[1] == "create" {
					fake.actions = append(fake.actions, "backup")
				}
				return runx.Run(ctx, spec)
			}
			verdicts, err := runtime.ReclaimWorkspaces(context.Background(), scenario == "dry-run", slog.New(slog.DiscardHandler))
			if !errors.Is(err, listErr) || len(verdicts) != 1 {
				t.Fatalf("verdicts=%+v err=%v", verdicts, err)
			}
			checkFiles := func(dir string) {
				t.Helper()
				for name, want := range map[string]string{
					"committed": committed, "tracked": tracked, "crlf": crlf,
					"text": "first\nafter\nlast\n", "-untracked\nfile": untracked,
				} {
					got, err := os.ReadFile(filepath.Join(dir, name))
					if err != nil || string(got) != want {
						t.Fatalf("%s/%q: %q, %v; want %q", dir, name, got, err, want)
					}
				}
			}
			if scenario == "restore" || scenario == "clean-submodule" || scenario == "submodule-local-file" {
				if len(verdicts[0].Blocks) != 0 || !slices.Equal(fake.actions, []string{"close idle", "backup", "remove " + workspace}) {
					t.Fatalf("verdicts=%+v actions=%v", verdicts, fake.actions)
				}
				git(repo, "branch", "-D", "local")
				restored := filepath.Join(root, "restored")
				git(root, "clone", origin, restored)
				git(restored, "fetch", filepath.Join(backup, "workspace.bundle"), "HEAD")
				git(restored, "checkout", "--detach", "FETCH_HEAD")
				if got := git(restored, "rev-parse", "HEAD"); got != head {
					t.Fatalf("restored HEAD=%s want %s", got, head)
				}
				git(restored, "apply", "--binary", filepath.Join(backup, "tracked.patch"))
				cmd := exec.Command("tar", "-xf", filepath.Join(backup, "worktree.tar"), "-C", restored)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("restore tar: %v %s", err, out)
				}
				checkFiles(restored)
				if scenario == "submodule-local-file" {
					got, err := os.ReadFile(filepath.Join(restored, "sub", "notes"))
					if err != nil || string(got) != submoduleNotes {
						t.Fatalf("restored sub/notes = %q, %v; want %q", got, err, submoduleNotes)
					}
				}
				if scenario == "restore" {
					if dest, err := os.Readlink(filepath.Join(restored, "old")); err != nil || dest != "." {
						t.Fatalf("restored link = %q, %v", dest, err)
					}
					members, err := exec.Command("tar", "-tf", filepath.Join(backup, "worktree.tar")).Output()
					if err != nil || slices.Contains(strings.Split(string(members), "\n"), "old/tracked") {
						t.Fatalf("archive includes a deleted file through its replacement symlink: %q, %v", members, err)
					}
				}
				if _, err := os.Stat(filepath.Join(restored, ".omc")); !os.IsNotExist(err) {
					t.Fatalf("ignored files restored: %v", err)
				}
				return
			}
			wantBlocks := []string{"backup-failed"}
			wantActions := []string{"close idle"}
			switch scenario {
			case "open-ticket", "dry-run":
				wantBlocks = []string{"dirty", "unpushed"}
				wantActions = nil
			case "reset-failed":
				wantBlocks = []string{"remove-failed"}
				wantActions = append(wantActions, "backup")
			case "terminal-close-failed":
				wantBlocks = []string{"terminal-close-failed"}
			}
			if removeCalled || !slices.Equal(fake.actions, wantActions) {
				t.Fatalf("retained workspace: remove called=%v actions=%v want %v", removeCalled, fake.actions, wantActions)
			}
			if got := git(workspace, "rev-parse", "HEAD"); got != head {
				t.Fatalf("retained HEAD=%s want %s", got, head)
			}
			checkFiles(workspace)
			if !slices.Equal(verdicts[0].Blocks, wantBlocks) {
				t.Fatalf("blocks=%v want %v", verdicts[0].Blocks, wantBlocks)
			}
			if scenario != "unwritable" && scenario != "reset-failed" {
				if _, err := os.Stat(backupRoot); !os.IsNotExist(err) {
					t.Fatalf("unexpected backup directory: %v", err)
				}
			}
		})
	}
}
