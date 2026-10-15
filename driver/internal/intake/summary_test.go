package intake

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// rollingSetup builds a Gateway backed by a stand-in claude command, the
// same shape TestIntakeSession uses, for the rolling-summary tests.
func rollingSetup(t *testing.T, script func(logs string) string) (g *Gateway, logs, state, checkout string, taken func() []string) {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "-q", "--bare", "-b", "main", "origin.git")
	repo := filepath.Join(root, "repo")
	git(t, root, "init", "-q", "-b", "main", repo)
	git(t, repo, "remote", "add", "origin", filepath.Join(root, "origin.git"))
	commit(t, repo, "scripts/agent-gates/roles/intake-chat.md")
	logs = filepath.Join(root, "calls")
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	standIn := filepath.Join(root, "claude")
	if err := os.WriteFile(standIn, []byte(script(logs)), 0o755); err != nil {
		t.Fatal(err)
	}
	checkout = filepath.Join(root, "checkout")
	runner := &Runner{Claude: standIn, Repo: repo, Checkout: checkout, Model: "claude-opus-5-5", Effort: "high", Timeout: 10 * time.Second}
	var mu sync.Mutex
	var replies []string
	reply := func(_ context.Context, _, text string) error {
		mu.Lock()
		defer mu.Unlock()
		replies = append(replies, text)
		return nil
	}
	taken = func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := replies
		replies = nil
		return out
	}
	state = filepath.Join(root, "state")
	var err error
	g, err = New(state, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, 2, nil, "", runner.Turn, reply, noopReact)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// branchingScript returns, for each call, the summary-fixture reply when the
// prompt carries a summary request and the plain-fixture reply otherwise.
func branchingScript(normal, summary string) func(string) string {
	return func(logs string) string {
		return "#!/bin/sh\n" +
			"d=$(mktemp -d " + logs + "/call.XXXXXX)\n" +
			"printf '%s\\0' \"$@\" > $d/args\n" +
			"cat /proc/$$/environ > $d/env\n" +
			"pwd | tr -d '\\n' > $d/cwd\n" +
			"cat > $d/stdin\n" +
			"if grep -q intake-summary-v1 $d/stdin; then exec cat " + summary + "; else exec cat " + normal + "; fi\n"
	}
}

