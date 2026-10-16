package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// pruneTaskServer serves TEST-1 as a managed GitHub task in the selected state;
// an empty state means the repository has no tasks.
func pruneTaskServer(t *testing.T, state *atomic.Value) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("prune wrote %s %s", r.Method, r.URL.Path)
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_, _ = fmt.Fprint(w, "[]")
			return
		}
		rows := []any{}
		if name, _ := state.Load().(string); name != "" {
			native := "open"
			if name == "Done" {
				native = "closed"
			}
			meta := fmt.Sprintf(`{"id":"01234567-1234-1234-1234-012345678901","identifier":"TEST-1","project":"test/repo","state":%q,"complete":true,"blockers":[],"related":[]}`, name)
			rows = append(rows, map[string]any{"number": 1, "state": native, "body": "<!-- orca-task-v1: " + meta + " -->"})
		}
		if err := json.NewEncoder(w).Encode(rows); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

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
	var taskState atomic.Value
	taskState.Store("")
	server := pruneTaskServer(t, &taskState)
	write(filepath.Join(dir, "gh"), "#!/bin/sh\ncase \"$1 $2\" in\n'auth token') printf test ;;\n*) printf '[]\\n' ;;\nesac\n")
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
		c.Env = append(os.Environ(), "AGENT_DRIVER_BIN="+driver, "PATH="+dir+":"+os.Getenv("PATH"), "AGENT_REPO_PATH="+repo, "AGENT_RUN_WORKTREE_PREFIX=run-", "HOME="+dir, "AGENT_DRIVER_STATE_ROOT="+filepath.Join(dir, "state"), "AGENT_ISSUE_BACKEND=github", "AGENT_GH_REPO=test/repo", "AGENT_GH_API_URL="+server.URL, "PRUNE_WORKER="+worker, "PRUNE_CALLS="+filepath.Join(dir, "calls"))
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
	if code != 7 || out != "pruned "+worker+" ok=false\n" {
		t.Fatalf("failed removal: %d %q %q", code, out, warnings)
	}
	if _, e := os.Stat(worker); e != nil {
		t.Fatal("failed removal lost worktree")
	}
	marker := filepath.Join(worker, ".omc/agent-run-finished")
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	git("config", "core.excludesFile", filepath.Join(dir, "exclude"))
	write(filepath.Join(dir, "exclude"), ".omc/\n")
	write(filepath.Join(dir, "orca"), `#!/bin/sh
case "$1 $2" in
'worktree list') jq -nc --arg p "$PRUNE_WORKER" '{ok:true,result:{worktrees:[{path:$p,createdAt:9999999999999}]}}' ;;
'worktree rm') printf '%s\n' '{"ok":false}'; exit 7 ;;
esac
`)
	code, out, warnings = invoke(true)
	if code != 0 || out != "" {
		t.Fatalf("directory is not finished marker: %d %q %q", code, out, warnings)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(dir, "finished"), "")
	if err := os.Symlink(filepath.Join(dir, "finished"), marker); err != nil {
		t.Fatal(err)
	}
	code, out, warnings = invoke(true)
	if code != 7 || out != "pruned "+worker+" ok=false\n" {
		t.Fatalf("symlink to regular finished marker: %d %q %q", code, out, warnings)
	}
	write(filepath.Join(dir, "orca"), `#!/bin/sh
case "$1 $2" in
'worktree list') jq -nc --arg p "$PRUNE_WORKER" '{ok:true,result:{worktrees:[{path:$p,createdAt:9999999999999}]}}' ;;
'worktree rm') case "$*" in *--force*) printf '%s\n' '{"ok":true}'; exit 0 ;; esac; printf '%s\n' '{"ok":false}'; exit 7 ;;
esac
`)
	code, out, warnings = invoke(true)
	if code != 7 {
		t.Fatalf("legacy removal must not force newly dirty checkout: %d %q %q", code, out, warnings)
	}
	taskState.Store("In Progress")
	write(filepath.Join(dir, "orca"), `#!/bin/sh
case "$1 $2" in
'worktree list')
 count="$PRUNE_CALLS.list"; if [ -f "$count" ]; then ticket=TEST-1; else ticket=''; touch "$count"; fi
 jq -nc --arg p "$PRUNE_WORKER" --arg ticket "$ticket" '{ok:true,result:{worktrees:[{path:$p,createdAt:9999999999999,linkedLinearIssue:$ticket}]}}' ;;
'worktree rm') printf '%s\n' '{"ok":false}'; exit 7 ;;
esac
`)
	code, out, warnings = invoke(true)
	if code != 0 || out != "" {
		t.Fatalf("fresh linked association must retain legacy: %d %q %q", code, out, warnings)
	}
	alias := filepath.Join(dir, "main-alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "foreign")
	c := exec.Command("git", "clone", "-q", repo, foreign)
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("clone: %s %v", b, e)
	}
	git("worktree", "lock", worker)
	taskState.Store("Done")
	write(filepath.Join(dir, "gh"), fmt.Sprintf("#!/bin/sh\ncase \"$1 $2\" in\n'auth token') printf test ;;\n*) printf '%%s\\n' '%s' ;;\nesac\n", `[{"merged_at":"2026-01-01","head":{"sha":"`+git("rev-parse", "HEAD")+`","repo":{"full_name":"test/repo"}}}]`))
	write(filepath.Join(dir, "orca"), fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
'worktree list') jq -nc --arg p %q --arg q %q --arg r "$PRUNE_WORKER" '{ok:true,result:{worktrees:[$p,$q,$r]|map({path:.,linkedLinearIssue:"TEST-1",createdAt:9999999999999})}}' ;;
'worktree rm') printf '%%s\n' "$*" >> "$PRUNE_CALLS.protected"; printf '%%s\n' '{"ok":false}'; exit 7 ;;
esac
`, alias, foreign))
	code, out, warnings = invoke(true)
	if code != 0 {
		t.Fatalf("protected linked candidates: %d %q %q", code, out, warnings)
	}
	if _, e := os.Stat(filepath.Join(dir, "calls.protected")); !os.IsNotExist(e) {
		t.Fatal("main alias, foreign repo or locked worker reached deletion")
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
