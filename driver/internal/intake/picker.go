package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// projectPick is one submission of the picker card: the thread it belongs
// to (from the button's callback value), the card message to update, who
// pressed the button, and the submitted project prefix.
type projectPick struct{ thread, card, operator, prefix string }

func (g *Gateway) parsePick(event *callback.CardActionTriggerEvent) (projectPick, bool) {
	if event == nil || event.Event == nil || event.Event.Operator == nil || event.Event.Action == nil || event.Event.Context == nil {
		return projectPick{}, false
	}
	e := event.Event
	thread, _ := e.Action.Value[pickerThreadKey].(string)
	prefix, _ := e.Action.FormValue[pickerField].(string)
	pick := projectPick{thread: thread, card: e.Context.OpenMessageID, operator: e.Operator.OpenID, prefix: prefix}
	return pick, e.Context.OpenChatID == g.chat && threadRoot.MatchString(pick.thread) && threadRoot.MatchString(pick.card) && pick.operator != ""
}

func toast(kind, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: kind, Content: content}}
}

// HandleCardAction answers a picker card submission within the callback
// deadline: it checks the pick against the thread's session and returns the
// toast, never a card (a JSON 2.0 card in a long-connection callback
// response is rejected with 200673). For an accepted pick, or any
// requester's pick once the thread already has a project, it also returns
// the work the caller runs after responding: settlePick.
func (g *Gateway) HandleCardAction(event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, func(context.Context) error) {
	pick, ok := g.parsePick(event)
	if !ok {
		return toast("error", "无法识别的卡片操作"), nil
	}
	session, resume, err := readSession(filepath.Join(g.dir, pick.thread+".json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "intake project pick failed for thread %s: %v\n", pick.thread, err)
		return toast("error", failedReply), nil
	}
	if !resume || session.State == Closed || g.now().Sub(session.LastMessage) > idleClose {
		return toast("error", "本话题已关闭"), nil
	}
	if pick.operator != session.Requester {
		return toast("error", "只有需求人可以选择项目"), nil
	}
	if session.Project != "" {
		return toast("info", selectedText(g.project(session.Project))), g.settlePick(pick)
	}
	p, found := g.findProject(pick.prefix, "")
	if !found {
		return toast("error", "未识别该项目"), nil
	}
	if !p.usable() {
		return toast("error", unavailableProjectReply(p)), nil
	}
	return toast("success", selectedText(p)), g.settlePick(pick)
}

// settlePick converges the thread on one project under its lock: the
// first accepted pick replaces the card with the selected state and opens
// the conversation with PendingPrompt; any later run (a repeated click, a
// pick after a text selection) only re-asserts the selected card.
func (g *Gateway) settlePick(pick projectPick) func(context.Context) error {
	return func(ctx context.Context) error {
		unlock := g.lockThread(pick.thread)
		defer unlock()
		path := filepath.Join(g.dir, pick.thread+".json")
		session, _, err := readSession(path)
		if err != nil {
			return err
		}
		if session.Project != "" {
			return g.updateCard(ctx, pick.card, selectedCard(g.project(session.Project)))
		}
		p, ok := g.findProject(pick.prefix, "")
		if !ok || !p.usable() {
			return nil
		}
		if err := g.updateCard(ctx, pick.card, selectedCard(p)); err != nil {
			return err
		}
		return g.beginProject(ctx, pick.thread, pick.thread, path, session, pick.operator, p, session.PendingPrompt)
	}
}

// projectPicker builds thread's picker card from the current project set,
// preselecting the group default when it is usable.
func (g *Gateway) projectPicker(thread string) string {
	var usable, unavailable []Project
	for _, p := range g.projectsSnapshot() {
		if p.usable() {
			usable = append(usable, p)
		} else {
			unavailable = append(unavailable, p)
		}
	}
	preselect := ""
	if g.defaultRemote != "" {
		if p, ok := g.projectByRemote(g.defaultRemote); ok && p.usable() {
			preselect = p.Prefix
		}
	}
	return pickerCard(thread, usable, unavailable, g.onboardableSnapshot(), preselect)
}

func selectedText(p Project) string { return "已选择 " + p.Prefix + " " + p.Name }

// The picker card's callback contract (OF-47): the submit button's callback
// value names the thread root; the form submits the chosen prefix under
// pickerField.
const (
	pickerThreadKey = "thread"
	pickerField     = "project"
)

func plainText(s string) map[string]any { return map[string]any{"tag": "plain_text", "content": s} }

func markdown(s string) map[string]any { return map[string]any{"tag": "markdown", "content": s} }

func cardJSON(title, template string, elements []any) string {
	b, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{"title": plainText(title), "template": template},
		"body":   map[string]any{"elements": elements},
	})
	return string(b)
}

// pickerCard is the JSON 2.0 card asking the requester of thread to choose a
// project: a form with a dropdown of the usable projects (preselect chosen
// when non-empty) and a submit button, then every unavailable project with
// its reason, then every onboardable repository (OF-49: known to Orca but
// never onboarded) with a fixed "需审批人接入后可用" note — read-only, no
// button, since onboarding itself needs an approver. A form plus button
// rather than submit-on-select, because a preselected default must be
// confirmable without changing the selection.
func pickerCard(thread string, usable, unavailable []Project, onboardable []Onboardable, preselect string) string {
	elements := []any{markdown("请选择本需求所属的项目，然后点“开始”。")}
	if len(usable) == 0 {
		elements[0] = markdown("暂无可接管的项目。")
	} else {
		options := make([]any, 0, len(usable))
		for _, p := range usable {
			options = append(options, map[string]any{"text": plainText(p.Prefix + " " + p.Name), "value": p.Prefix})
		}
		dropdown := map[string]any{"tag": "select_static", "name": pickerField, "required": true, "placeholder": plainText("选择项目"), "options": options}
		if preselect != "" {
			dropdown["initial_option"] = preselect
		}
		button := map[string]any{"tag": "button", "name": "start", "text": plainText("开始"), "type": "primary", "form_action_type": "submit",
			"behaviors": []any{map[string]any{"type": "callback", "value": map[string]any{pickerThreadKey: thread}}}}
		elements = append(elements, map[string]any{"tag": "form", "name": "project_picker", "elements": []any{dropdown, button}})
	}
	if len(unavailable) > 0 {
		var b strings.Builder
		b.WriteString("暂不可用：")
		for _, p := range unavailable {
			b.WriteString("\n- " + p.Prefix + " " + p.Name + "：" + unavailableReason(p))
		}
		elements = append(elements, markdown(b.String()))
	}
	if len(onboardable) > 0 {
		var b strings.Builder
		b.WriteString("可接入：")
		for _, o := range onboardable {
			b.WriteString("\n- " + o.Name + "（" + o.Remote + "）：需审批人接入后可用")
		}
		elements = append(elements, markdown(b.String()))
	}
	return cardJSON("选择项目", "blue", elements)
}

// selectedCard replaces the picker once the thread's project is fixed: no
// interactive element is left, so the card cannot be operated again.
func selectedCard(p Project) string {
	return cardJSON("选择项目", "green", []any{markdown(selectedText(p))})
}
