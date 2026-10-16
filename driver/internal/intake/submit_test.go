package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

// fakeLedger stands in for the project's issues.Store. Create mirrors the
// Feishu backend's adoption rule: both dependency lists must be explicit.
type fakeLedger struct {
	tasks   []issues.Task
	creates []issues.CreateInput
	updates []ledgerUpdate
	events  map[string][]string
}

type ledgerUpdate struct{ id, state, add string }

func (l *fakeLedger) List(context.Context) ([]issues.Task, error) { return slices.Clone(l.tasks), nil }

func (l *fakeLedger) Create(_ context.Context, in issues.CreateInput) (issues.Task, error) {
	l.creates = append(l.creates, in)
	if in.Blockers == nil || in.Related == nil {
		return issues.Task{}, fmt.Errorf("explicit adoption required")
	}
	n := len(l.creates)
	task := issues.Task{URL: fmt.Sprintf("https://ledger.example/rec%d", n), Title: in.Title, Description: in.Description}
	task.Metadata = issues.Metadata{ID: fmt.Sprintf("canon-%d", n), Identifier: fmt.Sprintf("OF-%d", 100+n), State: in.State, Blockers: in.Blockers, Related: in.Related, Complete: true}
	l.tasks = append(l.tasks, task)
	return task, nil
}

func (l *fakeLedger) index(id string) (int, error) {
	i := slices.IndexFunc(l.tasks, func(t issues.Task) bool { return t.Metadata.ID == id || t.Metadata.Identifier == id })
	if i < 0 {
		return 0, fmt.Errorf("task %s not found", id)
	}
	return i, nil
}

func (l *fakeLedger) Read(_ context.Context, id string) (issues.Task, error) {
	i, err := l.index(id)
	if err != nil {
		return issues.Task{}, err
	}
	return l.tasks[i], nil
}

func (l *fakeLedger) UpdateIssue(_ context.Context, id, state string, add, _ []string) (linear.Issue, error) {
	i, err := l.index(id)
	if err != nil {
		return linear.Issue{}, err
	}
	l.updates = append(l.updates, ledgerUpdate{id: id, state: state, add: strings.Join(add, ",")})
	t := &l.tasks[i]
	if state != "" {
		t.Metadata.State = state
	}
	for _, label := range add {
		t.Labels.Nodes = append(t.Labels.Nodes, linear.NamedID{ID: label, Name: label})
	}
	return t.Issue, nil
}

func (l *fakeLedger) ListComments(_ context.Context, id string) ([]linear.Comment, error) {
	var out []linear.Comment
	for _, body := range l.events[id] {
		out = append(out, linear.Comment{Body: body})
	}
	return out, nil
}

func (l *fakeLedger) CreateComment(_ context.Context, id, body string) (linear.Comment, error) {
	if l.events == nil {
		l.events = map[string][]string{}
	}
	l.events[id] = append(l.events[id], body)
	return linear.Comment{Body: body}, nil
}

func existingLedger() *fakeLedger {
	of12 := issues.Task{Title: "已有前置票"}
	of12.Metadata = issues.Metadata{ID: "canon-of12", Identifier: "OF-12", State: "Todo", Complete: true}
	return &fakeLedger{tasks: []issues.Task{of12}}
}

func draftReply(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return "整理好了，请核对：\n```intake-draft-v1\n" + string(b) + "\n```"
}

func draftTicket(key, scope string, deps ...string) map[string]any {
	t := map[string]any{"title": "网关确认提交 " + key, "要做": []string{"解析草稿", "需求人确认后建票"}, "CHECK": "cd driver && go test ./internal/intake -run TestIntakeSubmit", "SCOPE": scope,
		"正文": map[string]string{"现状": "网关没有提交路径", "断言": "确认后建票", "依赖说明": "无"}, "estimate": 3, "blockers": []string{}, "related": []string{}, "lane": 1}
	if key != "" {
		t["key"], t["depends_on"] = key, deps
	} else {
		t["blockers"] = deps
	}
	return t
}

type submitHarness struct {
	g       *Gateway
	dir     string
	ledger  *fakeLedger
	turns   []Turn
	replies []string
	targets []string
	answers []string
}

