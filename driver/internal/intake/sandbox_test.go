package intake

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// standInBwrap runs the wrapped command as is, for tests whose stand-in claude
// logs outside the paths the real sandbox exposes.
func standInBwrap(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bwrap")
	script := "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec env -u PWD -u OLDPWD \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// A stand-in claude reports everything it can reach from inside the real
// sandbox: an empty report means only the checkout and Claude's own directory
// were exposed.
func TestRunnerTurnConfinesAgentToSandbox(t *testing.T) {
	if err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-pid", "true").Run(); err != nil {
		// Hosted CI may forbid user namespaces; a machine running the gateway must not.
		if os.Getenv("CI") != "" {
			t.Skipf("bwrap unavailable: %v", err)
		}
		t.Fatalf("bwrap unavailable: %v", err)
	}
	// Outside /tmp, which the sandbox empties whatever else it exposes.
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", cache)
	home, outside := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	checkout := filepath.Join(home, ".cache", "intake", "checkout")
	writeFile(t, filepath.Join(checkout, "tracked"), "code", 0o600)
	env := filepath.Join(home, ".config", "orca-factory", "driver.env")
	sibling := filepath.Join(home, "github", "other", "secret")
	loose := filepath.Join(outside, "secret")
	for _, secret := range []string{env, sibling, loose, filepath.Join(home, ".claude", ".credentials.json"), filepath.Join(home, ".claude.json")} {
		writeFile(t, secret, "x", 0o600)
	}
	script := fmt.Sprintf(`#!/bin/sh
cat > /dev/null
seen=""
for f in %q %q %q /proc/%d/environ; do [ -r "$f" ] && seen="$seen $f"; done
touch tracked-write 2> /dev/null && seen="$seen checkout-write"
touch /root-write 2> /dev/null && seen="$seen root-write"
[ -r tracked ] || seen="$seen no-checkout"
[ -r "$HOME/.claude/.credentials.json" ] || seen="$seen no-credentials"
touch "$HOME/.claude/session" 2> /dev/null || seen="$seen no-session-write"
printf '{"type":"result","subtype":"success","is_error":false,"result":"seen:%%s"}' "$seen"
`, env, sibling, loose, os.Getpid())
	claude := filepath.Join(outside, "claude")
	writeFile(t, claude, script, 0o755)
	link := filepath.Join(outside, "claude-link")
	if err := os.Symlink(claude, link); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Claude: link, Repo: outside, Checkout: checkout, Timeout: 30 * time.Second}
	got, err := runner.Turn(context.Background(), Turn{Session: "s", Resume: true, Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "seen:" {
		t.Fatalf("agent reached %q, want nothing", got)
	}
}

func TestRunnerTurnRefusesWithoutSandbox(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	ran := filepath.Join(root, "ran")
	claude := filepath.Join(root, "claude")
	writeFile(t, claude, "#!/bin/sh\ntouch "+ran+"\nprintf '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\"}'\n", 0o755)
	runner := &Runner{Claude: claude, Repo: root, Checkout: root, Bwrap: filepath.Join(root, "no-bwrap"), Timeout: 10 * time.Second}
	if got, err := runner.Turn(context.Background(), Turn{Session: "s", Resume: true, Prompt: "p"}); err == nil {
		t.Fatalf("turn ran without a sandbox and returned %q", got)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("claude started without a sandbox")
	}
}
