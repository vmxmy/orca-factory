package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/projects"
)

// writeExecutable writes an executable stand-in script.
func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestFactoryCheckerThreeStates is OF-53's check contract: exit 0 is in
// step, exit 3 is core behind with the BEHIND files, any other exit is a
// failed check.
func TestFactoryCheckerThreeStates(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		behind       []string
		failed       bool
	}{
		{"in step", "echo 'check: ok       gate.sh'\nexit 0\n", nil, false},
		{"behind", "echo 'check: ok       gate.sh'\necho 'check: BEHIND   install.sh'\necho 'check: BEHIND   roles/onboard.md'\nexit 3\n", []string{"install.sh", "roles/onboard.md"}, false},
		{"modified", "echo 'check: BEHIND   install.sh'\necho 'check: DIFFERS  gate.sh'\nexit 1\n", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory := t.TempDir()
			writeExecutable(t, filepath.Join(factory, "bin", "orca-factory"), tc.script)
			behind, err := factoryChecker(factory)(context.Background(), t.TempDir())
			if (err != nil) != tc.failed || !slices.Equal(behind, tc.behind) {
				t.Fatalf("behind=%v err=%v, want behind=%v failed=%v", behind, err, tc.behind, tc.failed)
			}
		})
	}
}

// coreSyncFixture is a target repository cloned from a local bare origin, a
// factory checkout whose bin/orca-factory stand-in rewrites one core file
// (and one file outside scripts/agent-gates/), and a gh stand-in on PATH
// that answers `pr list` from prList and records every call.
type coreSyncFixture struct {
	origin, repo, factory, ghLog, prList string
	prView, prChecks, merged             string
	syncer                               coreSyncer
}

