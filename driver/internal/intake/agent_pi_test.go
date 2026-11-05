package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePiScript speaks the pi --mode rpc JSONL protocol (LF-framed records,
// command name in "type", responses correlated by "id") without any model:
// prompts are echoed back, session history persists under --session-dir so a
// respawned process must restore it, and session lifecycle is logged for
// assertions.
const fakePiScript = `#!/usr/bin/env python3
import json, os, sys

log_path = os.environ.get("ANTHROPIC_FAKE_PI_LOG", "")
mode = os.environ.get("ANTHROPIC_FAKE_PI_MODE", "ok")
last_message = ""
session_dir = ""
if "--session-dir" in sys.argv:
    session_dir = sys.argv[sys.argv.index("--session-dir") + 1]
history = []
history_restored = False
if session_dir:
    try:
        with open(os.path.join(session_dir, "history.jsonl")) as f:
            history = [json.loads(line).get("message", "") for line in f if line.strip()]
        history_restored = bool(history)
    except OSError:
        pass

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
if mode == "never-read":
    import time
    while True:
        time.sleep(3600)

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
        history.append(last_message)
        if session_dir:
            with open(os.path.join(session_dir, "history.jsonl"), "a") as f:
                f.write(json.dumps({"message": last_message}) + "\n")
        log({"event": "prompt", "message": last_message})
        if mode in ("hang", "abort-stuck", "abort-no-settled", "late-settled"):
            continue
        if mode == "die-on-prompt":
            sys.exit(1)
        respond(typ, rid, {"disposition": "started"})
        print(json.dumps({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "x"}}), flush=True)
        print(json.dumps({"type": "agent_settled"}), flush=True)
    elif typ == "get_last_assistant_text":
        text = "recalled:" + history[0] if history_restored else "echo:" + last_message
        respond(typ, rid, {"text": text})
    elif typ == "abort":
        log({"event": "abort"})
        if mode == "abort-stuck":
            continue
        respond(typ, rid)
        if mode == "abort-no-settled":
            continue
        if mode == "late-settled":
            print(json.dumps({"type": "agent_settled", "aborted": True}), flush=True)
            mode = "ok"
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
	if got, err := runner.Turn(context.Background(), Turn{Session: "s-kill", Resume: true, Prompt: "back"}); err != nil || got != "recalled:hang" {
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
	if got, err := runner.Turn(context.Background(), Turn{Session: "s-crash", Resume: true, Prompt: "again"}); err != nil || got != "recalled:boom" {
		t.Fatalf("respawn turn = %q, %v", got, err)
	}
	if n := countSpawns(readFakePiLog(t, logPath)); n != 2 {
		t.Fatalf("spawns=%d, want 2", n)
	}
}

// Sessions live on disk under --session-dir: a Runner rebuilt from the same
// state (gateway restart) must hand the same --session-id to a fresh process,
// and that process must restore the conversation from the session dir —
// without --session-dir the second process would answer from an empty history.
func TestPiSessionSurvivesRunnerRebuild(t *testing.T) {
	runner1, logPath := piTestRunner(t, "ok")
	startPiRunner(t, runner1)
	session := "11111111-2222-4333-8444-555555555555"
	reply1, err := runner1.Turn(context.Background(), Turn{Session: session, Resume: true, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if reply1 != "echo:one" {
		t.Fatalf("reply1=%q, want echo:one", reply1)
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
	reply2, err := runner2.Turn(context.Background(), Turn{Session: session, Resume: true, Prompt: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if reply2 != "recalled:one" {
		t.Fatalf("reply2=%q, want recalled:one (respawned process must restore session history)", reply2)
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

// TestPiRecordedTurnReplay replays a real recorded pi RPC exchange (prompt
// response, session events, agent_settled, get_last_assistant_text) through
// the protocol parsing layer and asserts the turn's key facts parse.
func TestPiRecordedTurnReplay(t *testing.T) {
	b, err := os.ReadFile("../../testdata/recorded/pi-rpc-turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	p := &piProc{
		events:  make(chan json.RawMessage, 256),
		done:    make(chan struct{}),
		pending: map[string]chan piResponse{},
	}
	promptCh, textCh := make(chan piResponse, 1), make(chan piResponse, 1)
	p.pendingMu.Lock()
	p.pending["1"] = promptCh
	p.pending["2"] = textCh
	p.pendingMu.Unlock()
	go p.readLoop(io.NopCloser(bytes.NewReader(b)))
	var promptResp piResponse
	select {
	case promptResp = <-promptCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no prompt response in recording")
	}
	var outcome struct {
		Disposition string `json:"disposition"`
	}
	if err := json.Unmarshal(promptResp.Data, &outcome); err != nil || !promptResp.Success || outcome.Disposition != "started" {
		t.Fatalf("prompt response %+v, disposition=%q, %v; want success/started", promptResp, outcome.Disposition, err)
	}
	settled := false
	timeout := time.After(5 * time.Second)
	for !settled {
		select {
		case rec := <-p.events:
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(rec, &head); err == nil && head.Type == "agent_settled" {
				settled = true
			}
		case <-timeout:
			t.Fatal("no agent_settled in recording")
		}
	}
	var textResp piResponse
	select {
	case textResp = <-textCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no get_last_assistant_text response in recording")
	}
	text, err := textResp.assistantText()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "收到") {
		t.Fatalf("assistant text %q lacks 收到", text)
	}
}

// bindPairIndex returns the index of flag in args whose value equals path,
// or -1. piSandboxArgs is asserted directly here because the behavior tests
// run through standInBwrap, which never performs the real mounts.
func bindPairIndex(args []string, flag, path string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == path {
			return i
		}
	}
	return -1
}

// The sandbox must bind the resolved pi package root (a multi-file npm
// bundle), never just the entry file, or pi dies looking for its siblings.
func TestPiSandboxArgsBindsPackageRootNotEntry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	pkg := filepath.Join(root, "npmroot", "lib", "node_modules", "pi-coding-agent")
	entry := filepath.Join(pkg, "dist", "bundle", "cli.js")
	writeFile(t, filepath.Join(pkg, "package.json"), "{}", 0o644)
	writeFile(t, entry, "#!/bin/sh\n", 0o755)
	r := &Runner{PiSessionDir: filepath.Join(root, "sessions"), Checkout: filepath.Join(root, "checkout")}
	args, err := r.piSandboxArgs(entry, "s-1")
	if err != nil {
		t.Fatal(err)
	}
	if i := bindPairIndex(args, "--ro-bind", pkg); i < 0 {
		t.Fatalf("package root %q not bound: %v", pkg, args)
	}
	if i := bindPairIndex(args, "--ro-bind", entry); i >= 0 {
		t.Fatalf("entry file %q bound alone, shadowing the package root: %v", entry, args)
	}
	if i := bindPairIndex(args, "--bind", r.PiSessionDir); i < 0 {
		t.Fatalf("session dir %q not bound read-write: %v", r.PiSessionDir, args)
	}
}

// A session dir nested inside the checkout must be bound after its ancestor,
// or the checkout's read-only bind shadows it and pi cannot persist sessions.
func TestPiSandboxArgsAncestorBoundBeforeNested(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	checkout := filepath.Join(root, "checkout")
	writeFile(t, filepath.Join(checkout, "tracked"), "code", 0o600)
	r := &Runner{PiSessionDir: filepath.Join(checkout, "sessions"), Checkout: checkout}
	args, err := r.piSandboxArgs(filepath.Join(root, "pi"), "s-1")
	if err != nil {
		t.Fatal(err)
	}
	checkoutAt, sessionAt := bindPairIndex(args, "--ro-bind", checkout), bindPairIndex(args, "--bind", r.PiSessionDir)
	if checkoutAt < 0 || sessionAt < 0 {
		t.Fatalf("checkout or session bind missing: %v", args)
	}
	if checkoutAt > sessionAt {
		t.Fatalf("checkout ro-bind (%d) must precede nested session bind (%d): %v", checkoutAt, sessionAt, args)
	}
}

// The roles prompt and the read-only tool allowlist ride the pi argv, and
// non-secret config files land at pi's agent config dir under HOME.
func TestPiSandboxArgsCarriesRolePromptAndConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	checkout := filepath.Join(root, "checkout")
	writeFile(t, filepath.Join(checkout, "tracked"), "code", 0o600)
	cfg := filepath.Join(root, "pi-agent-config")
	writeFile(t, filepath.Join(cfg, "models.json"), "{}", 0o644)
	writeFile(t, filepath.Join(cfg, "settings.json"), "{}", 0o644)
	r := &Runner{PiSessionDir: filepath.Join(root, "sessions"), Checkout: checkout, PiConfigDir: cfg}
	args, err := r.piSandboxArgs(filepath.Join(root, "pi"), "s-1")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--append-system-prompt "+filepath.Join(checkout, "scripts/agent-gates/roles/intake-chat.md")) {
		t.Fatalf("roles system prompt missing: %v", args)
	}
	if !strings.Contains(joined, "-t read,grep,glob") {
		t.Fatalf("read-only tool allowlist missing: %v", args)
	}
	for _, name := range []string{"models.json", "settings.json"} {
		target := filepath.Join(root, ".pi", "agent", name)
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--ro-bind-try" && args[i+1] == filepath.Join(cfg, name) && i+2 < len(args) && args[i+2] == target {
				found = true
			}
		}
		if !found {
			t.Fatalf("config %s not bound to %s: %v", name, target, args)
		}
	}
}

// A pi that stops reading stdin must not wedge the turn: the prompt write
// blocks, the turn deadline must still fire, and the process must end up
// cleaned (killed and dropped) instead of holding the gateway slot forever.
func TestPiTurnWriteBlockedByPeerTimesOut(t *testing.T) {
	runner, logPath := piTestRunner(t, "never-read")
	startPiRunner(t, runner)
	runner.Timeout = 300 * time.Millisecond
	big := strings.Repeat("x", 128<<10)
	done := make(chan error, 1)
	go func() {
		_, err := runner.Turn(context.Background(), Turn{Session: "s-blocked", Resume: true, Prompt: big})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("turn must fail when pi never reads stdin")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("turn hung past its deadline on a blocked stdin write")
	}
	if n := countSpawns(readFakePiLog(t, logPath)); n != 1 {
		t.Fatalf("spawns=%d, want 1", n)
	}
}

// When an abort acks but the aborted run's agent_settled never arrives, the
// process is in a state the next turn cannot trust: cleanup must retire it.
func TestPiTurnDropsProcWhenSettledNeverArrives(t *testing.T) {
	runner, logPath := piTestRunner(t, "abort-no-settled")
	startPiRunner(t, runner)
	runner.Timeout = 300 * time.Millisecond
	if _, err := runner.Turn(context.Background(), Turn{Session: "s-noset", Resume: true, Prompt: "hang"}); err == nil {
		t.Fatal("turn must fail when the agent never settles")
	}
	t.Setenv("ANTHROPIC_FAKE_PI_MODE", "ok")
	if got, err := runner.Turn(context.Background(), Turn{Session: "s-noset", Resume: true, Prompt: "back"}); err != nil || got != "recalled:hang" {
		t.Fatalf("respawn turn = %q, %v", got, err)
	}
	if n := countSpawns(readFakePiLog(t, logPath)); n != 2 {
		t.Fatalf("spawns=%d, want 2 (proc without settled must be replaced)", n)
	}
}

// An abort ack that races ahead of the aborted run's agent_settled must not
// leave that stale settled for the next turn: cleanup waits for it (bounded)
// and confirms it, keeping the process.
func TestPiCleanupConfirmsLateSettled(t *testing.T) {
	p := &piProc{events: make(chan json.RawMessage, 16), done: make(chan struct{})}
	go func() {
		time.Sleep(150 * time.Millisecond)
		p.events <- json.RawMessage(`{"type":"agent_settled","aborted":true}`)
	}()
	if !p.waitSettledAfterAbort() {
		t.Fatal("settled within the window must be confirmed")
	}
	select {
	case rec := <-p.events:
		t.Fatalf("stale event left for the next turn: %s", rec)
	default:
	}
}

// No settled within the window: the process cannot be trusted to serve the
// next turn.
func TestPiCleanupRejectsMissingSettled(t *testing.T) {
	p := &piProc{events: make(chan json.RawMessage, 16), done: make(chan struct{})}
	if p.waitSettledAfterAbort() {
		t.Fatal("missing settled must not confirm")
	}
}

// A late-settled abort must not corrupt the following turn: the stale settled
// is consumed by cleanup, the process stays resident, and the next turn gets
// a fresh reply over the same process.
func TestPiTurnAfterLateSettledAbortStaysClean(t *testing.T) {
	runner, logPath := piTestRunner(t, "late-settled")
	startPiRunner(t, runner)
	runner.Timeout = 300 * time.Millisecond
	if _, err := runner.Turn(context.Background(), Turn{Session: "s-late", Resume: true, Prompt: "hang"}); err == nil {
		t.Fatal("turn must fail when the agent never settles")
	}
	if got, err := runner.Turn(context.Background(), Turn{Session: "s-late", Resume: true, Prompt: "two"}); err != nil || got != "echo:two" {
		t.Fatalf("next turn = %q, %v", got, err)
	}
	if n := countSpawns(readFakePiLog(t, logPath)); n != 1 {
		t.Fatalf("spawns=%d, want 1 (confirmed proc stays resident)", n)
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
