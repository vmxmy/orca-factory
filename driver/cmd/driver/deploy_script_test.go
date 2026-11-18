package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMergeScriptKeepsWorktreeWithUnmergedWork(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    string
		keep      bool
		doneFails bool
	}{
		{name: "different PR", change: "diverged", keep: true},
		{name: "commits after PR head", change: "ahead", keep: true},
		{name: "unstaged changes", change: "unstaged", keep: true},
		{name: "staged changes", change: "staged", keep: true},
		{name: "untracked files", change: "untracked", keep: true},
		{name: "clean PR head"},
		{name: "ancestor of PR head", change: "ancestor"},
		{name: "ignored files", change: "ignored"},
		{name: "Done update fails", doneFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, "bin")
			repo := filepath.Join(home, "repo")
			wt := filepath.Join(home, "worktree")
			for _, dir := range []string{bin, repo, filepath.Join(home, ".config/orca-factory"), filepath.Join(repo, "scripts/agent-gates")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("ORCA_FACTORY_REPO", repo)
			t.Setenv("DONE_FAILS", strconv.FormatBool(tc.doneFails))
			git := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git(repo, "init", "-b", "main")
			git(repo, "config", "user.email", "test@example.com")
			git(repo, "config", "user.name", "Test")
			git(repo, "config", "commit.gpgsign", "false")
			write(filepath.Join(repo, ".gitignore"), "ignored\nscripts/\n")
			write(filepath.Join(repo, "tracked"), "base\n")
			git(repo, "add", ".")
			git(repo, "commit", "-m", "base")
			base := git(repo, "rev-parse", "HEAD")
			git(repo, "commit", "--allow-empty", "-m", "PR head")
			t.Setenv("PR_HEAD", git(repo, "rev-parse", "HEAD"))
			git(repo, "init", "--bare", filepath.Join(home, "remote.git"))
			git(repo, "remote", "add", "origin", filepath.Join(home, "remote.git"))
			git(repo, "worktree", "add", "-b", "candidate", wt)
			switch tc.change {
			case "diverged", "ancestor":
				git(wt, "reset", "--hard", base)
				if tc.change == "diverged" {
					git(wt, "commit", "--allow-empty", "-m", "another PR")
				}
			case "ahead":
				git(wt, "commit", "--allow-empty", "-m", "unmerged work")
			case "unstaged", "staged":
				write(filepath.Join(wt, "tracked"), "local changes\n")
				if tc.change == "staged" {
					git(wt, "add", "tracked")
				}
			case "untracked", "ignored":
				write(filepath.Join(wt, tc.change), "local file\n")
			}
			write(filepath.Join(bin, "gh"), `#!/bin/bash
set -eu
case "$*" in
  "pr checks "*" --json name "*) echo 8 ;;
  "pr checks "*" --watch "*|"pr merge "*) ;;
  "pr view "*" --json headRefOid "*) echo "$PR_HEAD" ;;
  "pr view "*" --json state,mergeCommit "*) echo 'MERGED abc1234' ;;
  *) exit 99 ;;
esac
`)
			write(filepath.Join(home, ".config/orca-factory/driver.env"), "")
			write(filepath.Join(repo, "scripts/agent-gates/agent-graph.sh"), `#!/bin/bash
set -eu
[ "$*" = 'issues status set OF-565 --to Done' ] || exit 99
echo attempted > "$HOME/done-attempted"
echo '{"name":"Done"}'
[ "$DONE_FAILS" = false ] || exit 42
`)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "bash", "../../deploy/ops/merge.sh", "123", "OF-565", wt).CombinedOutput()
			if (err != nil) != tc.doneFails {
				t.Errorf("exit error = %v, want failure %v; output:\n%s", err, tc.doneFails, output)
			}
			if _, err := os.Stat(filepath.Join(home, "done-attempted")); err != nil {
				t.Errorf("Done update was not attempted: %v; output:\n%s", err, output)
			}
			_, statErr := os.Stat(wt)
			if tc.keep {
				if statErr != nil {
					t.Fatalf("worktree must be preserved: %v; output:\n%s", statErr, output)
				}
				git(repo, "show-ref", "--verify", "refs/heads/candidate")
				if !strings.Contains(string(output), "保留 worktree") {
					t.Errorf("missing retention reason; output:\n%s", output)
				}
			} else {
				if !os.IsNotExist(statErr) {
					t.Errorf("worktree must be removed, stat error = %v; output:\n%s", statErr, output)
				}
				if branches := git(repo, "branch", "--list", "candidate"); branches != "" {
					t.Errorf("branch must be removed, got %q", branches)
				}
			}
		})
	}
}

func TestDeployFetchRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failures   int
		buildFails bool
		wantEvents string
	}{
		{"first success", 0, false, "fetch\nmerge\nrev-parse\ndrain\nidle\nbuild\nrestart\nsleep 5\nis-active\nresume\nversion\n"},
		{"second success", 1, false, "fetch\nsleep 5\nfetch\nmerge\nrev-parse\ndrain\nidle\nbuild\nrestart\nsleep 5\nis-active\nresume\nversion\n"},
		{"third success", 2, false, "fetch\nsleep 5\nfetch\nsleep 15\nfetch\nmerge\nrev-parse\ndrain\nidle\nbuild\nrestart\nsleep 5\nis-active\nresume\nversion\n"},
		{"exhausted", 3, false, "fetch\nsleep 5\nfetch\nsleep 15\nfetch\n"},
		{"build failure after drain", 0, true, "fetch\nmerge\nrev-parse\ndrain\nidle\nbuild\nresume\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, ".local/bin")
			repo := filepath.Join(home, "repo")
			for _, dir := range []string{bin, filepath.Join(repo, "driver")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			scripts := map[string]string{
				"git": `echo "$1" >> "$EVENTS"
case "$1" in
  fetch)
    count=0
    if [ -f "$HOME/fetch-count" ]; then read -r count < "$HOME/fetch-count"; fi
    count=$((count + 1))
    echo "$count" > "$HOME/fetch-count"
    [ "$count" -gt "$FETCH_FAILURES" ] || exit 42
    ;;
  merge) ;;
  rev-parse) echo abc123 ;;
  *) exit 99 ;;
esac
`,
				"sleep":   "echo \"sleep $1\" >> \"$EVENTS\"\n",
				"flock":   "exit 0\n",
				"timeout": "echo idle >> \"$EVENTS\"\n",
				"systemctl": `echo "$2" >> "$EVENTS"
`,
				"go": `echo build >> "$EVENTS"
[ "$BUILD_FAILS" = false ] || exit 43
cp "$HOME/.local/bin/orca-factory-driver" "$HOME/.local/bin/orca-factory-driver.new"
`,
				"orca-factory-driver": `case "$*" in
  "maintenance drain --wait 2m") echo drain >> "$EVENTS"; echo '{"mode":"draining"}' ;;
  "maintenance resume --wait 2m") echo resume >> "$EVENTS"; echo '{"mode":"running"}' ;;
  version) echo version >> "$EVENTS"; echo abc123 ;;
  *) exit 99 ;;
esac
`,
			}
			for name, body := range scripts {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\nset -eu\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			events := filepath.Join(home, "events")
			t.Setenv("HOME", home)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("ORCA_FACTORY_REPO", repo)
			t.Setenv("EVENTS", events)
			t.Setenv("FETCH_FAILURES", strconv.Itoa(tc.failures))
			t.Setenv("BUILD_FAILS", strconv.FormatBool(tc.buildFails))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "bash", "../../deploy/ops/deploy.sh").CombinedOutput()
			wantFailure := tc.failures == 3 || tc.buildFails
			if (err != nil) != wantFailure {
				t.Errorf("exit error = %v, want failure %v; output:\n%s", err, wantFailure, output)
			}
			got, readErr := os.ReadFile(events)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != tc.wantEvents {
				t.Errorf("events:\n%s\nwant:\n%s", got, tc.wantEvents)
			}
			if gotMessage := strings.Contains(string(output), "尚未 drain，未改动任何状态"); gotMessage != (tc.failures == 3) {
				t.Errorf("fetch exhaustion message = %v; output:\n%s", gotMessage, output)
			}
			backups, globErr := filepath.Glob(filepath.Join(home, ".local/state/orca-factory/deploy-backups/*/orca-factory-driver"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			wantBackups := 1
			if tc.failures == 3 {
				wantBackups = 0
			}
			if len(backups) != wantBackups {
				t.Errorf("backups = %v, want %d", backups, wantBackups)
			}
		})
	}
}

func TestDeployScriptFailureAfterDrainRestores(t *testing.T) {
	for _, tc := range []struct {
		name       string
		buildFails bool
		wantExit   int
		wantEvents string
	}{
		{"build failure", true, 43, "drain\nidle\nbuild\nresume old\n"},
		{"liveness failure", false, 45, "drain\nidle\nbuild\nrestart new\nis-active new\nrestart old\nresume old\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, ".local/bin")
			repo := filepath.Join(home, "repo")
			for _, dir := range []string{bin, filepath.Join(repo, "driver")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			scripts := map[string]string{
				"git": `case "$1" in
  fetch|merge) ;;
  rev-parse) echo abc123 ;;
  *) exit 99 ;;
esac
`,
				"sleep":   "exit 0\n",
				"flock":   "exit 0\n",
				"timeout": "echo idle >> \"$EVENTS\"\n",
				"systemctl": `version=old
if grep -q 'broken new binary' "$HOME/.local/bin/orca-factory-driver"; then version=new; fi
echo "$2 $version" >> "$EVENTS"
[ "$2" != is-active ] || exit 45
`,
				"go": `echo build >> "$EVENTS"
[ "$BUILD_FAILS" = false ] || exit 43
cat > "$HOME/.local/bin/orca-factory-driver.new" <<'BINARY'
#!/bin/bash
echo 'broken new binary' >> "$EVENTS"
exit 44
BINARY
chmod +x "$HOME/.local/bin/orca-factory-driver.new"
`,
				"orca-factory-driver": `case "$*" in
  "maintenance drain --wait 2m") echo drain >> "$EVENTS"; echo '{"mode":"draining"}' ;;
  "maintenance resume --wait 2m") echo 'resume old' >> "$EVENTS"; echo '{"mode":"running"}' ;;
  *) exit 99 ;;
esac
`,
			}
			for name, body := range scripts {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\nset -eu\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			events := filepath.Join(home, "events")
			t.Setenv("HOME", home)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("ORCA_FACTORY_REPO", repo)
			t.Setenv("EVENTS", events)
			t.Setenv("BUILD_FAILS", strconv.FormatBool(tc.buildFails))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "bash", "../../deploy/ops/deploy.sh").CombinedOutput()
			if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != tc.wantExit {
				t.Errorf("exit error = %v, want exit %d; output:\n%s", err, tc.wantExit, output)
			}
			got, err := os.ReadFile(events)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.wantEvents {
				t.Errorf("events:\n%s\nwant:\n%s", got, tc.wantEvents)
			}
			binary, err := os.ReadFile(filepath.Join(bin, "orca-factory-driver"))
			if err != nil {
				t.Fatal(err)
			}
			if want := "#!/bin/bash\nset -eu\n" + scripts["orca-factory-driver"]; string(binary) != want {
				t.Errorf("installed binary was not restored:\n%s", binary)
			}
		})
	}
}
