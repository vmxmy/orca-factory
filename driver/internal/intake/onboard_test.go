package intake

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
)

const ziikoo = "github.com/vmxmy/ziikoo-control-plane"

// clickOnboard replays the constructed card.action.trigger sample as a press
// of an onboardable repository's "接入" button: action.value names the
// thread, the onboard action and the repository remote; there is no form.
func clickOnboard(t *testing.T, g *Gateway, operator, remote string) (*callback.Toast, func(context.Context) error) {
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
	action["value"] = map[string]any{"thread": pickerRoot, "action": "onboard", "remote": remote}
	action["name"] = "onboard"
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

// TestIntakeOnboardButton covers OF-50: the picker card's 可接入 section
// carries one "接入" button per repository; only an AGENT_FEISHU_APPROVERS
// member's press starts onboarding, with the repository's local checkout,
// remote, the approver and the thread as the request. The thread first gets
// "开始接入 <仓库>", then a completion card (OF-54) carrying the PR link, the
// 启用=false row, and the same "启用" button handleEnableAction answers.
func TestIntakeOnboardButton(t *testing.T) {
	newGateway := func(t *testing.T) (*Gateway, *[]sent, *[]sent, *[]OnboardRequest) {
		var replies, cards []sent
		var requests []OnboardRequest
		reply := func(_ context.Context, target, content string) error {
			replies = append(replies, sent{target, content})
			return nil
		}
		replyCard := func(_ context.Context, target, content string) error {
			cards = append(cards, sent{target, content})
			return nil
		}
		g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, []string{"ou_APPROVER"}, 1, nil, "", nil, reply, noopReact, replyCard, noopReply)
		if err != nil {
			t.Fatal(err)
		}
		g.SetOnboardable([]Onboardable{{Name: "ziikoo-control-plane", Remote: ziikoo, Path: "/repo/ziikoo-control-plane"}})
		g.SetOnboarder(func(_ context.Context, req OnboardRequest) (OnboardResult, error) {
			requests = append(requests, req)
			return OnboardResult{PR: "https://github.com/vmxmy/ziikoo-control-plane/pull/7", Row: issues.LedgerProject{Name: "ziikoo-control-plane", Remote: ziikoo, Prefix: "ZCP"}}, nil
		})
		return g, &replies, &cards, &requests
	}

	t.Run("picker card has one 接入 button per onboardable repository", func(t *testing.T) {
		g, _, _, _ := newGateway(t)
		card := g.projectPicker(pickerRoot)
		for _, want := range []string{`"tag":"button"`, `"action":"onboard"`, `"remote":"` + ziikoo + `"`, `"content":"接入 ziikoo-control-plane"`} {
			if !strings.Contains(card, want) {
				t.Fatalf("card=%s\nwant it to contain %s", card, want)
			}
		}
	})

	t.Run("non-approver press is refused and starts nothing", func(t *testing.T) {
		g, replies, _, requests := newGateway(t)
		toast, begin := clickOnboard(t, g, "ou_REDACTED", ziikoo)
		if toast.Type != "error" || toast.Content != "只有审批人可以接入" || begin != nil {
			t.Fatalf("toast=%+v begin=%v", toast, begin != nil)
		}
		if len(*requests) != 0 || len(*replies) != 0 {
			t.Fatalf("requests=%+v replies=%+v", *requests, *replies)
		}
	})

	t.Run("press for a repository no longer onboardable is refused", func(t *testing.T) {
		g, _, _, requests := newGateway(t)
		toast, begin := clickOnboard(t, g, "ou_APPROVER", "github.com/vmxmy/orca-factory")
		if toast.Type != "error" || toast.Content != "该仓库当前不可接入" || begin != nil || len(*requests) != 0 {
			t.Fatalf("toast=%+v begin=%v requests=%+v", toast, begin != nil, *requests)
		}
	})

	t.Run("approver press starts onboarding and reports the PR card in the thread", func(t *testing.T) {
		g, replies, cards, requests := newGateway(t)
		toast, begin := clickOnboard(t, g, "ou_APPROVER", ziikoo)
		if toast.Type != "success" || toast.Content != "开始接入 ziikoo-control-plane" {
			t.Fatalf("toast=%+v", toast)
		}
		run(t, begin)
		want := OnboardRequest{Repo: "/repo/ziikoo-control-plane", Remote: ziikoo, Requester: "ou_APPROVER", Thread: pickerRoot}
		if len(*requests) != 1 || (*requests)[0] != want {
			t.Fatalf("requests=%+v, want [%+v]", *requests, want)
		}
		if len(*replies) != 1 || (*replies)[0] != (sent{pickerCardMessage, "开始接入 ziikoo-control-plane"}) {
			t.Fatalf("replies=%+v", *replies)
		}
		if len(*cards) != 1 {
			t.Fatalf("cards=%+v", *cards)
		}
		done := (*cards)[0]
		for _, part := range []string{"https://github.com/vmxmy/ziikoo-control-plane/pull/7", "ZCP", `"action":"enable"`, `"remote":"` + ziikoo + `"`, `"path":"/repo/ziikoo-control-plane"`, `"content":"启用 ziikoo-control-plane"`} {
			if done.target != pickerCardMessage || !strings.Contains(done.content, part) {
				t.Fatalf("completion card=%+v, want it to contain %q", done, part)
			}
		}
	})
}