func newSubmitHarness(t *testing.T, ledger *fakeLedger, answers ...string) *submitHarness {
	t.Helper()
	checkout := t.TempDir()
	for _, f := range []string{"driver/internal/intake/gateway.go", ".env.example", "README.md"} {
		if err := os.MkdirAll(filepath.Join(checkout, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(checkout, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &submitHarness{dir: t.TempDir(), ledger: ledger, answers: answers}
	h.start(t, Project{Prefix: "OF", Name: "orca-factory", Remote: "github.com/vmxmy/orca-factory", Repo: "/repo/of", Enabled: true, Checkout: checkout, Ledger: ledger, Lane2Globs: "driver/*", Lane3Globs: ".env* *secrets*"})
	return h
}

// start builds a gateway over h.dir, so calling it again simulates a restart.
func (h *submitHarness) start(t *testing.T, p Project) {
	t.Helper()
	agent := func(_ context.Context, turn Turn) (string, error) {
		h.turns = append(h.turns, turn)
		if len(h.answers) == 0 {
			t.Fatalf("unexpected agent turn %q", turn.Prompt)
		}
		answer := h.answers[0]
		h.answers = h.answers[1:]
		return answer, nil
	}
	reply := func(_ context.Context, id, text string) error {
		h.replies = append(h.replies, text)
		h.targets = append(h.targets, id)
		return nil
	}
	g, err := New(h.dir, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED", "ou_MEMBER"}, []string{"ou_APPROVER1", "ou_APPROVER2"}, 1, []Project{p}, p.Remote, agent, reply, noopReact, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.g = g
}

func (h *submitHarness) open(t *testing.T, thread, text string) {
	t.Helper()
	event := recordedEvent(t, "bot-event-message-at")
	event.Event.Message.MessageId = ptr(thread)
	event.Event.Message.Content = ptr(`{"text":"@_user_1 #OF ` + text + `"}`)
	h.handle(t, event)
}

func (h *submitHarness) say(t *testing.T, thread, id, sender, text string) {
	t.Helper()
	event := recordedEvent(t, "bot-event-thread-reply-no-at")
	event.Event.Message.MessageId = ptr(id)
	event.Event.Message.RootId = ptr(thread)
	event.Event.Sender.SenderId.OpenId = ptr(sender)
	event.Event.Message.Content = ptr(`{"text":"` + text + `"}`)
	h.handle(t, event)
}

func (h *submitHarness) handle(t *testing.T, event *im.P2MessageReceiveV1) {
	t.Helper()
	if err := h.g.Handle(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

func (h *submitHarness) lastReply(t *testing.T) string {
	t.Helper()
	if len(h.replies) == 0 {
		t.Fatal("no reply sent")
	}
	return h.replies[len(h.replies)-1]
}

// TestIntakeSubmit covers OF-15: a valid intake-draft-v1 is summarized with a
// gateway-computed lane and waits for confirmation; only the requester's
// exact “确认提交” creates managed Todo tickets without ready-for-agent,
// once per thread even across repeats and restarts; drafts that fail
// validation go back to the Agent without reaching the requester.
func TestIntakeSubmit(t *testing.T) {
	t.Run("requester confirm creates exactly one managed ticket", func(t *testing.T) {
		const thread = "om_submit1"
		h := newSubmitHarness(t, existingLedger(), draftReply(draftTicket("", "driver/internal/intake/*", "OF-12")))
		h.open(t, thread, "网关要能提交需求")
		summary := h.lastReply(t)
		if !strings.Contains(summary, "LANE 预判：2") || !strings.Contains(summary, "确认提交") || strings.Contains(summary, "intake-draft-v1") {
			t.Fatalf("summary=%q", summary)
		}
		if got := session(t, h.dir, thread); got.State != AwaitingConfirm || len(got.Draft) != 1 {
			t.Fatalf("session=%+v", got)
		}
		pending, err := os.ReadFile(filepath.Join(h.dir, thread+".json"))
		if err != nil {
			t.Fatal(err)
		}

		h.say(t, thread, "om_member_confirm", "ou_MEMBER", "确认提交")
		if len(h.ledger.creates) != 0 || !strings.Contains(h.lastReply(t), "仅需求人") {
			t.Fatalf("member confirm: creates=%d reply=%q", len(h.ledger.creates), h.lastReply(t))
		}
		h.answers = append(h.answers, quietMarker)
		h.say(t, thread, "om_confirm_fuzzy", "ou_REDACTED", "确认提交吧")
		if len(h.ledger.creates) != 0 {
			t.Fatal("inexact confirm created a ticket")
		}

		h.say(t, thread, "om_confirm1", "ou_REDACTED", "确认提交")
		if len(h.ledger.creates) != 1 {
			t.Fatalf("creates=%d", len(h.ledger.creates))
		}
		in := h.ledger.creates[0]
		if in.State != "Todo" || len(in.Labels) != 0 || !slices.Equal(in.Blockers, []string{"canon-of12"}) || in.Related == nil || len(in.Related) != 0 || in.Estimate == nil || *in.Estimate != 3 {
			t.Fatalf("create input=%+v", in)
		}
		for _, want := range []string{"提出人：ou_REDACTED", "\nintake-chat-v1: om_submit1\n", "## LANE 预判\n2", "## 要做\n1. 解析草稿\n2. 需求人确认后建票", "CHECK: cd driver", "## SCOPE\ndriver/internal/intake/*"} {
			if !strings.Contains(in.Description, want) {
				t.Fatalf("description lacks %q:\n%s", want, in.Description)
			}
		}
		done := h.lastReply(t)
		for _, want := range []string{"OF-101", "https://ledger.example/rec1", `<at user_id="ou_APPROVER1"></at>`, `<at user_id="ou_APPROVER2"></at>`} {
			if !strings.Contains(done, want) {
				t.Fatalf("submit reply lacks %q: %q", want, done)
			}
		}
		if got := session(t, h.dir, thread); got.State != Submitted {
			t.Fatalf("state=%s", got.State)
		}

		h.say(t, thread, "om_confirm2", "ou_REDACTED", "确认提交")
		if len(h.ledger.creates) != 1 || !strings.Contains(h.lastReply(t), "OF-101") {
			t.Fatalf("repeat confirm: creates=%d reply=%q", len(h.ledger.creates), h.lastReply(t))
		}

		// A crash between Create and the session write leaves the session
		// pending; the ledger marker still stops a second ticket.
		if err := os.WriteFile(filepath.Join(h.dir, thread+".json"), pending, 0o600); err != nil {
			t.Fatal(err)
		}
		h.start(t, h.g.projects[0])
		h.say(t, thread, "om_confirm3", "ou_REDACTED", "确认提交")
		if len(h.ledger.creates) != 1 || !strings.Contains(h.lastReply(t), "OF-101") {
			t.Fatalf("restart confirm: creates=%d reply=%q", len(h.ledger.creates), h.lastReply(t))
		}
		if len(h.turns) != 2 {
			t.Fatalf("confirm reached the agent: %d turns", len(h.turns))
		}
	})

	t.Run("invalid draft goes back to the agent, not the requester", func(t *testing.T) {
		const thread = "om_submit2"
		h := newSubmitHarness(t, existingLedger(),
			draftReply(draftTicket("", "nope/* driver/*", "OF-999")),
			draftReply(draftTicket("", "driver/*")))
		h.open(t, thread, "网关要能提交需求")
		if len(h.turns) != 2 || !h.turns[1].Resume || !strings.Contains(h.turns[1].Prompt, "nope/*") || !strings.Contains(h.turns[1].Prompt, "OF-999") {
			t.Fatalf("turns=%+v", h.turns)
		}
		if len(h.replies) != 1 || strings.Contains(h.replies[0], "nope") || !strings.Contains(h.replies[0], "LANE 预判：2") {
			t.Fatalf("replies=%q", h.replies)
		}
		if len(h.ledger.creates) != 0 {
			t.Fatal("ticket created without confirmation")
		}
	})

	t.Run("dependency cycle goes back to the agent", func(t *testing.T) {
		const thread = "om_submit3"
		h := newSubmitHarness(t, existingLedger(),
			draftReply(map[string]any{"tickets": []any{draftTicket("a", "driver/*", "b"), draftTicket("b", "driver/*", "a")}}),
			"好的，我再和需求人确认拆分")
		h.open(t, thread, "网关要能提交需求")
		if len(h.turns) != 2 || !strings.Contains(h.turns[1].Prompt, "环") {
			t.Fatalf("turns=%+v", h.turns)
		}
		if got := session(t, h.dir, thread); got.State != Chatting || got.Draft != nil {
			t.Fatalf("session=%+v", got)
		}
		if h.lastReply(t) != "好的，我再和需求人确认拆分" {
			t.Fatalf("reply=%q", h.lastReply(t))
		}
	})

	t.Run("multi-ticket draft is created in dependency order", func(t *testing.T) {
		const thread = "om_submit4"
		h := newSubmitHarness(t, existingLedger(),
			draftReply(map[string]any{"tickets": []any{draftTicket("b", ".env.example", "a"), draftTicket("a", "driver/*", "OF-12")}}))
		h.open(t, thread, "网关要能提交需求")
		h.say(t, thread, "om_confirm_multi", "ou_REDACTED", "确认提交")
		c := h.ledger.creates
		if len(c) != 2 || c[0].Title != "网关确认提交 a" || c[1].Title != "网关确认提交 b" {
			t.Fatalf("creates=%+v", c)
		}
		if !slices.Equal(c[0].Blockers, []string{"canon-of12"}) || !slices.Equal(c[1].Blockers, []string{"canon-1"}) {
			t.Fatalf("blockers a=%v b=%v", c[0].Blockers, c[1].Blockers)
		}
		if !strings.Contains(c[0].Description, "\nintake-chat-v1: om_submit4 a\n") || !strings.Contains(c[1].Description, "## LANE 预判\n3") {
			t.Fatalf("descriptions:\n%s\n---\n%s", c[0].Description, c[1].Description)
		}
		if !strings.Contains(h.lastReply(t), "OF-101") || !strings.Contains(h.lastReply(t), "OF-102") {
			t.Fatalf("reply=%q", h.lastReply(t))
		}
	})
}