func newCoreSyncFixture(t *testing.T) coreSyncFixture {
	t.Helper()
	root := t.TempDir()
	f := coreSyncFixture{origin: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "repo"), factory: filepath.Join(root, "factory"), ghLog: filepath.Join(root, "gh.log"), prList: filepath.Join(root, "pr-list.json"), prView: filepath.Join(root, "pr-view.json"), prChecks: filepath.Join(root, "pr-checks.json"), merged: filepath.Join(root, "merged")}
	t.Setenv("AGENT_DRIVER_STATE_ROOT", filepath.Join(root, "state"))
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(key, "t")
	}
	for _, key := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(key, "t@example.com")
	}
	gitIn(t, root, "init", "--quiet", "--bare", "-b", "main", f.origin)
	gitIn(t, root, "clone", "--quiet", f.origin, f.repo)
	writeExecutable(t, filepath.Join(f.repo, "scripts", "agent-gates", "install.sh"), "echo old\n")
	gitIn(t, f.repo, "add", ".")
	gitIn(t, f.repo, "commit", "--quiet", "-m", "init")
	gitIn(t, f.repo, "push", "--quiet", "origin", "HEAD:main")

	writeExecutable(t, filepath.Join(f.factory, "bin", "orca-factory"), `[ "$1" = update ] || exit 9
printf '#!/bin/sh\necho new\n' > "$2/scripts/agent-gates/install.sh"
echo stray > "$2/outside.txt"
`)
	gitIn(t, root, "init", "--quiet", "-b", "main", f.factory)
	gitIn(t, f.factory, "add", "--force", ".")
	gitIn(t, f.factory, "commit", "--quiet", "-m", "factory")

	bin := filepath.Join(root, "bin")
	writeExecutable(t, filepath.Join(bin, "gh"), `printf '%s\n' "$*" >> `+f.ghLog+`
case "$1 $2" in
"pr list") cat `+f.prList+` ;;
"pr create") echo https://github.com/example/repo/pull/9 ;;
"pr close") ;;
"pr view") if [ -e `+f.merged+` ]; then echo '{"state":"MERGED"}'; else cat `+f.prView+`; fi ;;
"pr checks") cat `+f.prChecks+` ;;
"pr merge") touch `+f.merged+` ;;
*) exit 9 ;;
esac
`)
	if err := os.WriteFile(f.prList, []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.prView, []byte(`{"state":"MERGED"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.syncer = coreSyncer{factoryRoot: f.factory, cacheDir: filepath.Join(root, "cache")}
	return f
}

func (f coreSyncFixture) project() projects.Project {
	return projects.Project{Path: f.repo, Remote: "github.com/example/repo", CoreBehind: []string{"install.sh"}}
}

func (f coreSyncFixture) prCreates(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(f.ghLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Count(string(b), "pr create")
}

// TestCoreSyncOpensOnePRPerFactoryVersion is OF-53: a lagging project gets
// one sync branch at origin/main carrying only scripts/agent-gates/ changes
// from `orca-factory update`, one `gh pr create`, and no leftover worktree or
// local branch; a second sync for the same factory version opens nothing.
func TestCoreSyncOpensOnePRPerFactoryVersion(t *testing.T) {
	f := newCoreSyncFixture(t)
	branch := "chore/orca-factory-core-sync-" + gitIn(t, f.factory, "rev-parse", "--short", "HEAD")
	for range 2 {
		if err := f.syncer.sync(context.Background(), f.project()); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.prCreates(t); n != 1 {
		t.Fatalf("pr create called %d times, want once", n)
	}
	files := gitIn(t, f.origin, "diff", "--name-only", "main", branch)
	if files != "scripts/agent-gates/install.sh" {
		t.Fatalf("sync branch changed %q, want only the core file", files)
	}
	if got := gitIn(t, f.origin, "show", branch+":scripts/agent-gates/install.sh"); !strings.Contains(got, "echo new") {
		t.Fatalf("install.sh=%q", got)
	}
	worktree := filepath.Join(f.syncer.cacheDir, "orca-factory", "core-sync", "github.com_example_repo")
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still present: %v", err)
	}
	if local := gitIn(t, f.repo, "branch", "--list", branch); local != "" {
		t.Fatalf("local branch left behind: %q", local)
	}
}

// TestCoreSyncSupersedesOlderSyncPR: an unmerged sync PR from an earlier
// factory version is closed with its branch, so the project is not stuck on
// stale core while nobody merges it; the current version still opens once.
func TestCoreSyncSupersedesOlderSyncPR(t *testing.T) {
	f := newCoreSyncFixture(t)
	if err := os.WriteFile(f.prList, []byte(`[{"headRefName":"chore/orca-factory-core-sync-0ld5a1"},{"headRefName":"feature/x"}]`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.syncer.sync(context.Background(), f.project()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if log := string(b); !strings.Contains(log, "pr close chore/orca-factory-core-sync-0ld5a1 --delete-branch") || strings.Contains(log, "pr close feature/x") {
		t.Fatalf("gh calls %q", log)
	}
	if n := f.prCreates(t); n != 1 {
		t.Fatalf("pr create called %d times, want once", n)
	}
}

// TestCoreSyncAutoMerge is OF-133: once the current factory version's sync PR
// touches only scripts/agent-gates/, every required check has passed and it
// is mergeable, the next sync squash-merges it at the checked head and reads
// back MERGED; any unmet condition leaves it open with an error (WARN, retry
// on the next discovery), and an already merged PR is a no-op.
func TestCoreSyncAutoMerge(t *testing.T) {
	const passed = `[{"name":"go","bucket":"pass"},{"name":"gate","bucket":"pass"}]`
	for _, tc := range []struct {
		name, files, checks string
		merge               bool
	}{
		{"all conditions met", `[{"path":"scripts/agent-gates/install.sh"}]`, passed, true},
		{"file outside agent-gates", `[{"path":"scripts/agent-gates/install.sh"},{"path":"outside.txt"}]`, passed, false},
		{"check not passed", `[{"path":"scripts/agent-gates/install.sh"}]`, `[{"name":"go","bucket":"pass"},{"name":"gate","bucket":"pending"}]`, false},
		{"no required checks", `[{"path":"scripts/agent-gates/install.sh"}]`, `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoreSyncFixture(t)
			if err := f.syncer.sync(context.Background(), f.project()); err != nil {
				t.Fatal(err)
			}
			branch := "chore/orca-factory-core-sync-" + gitIn(t, f.factory, "rev-parse", "--short", "HEAD")
			view := `{"state":"OPEN","headRefName":"` + branch + `","headRefOid":"abc123","mergeable":"MERGEABLE","files":` + tc.files + "}\n"
			if err := os.WriteFile(f.prView, []byte(view), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.prChecks, []byte(tc.checks+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := f.syncer.sync(context.Background(), f.project())
			b, readErr := os.ReadFile(f.ghLog)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !tc.merge {
				if err == nil || strings.Contains(string(b), "pr merge") {
					t.Fatalf("err=%v gh calls %q, want no merge and an error", err, b)
				}
				return
			}
			if err != nil || strings.Count(string(b), "pr merge "+branch+" --squash --match-head-commit abc123") != 1 {
				t.Fatalf("err=%v gh calls %q, want one squash merge at the checked head", err, b)
			}
			if err := f.syncer.sync(context.Background(), f.project()); err != nil {
				t.Fatalf("sync after merge: %v", err)
			}
			if b, _ := os.ReadFile(f.ghLog); strings.Count(string(b), "pr merge") != 1 {
				t.Fatalf("merged PR merged again: %q", b)
			}
		})
	}
}
