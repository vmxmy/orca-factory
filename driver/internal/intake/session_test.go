package intake

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, repo, name string) string {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", name)
	git(t, repo, "push", "-q", "origin", "main")
	return git(t, repo, "rev-parse", "HEAD")
}

type call struct {
	args, env  []string
	cwd, stdin string
}

// calls returns the stand-in invocations recorded since the last call and clears them.
func calls(t *testing.T, logs string) []call {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(logs, "call.*"))
	if err != nil {
		t.Fatal(err)
	}
	var out []call
	for _, dir := range dirs {
		read := func(name string) []string {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
		}
		env := read("env")
		slices.Sort(env)
		out = append(out, call{args: read("args"), env: env, cwd: read("cwd")[0], stdin: read("stdin")[0]})
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func session(t *testing.T, state, key string) Session {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIntakeSession(t *testing.T) {
	t.Setenv("AGENT_FEISHU_APP_SECRET", "must-not-reach-agent")
	root := t.TempDir()
	git(t, root, "init", "-q", "--bare", "-b", "main", "origin.git")
	repo := filepath.Join(root, "repo")
	git(t, root, "init", "-q", "-b", "main", repo)
	git(t, repo, "remote", "add", "origin", filepath.Join(root, "origin.git"))
	head := commit(t, repo, "scripts/agent-gates/roles/intake-chat.md")
	recorded, err := filepath.Abs("../../testdata/recorded/claude-print-json-result.json")
	if err != nil {
		t.Fatal(err)
	}
	logs, hold := filepath.Join(root, "calls"), filepath.Join(root, "hold")
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	standIn := filepath.Join(root, "claude")
	script := "#!/bin/sh\nd=$(mktemp -d " + logs + "/call.XXXXXX)\nprintf '%s\\0' \"$@\" > $d/args\ncat /proc/$$/environ > $d/env\npwd | tr -d '\\n' > $d/cwd\ncat > $d/stdin\n" +
		"while [ -e " + hold + " ]; do sleep 0.01; done\nexec cat " + recorded + "\n"
	if err := os.WriteFile(standIn, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(root, "checkout")
	runner := &Runner{Claude: standIn, Repo: repo, Checkout: checkout, Model: "claude-opus-5-5", Effort: "high", Timeout: 10 * time.Second}
	var mu sync.Mutex
	var replies []string
	reply := func(_ context.Context, _, text string) error {
		mu.Lock()
		defer mu.Unlock()
		replies = append(replies, text)
		return nil
	}
	taken := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := replies
		replies = nil
		return out
	}
	state := filepath.Join(root, "state")
	g, err := New(state, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, 2, nil, "", runner.Turn, reply, noopReact)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	argv := func(flag, id string) []string {
		return []string{"-p", "--output-format", "json", flag, id, "--model", "claude-opus-5-5", "--effort", "high",
			"--append-system-prompt-file", checkout + "/scripts/agent-gates/roles/intake-chat.md",
			"--tools", "Read,Grep,Glob,Skill", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config",
			"--settings", `{"permissions":{"deny":["Read(~/.claude/**)","Read(~/.config/**)","Read(~/.local/state/**)","Read(~/.ssh/**)"]}}`}
	}
	wantEnv := []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	answer := "pong\n\nROLE-LOADED"

	first, second := recordedEvent(t, "bot-event-message-at"), recordedEvent(t, "bot-event-thread-at-reply")
	thread := *first.Event.Message.MessageId
	var id string
	for i, turn := range []struct {
		event        *im.P2MessageReceiveV1
		flag, prompt string
	}{{first, "--session-id", "原始格式测试"}, {second, "--resume", "话题内带艾特"}} {
		if err := g.Handle(ctx, turn.event); err != nil {
			t.Fatal(err)
		}
		s := session(t, state, thread)
		if i == 0 {
			id = s.ID
		}
		got := calls(t, logs)
		if len(got) != 1 || !slices.Equal(got[0].args, argv(turn.flag, id)) || !slices.Equal(got[0].env, wantEnv) || got[0].cwd != checkout || got[0].stdin != "[需求人] "+turn.prompt {
			t.Fatalf("turn %d call %#v", i, got)
		}
		if s.Requester != "ou_REDACTED" || s.State != Chatting {
			t.Fatalf("session %+v", s)
		}
		if r := taken(); !slices.Equal(r, []string{answer}) {
			t.Fatalf("replies %q", r)
		}
	}
	if git(t, checkout, "rev-parse", "HEAD") != head {
		t.Fatal("checkout is not origin/main")
	}

	head = commit(t, repo, "later.txt")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"om_c1", "om_c2", "om_c3"} {
		event := recordedEvent(t, "bot-event-message-at")
		event.Event.Message.MessageId = ptr(id)
		wg.Go(func() {
			if err := g.Handle(ctx, event); err != nil {
				t.Error(err)
			}
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		started, _ := filepath.Glob(filepath.Join(logs, "call.*"))
		mu.Lock()
		queued := slices.Contains(replies, "排队中")
		mu.Unlock()
		if len(started) == 2 && queued {
			break
		}
		if len(started) > 2 || time.Now().After(deadline) {
			t.Fatalf("started=%d queued=%v", len(started), queued)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if got := calls(t, logs); len(got) != 3 {
		t.Fatalf("concurrent calls %d", len(got))
	}
	if r := taken(); len(r) != 4 || slices.Index(r, "排队中") < 0 || len(slices.DeleteFunc(r, func(s string) bool { return s == answer })) != 1 {
		t.Fatalf("concurrent replies %q", r)
	}
	if git(t, checkout, "rev-parse", "HEAD") != head {
		t.Fatal("new session did not refresh checkout")
	}

	before := session(t, state, thread)
	runner.Timeout = 200 * time.Millisecond
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second.Event.Message.MessageId = ptr("om_timeout")
	if err := g.Handle(ctx, second); err != nil {
		t.Fatal(err)
	}
	calls(t, logs)
	if r := taken(); !slices.Equal(r, []string{"处理失败，请重试"}) {
		t.Fatalf("timeout replies %q", r)
	}
	if after := session(t, state, thread); after != before {
		t.Fatalf("failed turn changed session %+v", after)
	}

	g.now = func() time.Time { return before.LastMessage.Add(7*24*time.Hour + time.Second) }
	second.Event.Message.MessageId = ptr("om_expired")
	if err := g.Handle(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, logs); len(got) != 0 {
		t.Fatal("closed session ran the agent")
	}
	if r := taken(); !slices.Equal(r, []string{closedReply}) || session(t, state, thread).State != Closed {
		t.Fatalf("expired replies %q", r)
	}
}
