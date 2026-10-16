package intake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
)

const dujiao = "github.com/dujiao-next/dujiao-next"

// clickEnable replays the constructed card.action.trigger sample as a press
// of a pending-enable repository's "启用" button: action.value names the
// thread, the enable action, the repository remote, its local checkout path
// and its ledger name; there is no form.
func clickEnable(t *testing.T, g *Gateway, operator, remote, path, name string) (*callback.Toast, func(context.Context) error) {
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
	action := event["action"].(map[string]any)
	action["value"] = map[string]any{"thread": pickerRoot, "action": "enable", "remote": remote, "path": path, "name": name}
	action["name"] = "enable"
	delete(action, "form_value")
	if b, err = json.Marshal(payload); err != nil {
		t.Fatal(err)
	}
	var begin func(context.Context) error
	handler := dispatcher.NewEventDispatcher("", "").OnP2CardActionTrigger(func(_ context.Context, e *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		var response *callback.CardActionTriggerResponse
		response, begin = g.HandleCardAction(e)
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

// TestIntakeProjectPickerPendingEnable covers OF-54: discovery's
// pending-enable repositories (onboarded, check passing, still 启用=false)
// show up in the picker card as a "待启用" section naming the ledger prefix,
// name and remote, each with its own "启用" button, distinct from the
// "可接入" section (OF-49) and from "暂不可用".
func TestIntakeProjectPickerPendingEnable(t *testing.T) {
	h := newPickerHarness(t)
	h.g.SetPendingEnable([]PendingEnable{
		{Prefix: "DJ", Name: "dujiao-next", Remote: dujiao, Path: "/repo/dujiao-next"},
	})
	h.open(t, "帮我整理这个需求")
	if len(h.cards) != 1 {
		t.Fatalf("cards=%+v", h.cards)
	}
	card := h.cards[0].content
	for _, want := range []string{"待启用", "DJ dujiao-next（" + dujiao + "）", `"action":"enable"`, `"remote":"` + dujiao + `"`, `"path":"/repo/dujiao-next"`, `"content":"启用 dujiao-next"`} {
		if !strings.Contains(card, want) {
			t.Fatalf("card=%s\nwant it to contain %q", card, want)
		}
	}
	if strings.Count(card, "待启用") != 1 {
		t.Fatalf("card=%s\nwant exactly one 待启用 section", card)
	}
}

// TestIntakeEnableButton covers OF-54: only an AGENT_FEISHU_APPROVERS
// member's press enables a repository, re-validated live by the Enabler
// (merge + check) rather than trusted from the card's rendering. The thread
// gets "已启用 <前缀> <名称>，下次项目发现后接管" on success, or the
// Enabler's failure reason otherwise; a repository the enable button names
// need not be in the gateway's current pendingEnable snapshot (OF-50's
// completion card presses this before any rediscovery ever lists it there).
func TestIntakeEnableButton(t *testing.T) {
	newGateway := func(t *testing.T) (*Gateway, *[]sent, *[]EnableRequest) {
		var replies []sent
		var requests []EnableRequest
		reply := func(_ context.Context, target, content string) error {
			replies = append(replies, sent{target, content})
			return nil
		}
		g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, []string{"ou_APPROVER"}, 1, nil, "", nil, reply, noopReact, noopReply, noopReply)
		if err != nil {
			t.Fatal(err)
		}
		g.SetEnabler(func(_ context.Context, req EnableRequest) (EnableResult, error) {
			requests = append(requests, req)
			return EnableResult{Row: issues.LedgerProject{Name: "dujiao-next", Remote: dujiao, Prefix: "DJ", Enabled: true}}, nil
		})
		return g, &replies, &requests
	}

	t.Run("non-approver press is refused and starts nothing", func(t *testing.T) {
		g, replies, requests := newGateway(t)
		toast, begin := clickEnable(t, g, "ou_REDACTED", dujiao, "/repo/dujiao-next", "dujiao-next")
		if toast.Type != "error" || toast.Content != "只有审批人可以启用" || begin != nil {
			t.Fatalf("toast=%+v begin=%v", toast, begin != nil)
		}
		if len(*requests) != 0 || len(*replies) != 0 {
			t.Fatalf("requests=%+v replies=%+v", *requests, *replies)
		}
	})

	t.Run("approver press enables and reports success in the thread", func(t *testing.T) {
		g, replies, requests := newGateway(t)
		toast, begin := clickEnable(t, g, "ou_APPROVER", dujiao, "/repo/dujiao-next", "dujiao-next")
		if toast.Type != "success" || toast.Content != "正在校验 dujiao-next" {
			t.Fatalf("toast=%+v", toast)
		}
		run(t, begin)
		want := EnableRequest{Repo: "/repo/dujiao-next", Remote: dujiao, Requester: "ou_APPROVER", Thread: pickerRoot}
		if len(*requests) != 1 || (*requests)[0] != want {
			t.Fatalf("requests=%+v, want [%+v]", *requests, want)
		}
		if len(*replies) != 1 || (*replies)[0] != (sent{pickerCardMessage, "已启用 DJ dujiao-next，下次项目发现后接管"}) {
			t.Fatalf("replies=%+v", *replies)
		}
	})

	t.Run("repeated approver presses stay idempotent", func(t *testing.T) {
		g, replies, requests := newGateway(t)
		_, first := clickEnable(t, g, "ou_APPROVER", dujiao, "/repo/dujiao-next", "dujiao-next")
		_, second := clickEnable(t, g, "ou_APPROVER", dujiao, "/repo/dujiao-next", "dujiao-next")
		run(t, first)
		run(t, second)
		if len(*requests) != 2 {
			t.Fatalf("requests=%+v, want the Enabler called once per press", *requests)
		}
		for _, r := range *replies {
			if r != (sent{pickerCardMessage, "已启用 DJ dujiao-next，下次项目发现后接管"}) {
				t.Fatalf("replies=%+v", *replies)
			}
		}
	})

	t.Run("enabler failure reports the reason and nothing is written", func(t *testing.T) {
		var replies []sent
		var requests []EnableRequest
		enableErr := errors.New("接入 PR 未合并")
		reply := func(_ context.Context, target, content string) error {
			replies = append(replies, sent{target, content})
			return nil
		}
		g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, []string{"ou_APPROVER"}, 1, nil, "", nil, reply, noopReact, noopReply, noopReply)
		if err != nil {
			t.Fatal(err)
		}
		g.SetEnabler(func(_ context.Context, req EnableRequest) (EnableResult, error) {
			requests = append(requests, req)
			return EnableResult{}, enableErr
		})
		toast, begin := clickEnable(t, g, "ou_APPROVER", dujiao, "/repo/dujiao-next", "dujiao-next")
		if toast.Type != "success" {
			t.Fatalf("toast=%+v", toast)
		}
		run(t, begin)
		if len(requests) != 1 {
			t.Fatalf("requests=%+v", requests)
		}
		if len(replies) != 1 || replies[0] != (sent{pickerCardMessage, "启用 dujiao-next 失败：" + enableErr.Error()}) {
			t.Fatalf("replies=%+v", replies)
		}
	})

	t.Run("no enabler configured refuses the press", func(t *testing.T) {
		var replies []sent
		reply := func(_ context.Context, target, content string) error {
			replies = append(replies, sent{target, content})
			return nil
		}
		g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, []string{"ou_APPROVER"}, 1, nil, "", nil, reply, noopReact, noopReply, noopReply)
		if err != nil {
			t.Fatal(err)
		}
		toast, begin := clickEnable(t, g, "ou_APPROVER", dujiao, "/repo/dujiao-next", "dujiao-next")
		if toast.Type != "error" || toast.Content != "本机未开启自动启用" || begin != nil || len(replies) != 0 {
			t.Fatalf("toast=%+v begin=%v replies=%+v", toast, begin != nil, replies)
		}
	})
}
