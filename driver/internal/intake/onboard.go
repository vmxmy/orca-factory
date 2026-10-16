package intake

import (
	"context"
	"fmt"
	"slices"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
)

// OnboardRequest asks the driver to onboard one repository (OF-50): its
// local checkout and Orca canonical remote, who asked, and the topic to
// report to. Requester and Thread are empty for the onboard command.
type OnboardRequest struct {
	Repo, Remote      string
	Requester, Thread string
}

// OnboardResult is a finished onboarding: the onboarding PR opened in the
// target repository, and the 启用=false 项目 row the driver wrote.
type OnboardResult struct {
	PR  string
	Row issues.LedgerProject
}

// Onboarder runs one onboarding to completion; it refuses a repository that
// already has one in progress.
type Onboarder func(context.Context, OnboardRequest) (OnboardResult, error)

// The onboard button's callback contract (OF-50): action.value carries the
// thread root, onboardAction under onboardActionKey, and the repository's
// canonical remote under onboardRemoteKey.
const (
	onboardActionKey = "action"
	onboardAction    = "onboard"
	onboardRemoteKey = "remote"
)

// SetOnboarder enables the picker card's "接入" buttons; without one a press
// is refused.
func (g *Gateway) SetOnboarder(onboard Onboarder) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onboard = onboard
}

func isOnboardAction(event *callback.CardActionTriggerEvent) bool {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return false
	}
	action, _ := event.Event.Action.Value[onboardActionKey].(string)
	return action == onboardAction
}

// handleOnboardAction answers an onboard button press within the callback
// deadline. Only an approver's press for a repository still in the current
// onboardable set returns the deferred onboarding run.
func (g *Gateway) handleOnboardAction(event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, func(context.Context) error) {
	e := event.Event
	if e.Operator == nil || e.Context == nil {
		return toast("error", "无法识别的卡片操作"), nil
	}
	thread, _ := e.Action.Value[pickerThreadKey].(string)
	remote, _ := e.Action.Value[onboardRemoteKey].(string)
	card, operator := e.Context.OpenMessageID, e.Operator.OpenID
	if e.Context.OpenChatID != g.chat || !threadRoot.MatchString(thread) || !threadRoot.MatchString(card) || operator == "" {
		return toast("error", "无法识别的卡片操作"), nil
	}
	if !slices.Contains(g.approvers, operator) {
		return toast("error", "只有审批人可以接入"), nil
	}
	g.mu.Lock()
	onboard := g.onboard
	g.mu.Unlock()
	if onboard == nil {
		return toast("error", "本机未开启自动接入"), nil
	}
	onboardable := g.onboardableSnapshot()
	i := slices.IndexFunc(onboardable, func(o Onboardable) bool { return o.Remote == remote })
	if i < 0 {
		return toast("error", "该仓库当前不可接入"), nil
	}
	repo := onboardable[i]
	started := "开始接入 " + repo.Name
	return toast("success", started), func(ctx context.Context) error {
		if err := g.reply(ctx, card, started); err != nil {
			return err
		}
		result, err := onboard(ctx, OnboardRequest{Repo: repo.Path, Remote: repo.Remote, Requester: operator, Thread: thread})
		if err != nil {
			return g.reply(ctx, card, fmt.Sprintf("接入 %s 失败：%v", repo.Name, err))
		}
		return g.reply(ctx, card, fmt.Sprintf("接入 PR：%s\n项目表行：%s %s（启用=false）\n合并 PR 并在项目表勾选启用后生效", result.PR, result.Row.Prefix, result.Row.Name))
	}
}