func TestIntakeRollingSummary(t *testing.T) {
	recordedNormal, err := filepath.Abs("../../testdata/recorded/claude-print-json-result.json")
	if err != nil {
		t.Fatal(err)
	}
	recordedSummary, err := filepath.Abs("../../testdata/recorded/claude-print-json-summary-result.json")
	if err != nil {
		t.Fatal(err)
	}
	argv := func(flag, id, checkout string) []string {
		return []string{"-p", "--output-format", "json", flag, id, "--model", "claude-opus-5-5", "--effort", "high",
			"--append-system-prompt-file", checkout + "/scripts/agent-gates/roles/intake-chat.md",
			"--tools", "Read,Grep,Glob,Skill", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config",
			"--settings", `{"permissions":{"deny":["Read(~/.claude/**)","Read(~/.config/**)","Read(~/.local/state/**)","Read(~/.ssh/**)"]}}`}
	}
	wantEnv := []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	pong := "pong\n\nROLE-LOADED"

	t.Run("accepted", func(t *testing.T) {
		t.Setenv("AGENT_INTAKE_MAX_TURNS", "3")
		g, logs, state, checkout, taken := rollingSetup(t, branchingScript(recordedNormal, recordedSummary))
		ctx := context.Background()

		first := recordedEvent(t, "bot-event-message-at")
		thread := *first.Event.Message.MessageId
		if err := g.Handle(ctx, first); err != nil {
			t.Fatal(err)
		}
		originalID := session(t, state, thread).ID
		if got := calls(t, logs); len(got) != 1 || !slices.Equal(got[0].args, argv("--session-id", originalID, checkout)) || !slices.Equal(got[0].env, wantEnv) || got[0].cwd != checkout {
			t.Fatalf("turn 1 call %#v", got)
		}
		if r := taken(); !slices.Equal(r, []string{pong}) {
			t.Fatalf("turn 1 replies %q", r)
		}

		turn2 := recordedEvent(t, "bot-event-thread-at-reply")
		turn2.Event.Message.MessageId = ptr("om_rolling_turn2")
		if err := g.Handle(ctx, turn2); err != nil {
			t.Fatal(err)
		}
		if got := calls(t, logs); len(got) != 1 || !slices.Equal(got[0].args, argv("--resume", originalID, checkout)) {
			t.Fatalf("turn 2 call %#v", got)
		}
		taken()

		turn3 := recordedEvent(t, "bot-event-thread-at-reply")
		turn3.Event.Message.MessageId = ptr("om_rolling_turn3")
		if err := g.Handle(ctx, turn3); err != nil {
			t.Fatal(err)
		}
		got := calls(t, logs)
		if len(got) != 2 {
			t.Fatalf("turn 3 calls %#v", got)
		}
		// calls() globs call.* directories in lexical order, not call order,
		// since mktemp's random suffix carries no timestamp: classify by
		// content instead of assuming which ran first.
		summaryCalls := slices.DeleteFunc(slices.Clone(got), func(c call) bool { return !strings.Contains(c.stdin, "intake-summary-v1") })
		replyCalls := slices.DeleteFunc(slices.Clone(got), func(c call) bool { return strings.Contains(c.stdin, "intake-summary-v1") })
		if len(summaryCalls) != 1 || len(replyCalls) != 1 {
			t.Fatalf("turn 3 calls %#v", got)
		}
		if !slices.Equal(replyCalls[0].args, argv("--resume", originalID, checkout)) {
			t.Fatalf("turn 3 reply call %#v", replyCalls[0])
		}
		if !slices.Equal(summaryCalls[0].args, argv("--resume", originalID, checkout)) {
			t.Fatalf("turn 3 summary-request call %#v", summaryCalls[0])
		}
		taken()

		rolled := session(t, state, thread)
		if rolled.ID == originalID {
			t.Fatal("session ID did not roll over")
		}
		if rolled.Generation != 1 || rolled.Turns != 0 || rolled.PendingSummary == "" {
			t.Fatalf("rolled session %+v", rolled)
		}

		turn4 := recordedEvent(t, "bot-event-thread-at-reply")
		turn4.Event.Message.MessageId = ptr("om_rolling_turn4")
		if err := g.Handle(ctx, turn4); err != nil {
			t.Fatal(err)
		}
		got = calls(t, logs)
		if len(got) != 1 || !slices.Equal(got[0].args, argv("--session-id", rolled.ID, checkout)) {
			t.Fatalf("turn 4 call %#v", got)
		}
		if !strings.Contains(got[0].stdin, "示例需求") || !strings.Contains(got[0].stdin, "话题内带艾特") {
			t.Fatalf("turn 4 stdin %q", got[0].stdin)
		}
		if after := session(t, state, thread); after.PendingSummary != "" {
			t.Fatalf("pending summary not cleared: %+v", after)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		t.Setenv("AGENT_INTAKE_MAX_TURNS", "3")
		g, logs, state, checkout, taken := rollingSetup(t, branchingScript(recordedNormal, recordedNormal))
		ctx := context.Background()

		first := recordedEvent(t, "bot-event-message-at")
		thread := *first.Event.Message.MessageId
		if err := g.Handle(ctx, first); err != nil {
			t.Fatal(err)
		}
		originalID := session(t, state, thread).ID
		calls(t, logs)
		taken()

		turn2 := recordedEvent(t, "bot-event-thread-at-reply")
		turn2.Event.Message.MessageId = ptr("om_rejected_turn2")
		if err := g.Handle(ctx, turn2); err != nil {
			t.Fatal(err)
		}
		calls(t, logs)
		taken()

		turn3 := recordedEvent(t, "bot-event-thread-at-reply")
		turn3.Event.Message.MessageId = ptr("om_rejected_turn3")
		if err := g.Handle(ctx, turn3); err != nil {
			t.Fatal(err)
		}
		if got := calls(t, logs); len(got) != 2 {
			t.Fatalf("turn 3 calls %#v", got)
		}
		taken()

		kept := session(t, state, thread)
		if kept.ID != originalID || kept.Generation != 0 || kept.PendingSummary != "" {
			t.Fatalf("rejected summary changed session %+v", kept)
		}

		turn4 := recordedEvent(t, "bot-event-thread-at-reply")
		turn4.Event.Message.MessageId = ptr("om_rejected_turn4")
		if err := g.Handle(ctx, turn4); err != nil {
			t.Fatal(err)
		}
		got := calls(t, logs)
		if len(got) == 0 || !slices.Equal(got[0].args, argv("--resume", originalID, checkout)) {
			t.Fatalf("turn 4 call %#v", got)
		}
	})
}
