package intake

import (
	"context"
	"fmt"
	"slices"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
)

// EnableRequest asks the driver to enable one onboarded-but-disabled
// repository (OF-54): its local checkout and Orca canonical remote, who
// asked, and the topic to report to.
type EnableRequest struct {
	Repo, Remote      string
	Requester, Thread string
}

// EnableResult is a finished enabling: the ledger 项目 row, now 启用=true,
// plus Cleanup (OF-57), a note on the onboarding scene the enable also tried
// to remove — "" when fully clean, otherwise what is left and why (a
// cleanup failure never fails the enable itself).
type EnableResult struct {
	Row     issues.LedgerProject
	Cleanup string
}

// Enabler re-validates one repository live (the onboarding PR's merge into
// origin/main, and its own pipeline check) and, only when both still hold,
// flips its ledger row to 启用=true. A card's rendered "待启用" set, or
// OF-50's onboarding completion reply, can be stale by the time it is
// pressed, so this never trusts that rendering alone.
type Enabler func(context.Context, EnableRequest) (EnableResult, error)

// The enable button's callback contract (OF-54): action.value carries the
// thread root, enableAction under onboardActionKey (shared with the onboard
// button, OF-50), and the repository's canonical remote, local checkout path
// and ledger name under enableRemoteKey/enablePathKey/enableNameKey.
const (
	enableAction  = "enable"
	enablePathKey = "path"
	enableNameKey = "name"
)

// SetEnabler enables the picker card's, and OF-50's onboarding completion
// reply's, "启用" buttons; without one a press is refused.
func (g *Gateway) SetEnabler(enable Enabler) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.enable = enable
}

func isEnableAction(event *callback.CardActionTriggerEvent) bool {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return false
	}
	action, _ := event.Event.Action.Value[onboardActionKey].(string)
	return action == enableAction
}

// enableButton builds one "启用" button for repo: the picker card's
// "待启用" section (OF-54) and OF-50's onboarding completion reply share this
// shape, so a press carries everything handleEnableAction needs without
// looking anything up in the gateway's current pendingEnable snapshot (which
// a newly-onboarded repository, not yet discovered as pending-enable, would
// not be in).
func enableButton(thread, name, remote, path string) map[string]any {
	value := map[string]any{pickerThreadKey: thread, onboardActionKey: enableAction, onboardRemoteKey: remote, enableNameKey: name, enablePathKey: path}
	return map[string]any{"tag": "button", "text": plainText("启用 " + name), "type": "default",
		"behaviors": []any{map[string]any{"type": "callback", "value": value}}}
}

// handleEnableAction answers an enable button press within the callback
// deadline. Only an approver's press returns the deferred work: the live
// merge/check re-validation and the ledger write happen there, after the
// toast, exactly like onboarding (OF-50) defers its agent run.
func (g *Gateway) handleEnableAction(event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, func(context.Context) error) {
	e := event.Event
	if e.Operator == nil || e.Context == nil {
		return toast("error", "无法识别的卡片操作"), nil
	}
	thread, _ := e.Action.Value[pickerThreadKey].(string)
	remote, _ := e.Action.Value[onboardRemoteKey].(string)
	path, _ := e.Action.Value[enablePathKey].(string)
	name, _ := e.Action.Value[enableNameKey].(string)
	card, operator := e.Context.OpenMessageID, e.Operator.OpenID
	if e.Context.OpenChatID != g.chat || !threadRoot.MatchString(thread) || !threadRoot.MatchString(card) || operator == "" || remote == "" || path == "" {
		return toast("error", "无法识别的卡片操作"), nil
	}
	if !slices.Contains(g.approvers, operator) {
		return toast("error", "只有审批人可以启用"), nil
	}
	g.mu.Lock()
	enable := g.enable
	g.mu.Unlock()
	if enable == nil {
		return toast("error", "本机未开启自动启用"), nil
	}
	return toast("success", "正在校验 "+name), func(ctx context.Context) error {
		result, err := enable(ctx, EnableRequest{Repo: path, Remote: remote, Requester: operator, Thread: thread})
		if err != nil {
			return g.reply(ctx, card, fmt.Sprintf("启用 %s 失败：%v", name, err))
		}
		msg := fmt.Sprintf("已启用 %s %s，下次项目发现后接管", result.Row.Prefix, result.Row.Name)
		if result.Cleanup != "" {
			msg += "；" + result.Cleanup
		}
		return g.reply(ctx, card, msg)
	}
}
