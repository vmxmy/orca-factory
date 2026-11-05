package intake

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePiScript speaks the pi --mode rpc JSONL protocol (LF-framed records,
// command name in "type", responses correlated by "id") without any model:
// prompts are echoed back, session lifecycle is logged for assertions.
const fakePiScript = `#!/usr/bin/env python3
import json, os, sys

log_path = os.environ.get("ANTHROPIC_FAKE_PI_LOG", "")
mode = os.environ.get("ANTHROPIC_FAKE_PI_MODE", "ok")
last_message = ""

def log(event):
    if log_path:
        with open(log_path, "a") as f:
            f.write(json.dumps(event) + "\n")

def respond(typ, rid, data=None):
    rec = {"type": "response", "command": typ, "success": True}
    if rid:
        rec["id"] = rid
    if data is not None:
        rec["data"] = data
    print(json.dumps(rec), flush=True)

log({"event": "spawn", "argv": sys.argv[1:]})

if mode == "crash":
    sys.exit(1)

for raw in sys.stdin:
    line = raw.rstrip("\n")
    if not line:
        continue
    try:
        cmd = json.loads(line)
    except Exception:
        continue
    typ = cmd.get("type")
    rid = cmd.get("id")
    if typ == "prompt":
        last_message = cmd.get("message", "")
        log({"event": "prompt", "message": last_message})
        if mode in ("hang", "abort-stuck"):
            continue
        if mode == "die-on-prompt":
            sys.exit(1)
        respond(typ, rid, {"disposition": "started"})
        print(json.dumps({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "x"}}), flush=True)
        print(json.dumps({"type": "agent_settled"}), flush=True)
    elif typ == "get_last_assistant_text":
        respond(typ, rid, {"text": "echo:" + last_message})
    elif typ == "abort":
        log({"event": "abort"})
        if mode != "abort-stuck":
            respond(typ, rid)
    elif typ in ("set_model", "set_thinking_level", "get_state", "get_session_stats"):
        respond(typ, rid)
    else:
        rec = {"type": "response", "command": typ, "success": False, "error": "unknown command"}
        if rid:
            rec["id"] = rid
        print(json.dumps(rec), flush=True)
`

type fakePiLog struct {
	Event   string   `json:"event"`
	Argv    []string `json:"argv"`
	Message string   `json:"message"`
}

func piTestRunner(t *testing.T, mode string) (*Runner, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	checkout := filepath.Join(root, "checkout")
	writeFile(t, filepath.Join(checkout, "tracked"), "code", 0o600)
	logPath := filepath.Join(root, "pi.log")
	t.Setenv("ANTHROPIC_FAKE_PI_LOG", logPath)
	t.Setenv("ANTHROPIC_FAKE_PI_MODE", mode)
	fake := filepath.Join(root, "pi")
	writeFile(t, fake, fakePiScript, 0o755)
	runner := &Runner{
		PiBinary:     fake,
		PiProvider:   "anthropic",
		PiModel:      "claude-sonnet-5-5",
		PiThinking:   "high",
		PiSessionDir: filepath.Join(root, "pi-sessions"),
		Repo:         root,
		Checkout:     checkout,
		Bwrap:        standInBwrap(t),
		Timeout:      5 * time.Second,
	}
	return runner, logPath
}

// startPiRunner starts the janitor after the caller has finished configuring
// the runner; idle timeouts must be set before Start, not while the janitor
// goroutine reads them.
func startPiRunner(t *testing.T, runner *Runner) {
	t.Helper()
	runner.Start()
	t.Cleanup(runner.Stop)
}

func readFakePiLog(t *testing.T, path string) []fakePiLog {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []fakePiLog
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var e fakePiLog
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("fake pi log line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func countSpawns(events []fakePiLog) int {
	n := 0
	for _, e := range events {
		if e.Event == "spawn" {
			n++
		}
	}
	return n
}

func TestPiTurnMainPath(t *testing.T) {
	runner, logPath := piTestRunner(t, "ok")
	startPiRunner(t, runner)
	got, err := runner.Turn(context.Background(), Turn{Session: "11111111-2222-4333-8444-555555555555", Resume: true, Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo:hello" {
		t.Fatalf("reply=%q, want echoed prompt", got)
	}
	events := readFakePiLog(t, logPath)
	if n := countSpawns(events); n != 1 {
		t.Fatalf("spawns=%d, want 1: %v", n, events)
	}
	var argv []string
	for _, e := range events {
		if e.Event == "spawn" {
			argv = e.Argv
		}
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--mode rpc", "--session-id 11111111-2222-4333-8444-555555555555", "-t read,grep,glob"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("pi argv %q lacks %q", joined, want)
		}
	}
}

// A second turn on the same session must reuse the resident process instead
// of paying a fresh spawn and full session replay (OF-416's core win).
func TestPiTurnReusesProcessAcrossTurns(t *testing.T) {
	runner, logPath := piTestRunner(t, "ok")
	startPiRunner(t, runner)
	session := "11111111-2222-4333-8444-555555555555"
	if _, err := runner.Turn(context.Background(), Turn{Session: session, Resume: true, Prompt: "first"}); err != nil {
		t.Fatal(err)
	}
	got, err := runner.Turn(context.Background(), Turn{Session: session, Resume: true, Prompt: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo:second" {
		t.Fatalf("reply=%q, want echo:second", got)
	}
	if n := countSpawns(readFakePiLog(t, logPath)); n != 1 {
		t.Fatalf("spawns=%d, want 1 (process must be reused)", n)
	}
}

// A timed-out prompt must be aborted; an abort pi acks keeps the process for
// the next turn.
func TestPiTurnTimeoutAborts(t *testing.T) {
	runner, logPath := piTestRunner(t, "hang")
	startPiRunner(t, runner)
	runner.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := runner.Turn(context.Background(), Turn{Session: "s-timeout", Resume: true, Prompt: "hang"})
	if err == nil {
		t.Fatal("turn must fail when the agent never settles")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("timeout handling took %s; abort grace must bound it", time.Since(start))
	}
	events := readFakePiLog(t, logPath)
	sawAbort := false
	for _, e := range events {
		if e.Event == "abort" {
			sawAbort = true
		}
	}
	if !sawAbort {
		t.Fatalf("abort never reached pi: %v", events)
	}
	if n := countSpawns(events); n != 1 {
		t.Fatalf("spawns=%d, want 1 (aborted process stays resident)", n)
	}
}

// When pi never acks the abort, the turn must kill the process group and drop
// it, so the next turn spawns a fresh process.
func TestPiTurnTimeoutKillsUnresponsiveProc(t *testing.T) {
	runner, logPath := piTestRunner(t, "abort-stuck")
	startPiRunner(t, runner)
	runner.Timeout = 300 * time.Millisecond
	if _, err := runner.Turn(context.Background(), Turn{Session: "s-kill", Resume: true, Prompt: "hang"}); err == nil {
		t.Fatal("turn must fail when the agent never settles")
	}
	t.Setenv("ANTHROPIC_FAKE_PI_MODE", "ok")
	if got, err := runner.Turn(context.Background(), Turn{Session: "s-kill", Resume: true, Prompt: "back"}); err != nil || got != "echo:back" {
		t.Fatalf("respawn turn = %q, %v", got, err)
	}
	if n := countSpawns(readFakePiLog(t, logPath)); n != 2 {
		t.Fatalf("spawns=%d, want 2 (unresponsive proc must be replaced)", n)
	}
}

// A process that dies mid-turn is an error; the next turn respawns instead of
// retrying the whole conversation against a corpse.
func TestPiProcCrashMidTurnRespawns(t *testing.T) {
	runner, logPath := piTestRunner(t, "die-on-prompt")
	startPiRunner(t, runner)
	if _, err := runner.Turn(context.Background(), Turn{Session: "s-crash", Resume: true, Prompt: "boom"}); err == nil {
		t.Fatal("turn must fail when pi exits mid-turn")
	}
	t.Setenv("ANTHROPIC_FAKE_PI_MODE", "ok")
	if got, err := runner.Turn(context.Background(), Turn{Session: "s-crash", Resume: true, Prompt: "again"}); err != nil || got != "echo:again" {
		t.Fatalf("respawn turn = %q, %v", got, err)
	}
	if n := countSpawns(readFakePiLog(t, logPath)); n != 2 {
		t.Fatalf("spawns=%d, want 2", n)
	}
}

// Sessions live on disk under --session-dir: a Runner rebuilt from the same
// state (gateway restart) must hand the same --session-id to a fresh process,
// so pi reloads the conversation instead of starting blank.
func TestPiSessionSurvivesRunnerRebuild(t *testing.T) {
	runner1, logPath := piTestRunner(t, "ok")
	startPiRunner(t, runner1)
	session := "11111111-2222-4333-8444-555555555555"
	if _, err := runner1.Turn(context.Background(), Turn{Session: session, Resume: true, Prompt: "one"}); err != nil {
		t.Fatal(err)
	}
	sessionDir := runner1.PiSessionDir
	runner1.Stop()
	runner2 := &Runner{
		PiBinary:     runner1.PiBinary,
		PiProvider:   "anthropic",
		PiModel:      "claude-sonnet-5-5",
		PiThinking:   "high",
		PiSessionDir: sessionDir,
		Repo:         runner1.Repo,
		Checkout:     runner1.Checkout,
		Bwrap:        runner1.Bwrap,
		Timeout:      5 * time.Second,
	}
	startPiRunner(t, runner2)
	if _, err := runner2.Turn(context.Background(), Turn{Session: session, Resume: true, Prompt: "two"}); err != nil {
		t.Fatal(err)
	}
	events := readFakePiLog(t, logPath)
	if n := countSpawns(events); n != 2 {
		t.Fatalf("spawns=%d, want 2 (rebuilt runner respawns)", n)
	}
	for _, e := range events {
		if e.Event == "spawn" && !strings.Contains(strings.Join(e.Argv, " "), "--session-id "+session) {
			t.Fatalf("respawn lost session identity: %v", e.Argv)
		}
	}
}

// pi 的 npm 包是多文件 bundle：沙箱必须绑定包根目录（含 package.json 的
// 目录），而不是 EvalSymlinks 后的入口单文件，否则入口文件读不到同目录的
// 运行时模块（真实缺陷：Cannot find module './cli-runtime.js'）。
func TestPiSandboxBindsPackageRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	pkg := filepath.Join(root, "npmroot", "lib", "node_modules", "@earendil-works", "pi-coding-agent")
	writeFile(t, filepath.Join(pkg, "package.json"), `{"name":"pi-coding-agent","version":"0.0.0"}`, 0o644)
	writeFile(t, filepath.Join(pkg, "dist", "bundle", "sibling.txt"), "bundle-code", 0o644)
	const packagedFake = `#!/usr/bin/env python3
import json, os, sys
here = os.path.dirname(os.path.realpath(__file__))
try:
    token = open(os.path.join(here, "sibling.txt")).read().strip()
except OSError:
    token = "no-sibling"
last = ""
for raw in sys.stdin:
    line = raw.rstrip("\n")
    if not line:
        continue
    cmd = json.loads(line)
    typ, rid = cmd.get("type"), cmd.get("id")
    if typ == "prompt":
        last = cmd.get("message", "")
        print(json.dumps({"id": rid, "type": "response", "command": "prompt", "success": True, "data": {"disposition": "started"}}), flush=True)
        print(json.dumps({"type": "agent_settled"}), flush=True)
    elif typ == "get_last_assistant_text":
        print(json.dumps({"id": rid, "type": "response", "command": typ, "success": True, "data": {"text": "pkg:" + token + ":" + last}}), flush=True)
    else:
        print(json.dumps({"id": rid, "type": "response", "command": typ, "success": True}), flush=True)
`
	writeFile(t, filepath.Join(pkg, "dist", "bundle", "cli.js"), packagedFake, 0o755)
	bin := filepath.Join(root, "npmroot", "bin", "pi")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(pkg, "dist", "bundle", "cli.js"), bin); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(root, "checkout")
	writeFile(t, filepath.Join(checkout, "tracked"), "code", 0o600)
	runner := &Runner{
		PiBinary:     bin, // npm shim：经 EvalSymlinks 解析到包内入口文件
		PiSessionDir: filepath.Join(root, "pi-sessions"),
		Repo:         root,
		Checkout:     checkout,
		Bwrap:        standInBwrap(t),
		Timeout:      5 * time.Second,
	}
	got, err := runner.Turn(context.Background(), Turn{Session: "s-pkg", Resume: true, Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "pkg:bundle-code:hi" {
		t.Fatalf("reply=%q, want pkg:bundle-code:hi (sibling module must be visible)", got)
	}
}

// A Runner without pi configuration keeps the claude -p contract untouched.
func TestPiTierParsing(t *testing.T) {
	provider, model, thinking, err := ParsePiTier("pi:anthropic/claude-sonnet-5-5:high")
	if err != nil || provider != "anthropic" || model != "claude-sonnet-5-5" || thinking != "high" {
		t.Fatalf("tier=(%q,%q,%q), %v", provider, model, thinking, err)
	}
	if _, _, _, err := ParsePiTier("claude:claude-opus-5-5:high"); err == nil {
		t.Fatal("non-pi tier must be rejected")
	}
	if _, _, _, err := ParsePiTier("pi:claude-sonnet-5-5:high"); err == nil {
		t.Fatal("tier without provider/model split must be rejected")
	}
}

// The session dir may live inside the checkout; the sandbox must bind the
// ancestor first, or the checkout's read-only bind shadows the session dir
// and pi cannot persist its session (real defect: EROFS on session write).
func TestPiSandboxNestedSessionDirStaysWritable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	checkout := filepath.Join(root, "checkout")
	writeFile(t, filepath.Join(checkout, "tracked"), "code", 0o600)
	fake := filepath.Join(root, "pi")
	const sessionWritingFake = `#!/usr/bin/env python3
import json, os, sys
session_dir = sys.argv[sys.argv.index("--session-dir") + 1]
last = ""
for raw in sys.stdin:
    line = raw.rstrip("\n")
    if not line:
        continue
    cmd = json.loads(line)
    typ, rid = cmd.get("type"), cmd.get("id")
    if typ == "prompt":
        last = cmd.get("message", "")
        with open(os.path.join(session_dir, "session.jsonl"), "a") as f:
            f.write(json.dumps({"kind": "user", "text": last}) + "\n")
        print(json.dumps({"id": rid, "type": "response", "command": "prompt", "success": True, "data": {"disposition": "started"}}), flush=True)
        print(json.dumps({"type": "agent_settled"}), flush=True)
    elif typ == "get_last_assistant_text":
        print(json.dumps({"id": rid, "type": "response", "command": typ, "success": True, "data": {"text": "wrote:" + last}}), flush=True)
    else:
        print(json.dumps({"id": rid, "type": "response", "command": typ, "success": True}), flush=True)
`
	writeFile(t, fake, sessionWritingFake, 0o755)
	runner := &Runner{
		PiBinary:     fake,
		PiSessionDir: filepath.Join(checkout, "sessions"), // nested inside the ro checkout
		Repo:         root,
		Checkout:     checkout,
		Bwrap:        standInBwrap(t),
		Timeout:      5 * time.Second,
	}
	if _, err := runner.Turn(context.Background(), Turn{Session: "s-nested", Resume: true, Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(checkout, "sessions", "session.jsonl"))
	if err != nil {
		t.Fatalf("session file not written through nested bind: %v", err)
	}
	if !strings.Contains(string(b), "hi") {
		t.Fatalf("session file lacks prompt: %q", b)
	}
}

// The janitor reclaims processes idle past the configured deadline, so a
// gateway holding many quiet Feishu threads does not leak pi processes.
func TestPiJanitorReclaimsIdleProcs(t *testing.T) {
	runner, logPath := piTestRunner(t, "ok")
	runner.PiIdleTimeout = 150 * time.Millisecond
	startPiRunner(t, runner)
	session := "s-idle"
	if _, err := runner.Turn(context.Background(), Turn{Session: session, Resume: true, Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond) // well past the idle deadline and a janitor tick
	if _, err := runner.Turn(context.Background(), Turn{Session: session, Resume: true, Prompt: "again"}); err != nil {
		t.Fatal(err)
	}
	events := readFakePiLog(t, logPath)
	if n := countSpawns(events); n != 2 {
		t.Fatalf("spawns=%d, want 2 after idle reclamation: %v", n, events)
	}
}
