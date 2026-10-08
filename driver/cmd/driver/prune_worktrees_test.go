package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPruneWorktreesPublicEntry(t *testing.T) {
	factory, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	driver := filepath.Join(dir, "driver")
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(driver, fmt.Sprintf("#!/bin/sh\nDRIVER_TEST_PROCESS=1 exec %q -test.run=^TestCLIProcess$ -- \"$@\"\n", os.Args[0]))
	repo := filepath.Join(dir, "repo")
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %s %v", args, b, e)
		}
		return strings.TrimSpace(string(b))
	}
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("config", "user.name", "test")
	git("config", "user.email", "test@example.test")
	git("commit", "--allow-empty", "-qm", "seed")
	worker := filepath.Join(repo, "run-中文 space")
	git("worktree", "add", "-qb", "run", worker)
	write(filepath.Join(worker, "dirty"), "unsaved")
	write(filepath.Join(dir, "orca"), `#!/bin/sh
case "$1 $2" in
'worktree list') jq -nc --arg p "$PRUNE_WORKER" '{ok:true,result:{worktrees:[{path:$p,createdAt:1}]}}' ;;
'linear list-issues') printf '%s\n' '{"ok":true,"result":{"issues":[]}}' ;;
'worktree rm') printf '%s\n' "$*" >> "$PRUNE_CALLS"; printf '%s\n' '{"ok":false}'; exit 7 ;;
esac
`)
	entry := filepath.Join(factory, "core/prune-run-worktrees.sh")
	cancelProcess := false
	invoke := func(pipe bool, args ...string) (int, string, string) {
		t.Helper()
		c := exec.Command("bash", append([]string{entry}, args...)...)
		if pipe {
			c = exec.Command("bash")
			body, e := os.ReadFile(entry)
			if e != nil {
				t.Fatal(e)
			}
			c.Stdin = bytes.NewReader(body)
		}
		c.Dir = dir
		c.Env = append(os.Environ(), "AGENT_DRIVER_BIN="+driver, "PATH="+dir+":"+os.Getenv("PATH"), "AGENT_REPO_PATH="+repo, "AGENT_RUN_WORKTREE_PREFIX=run-", "AGENT_LINEAR_TEAM=TEST", "AGENT_LINEAR_PROJECT=test", "AGENT_GH_REPO=", "AGENT_SNAPSHOT_FILE="+filepath.Join(dir, "none"), "AGENT_LINEAR_COUNT_FILE="+filepath.Join(dir, "count"), "PRUNE_WORKER="+worker, "PRUNE_CALLS="+filepath.Join(dir, "calls"))
		var out, warnings bytes.Buffer
		c.Stdout = &out
		c.Stderr = &warnings
		code := 0
		run := c.Run
		if cancelProcess {
			run = func() error {
				if err := c.Start(); err != nil {
					return err
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						_ = c.Process.Kill()
						_ = c.Wait()
						t.Fatal("external process did not start")
					}
					time.Sleep(time.Millisecond)
				}
				if err := c.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				return c.Wait()
			}
		}
		if e := run(); e != nil {
			exit, ok := e.(*exec.ExitError)
			if !ok {
				t.Fatal(e)
			}
			code = exit.ExitCode()
		}
		return code, out.String(), warnings.String()
	}
	code, out, warnings := invoke(false, "--contract-version")
	if code != 0 || out != "prune-worktrees-v1\n" || warnings != "" {
		t.Fatalf("probe: %d %q %q", code, out, warnings)
	}
	code, out, warnings = invoke(true)
	if code != 0 || out != "" || !strings.Contains(warnings, "worktree is dirty") {
		t.Fatalf("dirty pipeline: %d %q %q", code, out, warnings)
	}
	if _, e := os.Stat(filepath.Join(dir, "calls")); !os.IsNotExist(e) {
		t.Fatal("dirty legacy worktree reached deletion")
	}
	if e := os.Remove(filepath.Join(worker, "dirty")); e != nil {
		t.Fatal(e)
	}
	code, out, warnings = invoke(true)
	if code != 7 || out != "pruned "+worker+" ok=false\n" || !strings.Contains(warnings, "prune_linear_calls=1") {
		t.Fatalf("failed removal: %d %q %q", code, out, warnings)
	}
	if _, e := os.Stat(worker); e != nil {
		t.Fatal("failed removal lost worktree")
	}
	write(filepath.Join(dir, "orca"), "#!/bin/sh\nprintf '%s\\n' \"$$\" > \""+filepath.Join(dir, "ready")+"\"\nexec sleep 30\n")
	cancelProcess = true
	code, _, _ = invoke(true)
	pidText, err := os.ReadFile(filepath.Join(dir, "ready"))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(string(pidText), &pid); err != nil {
		t.Fatal(err)
	}
	if code == 0 || syscall.Kill(pid, 0) == nil {
		t.Fatal("cancellation left an external process running")
	}
}
