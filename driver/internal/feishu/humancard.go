package feishu

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The human-ticket card's callback contract, shared by the scheduler that
// sends the card and the gateway that receives the press.
const (
	HumanActionKey  = "action"
	HumanAction     = "human_review"
	HumanVerbKey    = "verb"
	HumanTicketKey  = "ticket"
	HumanAttemptKey = "attempt"
	HumanNoteField  = "note"

	VerbRelease = "放行"
	VerbRuling  = "裁定"
	VerbScope   = "扩SCOPE"
	VerbCancel  = "取消"
)

// HumanCard is the decision card for a ticket handed to a human. Attempt ties
// a press to the escalation the card was sent for.
type HumanCard struct {
	Ticket, Title, Reason, URL string
	Attempt                    int
	Scope                      []string
	AutoReviewRefusal          string
}

func plainDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "plain_text", "content": content}}
}

func buttonText(content string) map[string]any {
	return map[string]any{"tag": "plain_text", "content": content}
}

func cardBody(title, template string, elements []any) string {
	b, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{"title": buttonText(title), "template": template},
		"body":   map[string]any{"elements": elements},
	})
	return string(b)
}

func (c HumanCard) button(name, label, verb, style string) map[string]any {
	return map[string]any{"tag": "button", "name": name, "text": buttonText(label), "type": style, "form_action_type": "submit",
		"behaviors": []any{map[string]any{"type": "callback", "value": map[string]any{
			HumanActionKey: HumanAction, HumanVerbKey: verb, HumanTicketKey: c.Ticket, HumanAttemptKey: strconv.Itoa(c.Attempt),
		}}}}
}

// JSON renders the card. Reason, title and paths are agent or ledger text, so
// they stay plain_text and cannot add links or mentions.
func (c HumanCard) JSON() string {
	elements := []any{plainDiv(c.Title), plainDiv("原因：" + c.Reason)}
	if c.AutoReviewRefusal != "" {
		elements = append(elements, plainDiv("未自动批准："+c.AutoReviewRefusal))
	}
	form := []any{
		map[string]any{"tag": "input", "name": HumanNoteField, "required": false, "placeholder": buttonText("裁定或取消时填写理由")},
		c.button("human_release", "放行", VerbRelease, "primary"),
	}
	if len(c.Scope) > 0 {
		elements = append(elements, plainDiv("申请扩 SCOPE："+strings.Join(c.Scope, " ")))
		form = append(form, c.button("human_scope", "同意扩 SCOPE", VerbScope, "default"))
	}
	form = append(form, c.button("human_ruling", "裁定", VerbRuling, "default"), c.button("human_cancel", "取消票", VerbCancel, "danger"))
	elements = append(elements, map[string]any{"tag": "form", "name": "human_review_form", "elements": form})
	if c.URL != "" {
		elements = append(elements, map[string]any{"tag": "button", "text": buttonText("打开票据"), "type": "default",
			"behaviors": []any{map[string]any{"type": "open_url", "default_url": c.URL}}})
	}
	elements = append(elements, plainDiv(fmt.Sprintf("也可发文字：@机器人 放行 %[1]s、裁定 %[1]s <理由>、扩SCOPE %[1]s <路径...>、取消 %[1]s <理由>", c.Ticket)))
	return cardBody("转人 "+c.Ticket, "orange", elements)
}

// SettledHumanCard replaces the decision card once the ledger holds the
// decision, so no button remains to press.
func SettledHumanCard(ticket, title, verb, operator, detail string) string {
	elements := []any{plainDiv(title), map[string]any{"tag": "markdown", "content": fmt.Sprintf("已由 <at id=%s></at> %s", operator, verb)}}
	if detail != "" {
		elements = append(elements, plainDiv(detail))
	}
	template := "green"
	if verb == VerbCancel {
		template = "grey"
	}
	return cardBody(ticket+" 已"+verb, template, elements)
}
