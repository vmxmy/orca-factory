package intake

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// recordedCardContent returns the card JSON a recorded send or update request
// carried, decoded so a comparison ignores key order and whitespace.
func recordedCardContent(t *testing.T, name string) any {
	t.Helper()
	b, err := os.ReadFile("../../testdata/recorded/feishu/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var recording struct {
		Request struct {
			Body struct {
				Content string `json:"content"`
			} `json:"body"`
		} `json:"request"`
	}
	if err := json.Unmarshal(b, &recording); err != nil {
		t.Fatal(err)
	}
	return decodeCard(t, recording.Request.Body.Content)
}

func decodeCard(t *testing.T, card string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(card), &v); err != nil {
		t.Fatalf("card is not JSON: %v", err)
	}
	return v
}

type sent struct{ target, content string }

// pickerHarness drives one gateway whose project set is the one the recorded
// picker card was built from (bot-message-reply-card.json).
type pickerHarness struct {
	g                       *Gateway
	turns                   []Turn
	replies, cards, updates []sent
}

func newPickerHarness(t *testing.T) *pickerHarness {
	t.Helper()
	of := recordedLedgerProject(t)
	of.Repo = "/repo/of"
	projects := []Project{
		of,
		{Prefix: "ZY", Name: "zhiyun-ai-dashboard", Remote: "github.com/vmxmy/zhiyun-ai-dashboard", Enabled: true, Repo: "/repo/zy"},
		{Prefix: "AN", Name: "ansible", Remote: "github.com/vmxmy/ansible", Enabled: true, Repo: "/repo/an"},
		{Prefix: "OLD", Name: "retired", Remote: "github.com/vmxmy/retired", Enabled: false, Repo: "/repo/old"},
		{Prefix: "DOC", Name: "docs-site", Remote: "github.com/vmxmy/docs-site", Enabled: true, Reason: "未装流水线"},
	}
	h := &pickerHarness{}
	agent := func(_ context.Context, turn Turn) (string, error) {
		h.turns = append(h.turns, turn)
		return "已整理", nil
	}
	record := func(into *[]sent) func(context.Context, string, string) error {
		return func(_ context.Context, target, content string) error {
			*into = append(*into, sent{target, content})
			return nil
		}
	}
	g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED", "ou_MEMBER"}, nil, 1, projects, of.Remote, agent, record(&h.replies), noopReact, record(&h.cards), record(&h.updates))
	if err != nil {
		t.Fatal(err)
	}
	h.g = g
	return h
}

const (
	pickerRoot        = "om_REDACTED_ROOT"
	pickerCardMessage = "om_REDACTED_CARD"
)

