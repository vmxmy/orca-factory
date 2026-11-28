package intake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/agentcli"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
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

// antigravityRunner wires a Runner whose primary candidate is antigravity and
// whose fallback is claude: the stand-in agy logs each argv under logs and
// prints the recorded agy result, and the stand-in claude only marks claudeRan.
func antigravityRunner(t *testing.T) (runner *Runner, logs, claudeRan string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	git(t, root, "init", "-q", "--bare", "-b", "main", "origin.git")
	repo := filepath.Join(root, "repo")
	git(t, root, "init", "-q", "-b", "main", repo)
	git(t, repo, "remote", "add", "origin", filepath.Join(root, "origin.git"))
	commit(t, repo, "scripts/agent-gates/roles/intake-chat.md")
	recorded, err := filepath.Abs("../../testdata/recorded/agy-print-json-result.json")
	if err != nil {
		t.Fatal(err)
	}
	logs, claudeRan = filepath.Join(root, "calls"), filepath.Join(root, "claude-ran")
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	agy := filepath.Join(root, "agy")
	writeFile(t, agy, "#!/bin/sh\nd=$(mktemp -d "+logs+"/call.XXXXXX)\nprintf '%s\\0' \"$@\" > $d/args\n: > $d/env\npwd | tr -d '\\n' > $d/cwd\n: > $d/stdin\nexec cat "+recorded+"\n", 0o755)
	claude := filepath.Join(root, "claude")
	writeFile(t, claude, "#!/bin/sh\ntouch "+claudeRan+"\nprintf '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"claude\"}'\n", 0o755)
	agentcli.Configure(map[string]agentcli.Profile{"antigravity": {Binary: agy,
		IntakeArgs:       []string{"--print={prompt}", "--model", "{model}", "--effort", "{effort}", "--output-format", "json", "--mode", "plan"},
		IntakeResumeArgs: []string{"--print={prompt}", "--conversation", "{conversation}", "--model", "{model}", "--effort", "{effort}", "--output-format", "json", "--mode", "plan"}}})
	t.Cleanup(func() { agentcli.Configure(nil) })
	runner = &Runner{Agent: "antigravity", Claude: claude, Bwrap: standInBwrap(t), Repo: repo, Checkout: filepath.Join(root, "checkout"),
		Model: "gemini-4-neon-high", Effort: "high", Fallback: []modelhealth.Ref{{Agent: "claude", Model: "claude-opus-5-5", Effort: "high"}},
		StateRoot: filepath.Join(root, "state"), Timeout: 10 * time.Second}
	return runner, logs, claudeRan
}

func TestAntigravityIntakeTurn(t *testing.T) {
	runner, logs, claudeRan := antigravityRunner(t)
	const session, conversation = "33333333-3333-4333-8333-333333333333", "2e3a6cbc-b7e6-4367-a478-6315c666ec4c"
	for i, resume := range []bool{false, true} {
		got, err := runner.Turn(context.Background(), Turn{Session: session, Resume: resume, Prompt: "-p hi"})
		if err != nil || got != "OK" {
			t.Fatalf("turn %d = %q, %v", i, got, err)
		}
		recorded := calls(t, logs)
		if len(recorded) != 1 {
			t.Fatalf("turn %d calls %d", i, len(recorded))
		}
		args := recorded[0].args
		plan := slices.Index(args, "--mode")
		if plan < 0 || plan+1 >= len(args) || args[plan+1] != "plan" {
			t.Fatalf("turn %d argv lacks --mode plan: %q", i, args)
		}
		resumed := slices.Index(args, "--conversation")
		if !resume && resumed >= 0 {
			t.Fatalf("first turn resumes a conversation: %q", args)
		}
		if resume && (resumed < 0 || resumed+1 >= len(args) || args[resumed+1] != conversation) {
			t.Fatalf("second turn argv lacks --conversation %s: %q", conversation, args)
		}
	}
	if _, err := os.Stat(claudeRan); err == nil {
		t.Fatal("claude started for an antigravity session")
	}
}

// A stand-in agy inside the real sandbox reports what it reaches: agy's own
// directory is a private one under StateRoot holding only the sign-in token,
// so the conversations of every other agy session stay hidden.
func TestAntigravityTurnConfinesAgentToSandbox(t *testing.T) {
	if err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-pid", "true").Run(); err != nil {
		if os.Getenv("CI") != "" {
			t.Skipf("bwrap unavailable: %v", err)
		}
		t.Fatalf("bwrap unavailable: %v", err)
	}
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
	writeFile(t, filepath.Join(checkout, "scripts/agent-gates/roles/intake-chat.md"), "role", 0o600)
	agyDir := filepath.Join(home, ".gemini", "antigravity-cli")
	writeFile(t, filepath.Join(agyDir, "antigravity-oauth-token"), "token", 0o600)
	hidden := []string{
		filepath.Join(agyDir, "brain", "other", "plan.md"), filepath.Join(agyDir, "conversations", "other.db"),
		filepath.Join(agyDir, "history.jsonl"), filepath.Join(agyDir, "settings.json"),
		filepath.Join(home, ".gemini", "settings.json"), filepath.Join(home, ".claude", ".credentials.json"),
		filepath.Join(home, ".codex", "auth.json"),
	}
	for _, secret := range hidden {
		writeFile(t, secret, "x", 0o600)
	}
	recorded, err := os.ReadFile("../../testdata/recorded/agy-print-json-result.json")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nseen=\"\"\nfor f in"
	for _, secret := range hidden {
		script += fmt.Sprintf(" %q", secret)
	}
	script += fmt.Sprintf(` /proc/%d/environ; do [ -e "$f" ] && seen="$seen $f"; done
[ -r %q ] || seen="$seen no-token"
touch %q 2> /dev/null || seen="$seen no-state-write"
[ -z "$seen" ] || { printf 'seen:%%s' "$seen" >&2; exit 1; }
printf '%%s' %q
`, os.Getpid(), filepath.Join(agyDir, "antigravity-oauth-token"), filepath.Join(agyDir, "marker"), string(recorded))
	agy := filepath.Join(outside, "agy")
	writeFile(t, agy, script, 0o755)
	agentcli.Configure(map[string]agentcli.Profile{"antigravity": {Binary: agy,
		IntakeArgs: []string{"--print={prompt}"}, IntakeResumeArgs: []string{"--print={prompt}", "--conversation", "{conversation}"}}})
	t.Cleanup(func() { agentcli.Configure(nil) })
	state := filepath.Join(outside, "state")
	runner := &Runner{Agent: "antigravity", Repo: outside, Checkout: checkout, StateRoot: state, Timeout: 30 * time.Second}
	if _, err := runner.antigravity(context.Background(), Turn{Session: "s", Prompt: "p"}, modelhealth.Ref{Agent: "antigravity"}, ""); err != nil {
		var exit *exec.ExitError
		errors.As(err, &exit)
		t.Fatalf("agy reached hidden paths or lost its state: %v: %s", err, exit.Stderr)
	}
	if _, err := os.Stat(filepath.Join(state, "intake-antigravity", "marker")); err != nil {
		t.Fatalf("agy state not kept under StateRoot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agyDir, "marker")); err == nil {
		t.Fatal("agy wrote into the shared antigravity-cli directory")
	}
}