func (h *pickerHarness) open(t *testing.T, text string) {
	t.Helper()
	event := recordedEvent(t, "bot-event-message-at")
	event.Event.Message.MessageId = ptr(pickerRoot)
	event.Event.Message.Content = ptr(`{"text":"@_user_1 ` + text + `"}`)
	if err := h.g.Handle(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

func (h *pickerHarness) say(t *testing.T, id, text string) {
	t.Helper()
	event := recordedEvent(t, "bot-event-thread-reply-no-at")
	event.Event.Message.MessageId = ptr(id)
	event.Event.Message.RootId = ptr(pickerRoot)
	event.Event.Message.Content = ptr(`{"text":"` + text + `"}`)
	if err := h.g.Handle(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

// click replays the constructed card.action.trigger callback through the
// SDK dispatcher, as the gateway's long connection delivers it, with the
// operator and the submitted project replaced. It returns the toast and the
// deferred work the gateway asked to run after the callback response.
func (h *pickerHarness) click(t *testing.T, operator, prefix string) (*callback.Toast, func(context.Context) error) {
	t.Helper()
	b, err := os.ReadFile("../../testdata/recorded/feishu/constructed-card-action-project-pick.json")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	event := payload["event"].(map[string]any)
	event["operator"].(map[string]any)["open_id"] = operator
	event["action"].(map[string]any)["form_value"] = map[string]any{"project": prefix}
	if b, err = json.Marshal(payload); err != nil {
		t.Fatal(err)
	}
	var begin func(context.Context) error
	handler := dispatcher.NewEventDispatcher("", "").OnP2CardActionTrigger(func(_ context.Context, e *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		var response *callback.CardActionTriggerResponse
		response, begin = h.g.HandleCardAction(e)
		return response, nil
	})
	out, err := handler.Do(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	response, ok := out.(*callback.CardActionTriggerResponse)
	if !ok || response.Toast == nil || response.Card != nil {
		t.Fatalf("callback response=%#v, want a toast and no card", out)
	}
	return response.Toast, begin
}

func run(t *testing.T, begin func(context.Context) error) {
	t.Helper()
	if begin == nil {
		t.Fatal("no deferred work for an accepted pick")
	}
	if err := begin(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestIntakeProjectPicker covers OF-47: a new thread's first message without
// a project marker gets an interactive card listing every project the
// gateway can take over (group default preselected) and why the others are
// unavailable; the session waits with the first message as PendingPrompt.
// Only the requester's submission counts. An accepted pick replaces the card
// with a non-interactive "已选择" card through the message update API and
// opens the conversation with the first message, exactly once however often
// the button fires. Replying with an index or prefix still works.
func TestIntakeProjectPicker(t *testing.T) {
	selectedZY := recordedCardContent(t, "bot-message-update-card.json")

	t.Run("first message without marker sends the picker card", func(t *testing.T) {
		h := newPickerHarness(t)
		h.open(t, "帮我整理这个需求")
		if len(h.cards) != 1 || h.cards[0].target != pickerRoot {
			t.Fatalf("cards=%+v", h.cards)
		}
		if got, want := decodeCard(t, h.cards[0].content), recordedCardContent(t, "bot-message-reply-card.json"); !reflect.DeepEqual(got, want) {
			t.Fatalf("card=%s\nwant the recorded card", h.cards[0].content)
		}
		if len(h.turns) != 0 || len(h.replies) != 0 {
			t.Fatalf("turns=%+v replies=%+v", h.turns, h.replies)
		}
		if got := session(t, h.g.dir, pickerRoot); got.Project != "" || got.PendingPrompt != "帮我整理这个需求" || got.Requester != "ou_REDACTED" {
			t.Fatalf("session=%+v", got)
		}
	})

	t.Run("requester pick updates the card and opens the conversation", func(t *testing.T) {
		h := newPickerHarness(t)
		h.open(t, "帮我整理这个需求")
		toast, begin := h.click(t, "ou_REDACTED", "ZY")
		if toast.Type != "success" || toast.Content != "已选择 ZY zhiyun-ai-dashboard" {
			t.Fatalf("toast=%+v", toast)
		}
		run(t, begin)
		if len(h.updates) != 1 || h.updates[0].target != pickerCardMessage || !reflect.DeepEqual(decodeCard(t, h.updates[0].content), selectedZY) {
			t.Fatalf("updates=%+v", h.updates)
		}
		if len(h.turns) != 1 || h.turns[0].Project.Prefix != "ZY" || h.turns[0].Prompt != "[需求人] 帮我整理这个需求" || h.turns[0].Resume {
			t.Fatalf("turns=%+v", h.turns)
		}
		if len(h.replies) != 1 || h.replies[0] != (sent{pickerRoot, "已整理"}) {
			t.Fatalf("replies=%+v", h.replies)
		}
		if got := session(t, h.g.dir, pickerRoot); got.Project != "ZY" || got.PendingPrompt != "" {
			t.Fatalf("session=%+v", got)
		}
	})

	t.Run("non-requester click changes nothing", func(t *testing.T) {
		h := newPickerHarness(t)
		h.open(t, "帮我整理这个需求")
		toast, begin := h.click(t, "ou_MEMBER", "ZY")
		if toast.Content != "只有需求人可以选择项目" || begin != nil {
			t.Fatalf("toast=%+v begin=%v", toast, begin != nil)
		}
		if got := session(t, h.g.dir, pickerRoot); got.Project != "" || got.PendingPrompt != "帮我整理这个需求" {
			t.Fatalf("session=%+v", got)
		}
		if len(h.turns) != 0 || len(h.updates) != 0 {
			t.Fatalf("turns=%+v updates=%+v", h.turns, h.updates)
		}
	})

	t.Run("repeated clicks open the conversation once", func(t *testing.T) {
		h := newPickerHarness(t)
		h.open(t, "帮我整理这个需求")
		_, first := h.click(t, "ou_REDACTED", "ZY")
		_, second := h.click(t, "ou_REDACTED", "ZY")
		run(t, first)
		run(t, second)
		toast, third := h.click(t, "ou_REDACTED", "OF")
		if toast.Content != "已选择 ZY zhiyun-ai-dashboard" {
			t.Fatalf("toast after selection=%+v", toast)
		}
		run(t, third)
		if len(h.turns) != 1 || h.turns[0].Project.Prefix != "ZY" {
			t.Fatalf("turns=%+v", h.turns)
		}
		for _, u := range h.updates {
			if u.target != pickerCardMessage || !reflect.DeepEqual(decodeCard(t, u.content), selectedZY) {
				t.Fatalf("updates=%+v", h.updates)
			}
		}
		if got := session(t, h.g.dir, pickerRoot); got.Project != "ZY" {
			t.Fatalf("session=%+v", got)
		}
	})

	for _, tc := range []struct{ prefix, reason string }{{"OLD", "未启用"}, {"DOC", "未装流水线"}, {"NOPE", "未识别该项目"}} {
		t.Run("pick of unusable "+tc.prefix+" stays pending", func(t *testing.T) {
			h := newPickerHarness(t)
			h.open(t, "帮我整理这个需求")
			toast, begin := h.click(t, "ou_REDACTED", tc.prefix)
			if toast.Type != "error" || !strings.Contains(toast.Content, tc.reason) || begin != nil {
				t.Fatalf("toast=%+v begin=%v", toast, begin != nil)
			}
			if got := session(t, h.g.dir, pickerRoot); got.Project != "" || got.PendingPrompt != "帮我整理这个需求" {
				t.Fatalf("session=%+v", got)
			}
		})
	}

	t.Run("marker #ZY skips the card", func(t *testing.T) {
		h := newPickerHarness(t)
		h.open(t, "#ZY 帮我整理这个需求")
		if len(h.cards) != 0 || len(h.turns) != 1 || h.turns[0].Project.Prefix != "ZY" {
			t.Fatalf("cards=%+v turns=%+v", h.cards, h.turns)
		}
	})

	t.Run("text reply still picks and lists choices when unrecognized", func(t *testing.T) {
		h := newPickerHarness(t)
		h.open(t, "帮我整理这个需求")
		h.say(t, "om_text_bad", "99")
		if len(h.turns) != 0 || len(h.replies) != 1 {
			t.Fatalf("turns=%+v replies=%+v", h.turns, h.replies)
		}
		want := "未识别该项目。请回复序号或编号前缀选择项目：\n1. OF orca-factory\n2. ZY zhiyun-ai-dashboard\n3. AN ansible"
		if h.replies[0].content != want {
			t.Fatalf("reply=%q\nwant %q", h.replies[0].content, want)
		}
		h.say(t, "om_text_good", "ZY")
		if len(h.turns) != 1 || h.turns[0].Project.Prefix != "ZY" || h.turns[0].Prompt != "[需求人] 帮我整理这个需求" {
			t.Fatalf("turns=%+v", h.turns)
		}
		if !slices.ContainsFunc(h.replies, func(s sent) bool { return s.content == "已整理" }) {
			t.Fatalf("replies=%+v", h.replies)
		}
	})
}
