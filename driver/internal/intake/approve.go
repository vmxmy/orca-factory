package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

const (
	readyForAgent   = "ready-for-agent"
	noApprovalReply = "无审批权限"
	lane3Reply      = "lane 3 需人工实现，不放行"
)

var (
	reviewCommand = regexp.MustCompile(`(?s)^(批准|驳回|放行|取消)\s+([A-Z][A-Z0-9]*)-([0-9]+)(?:\s+(\S.*))?$`)
	laneHeading   = regexp.MustCompile(`(?m)^## LANE 预判\s*\n\s*([0-9])`)
)

// review carries intake approval/rejection or human ticket release/cancellation.
type review struct {
	approve        bool
	human          bool
	prefix, ticket string
	reason         string
}

func (r review) verb() string {
	if r.human {
		if r.approve {
			return "放行"
		}
		return "取消"
	}
	if r.approve {
		return "批准"
	}
	return "驳回"
}

// parseReview matches the whole prompt: approval/release takes no trailing
// text and rejection/cancellation requires a reason.
func parseReview(prompt string) (review, bool) {
	m := reviewCommand.FindStringSubmatch(prompt)
	if m == nil {
		return review{}, false
	}
	r := review{approve: m[1] == "批准" || m[1] == "放行", human: m[1] == "放行" || m[1] == "取消", prefix: m[2], ticket: m[2] + "-" + m[3], reason: strings.TrimSpace(m[4])}
	if r.approve == (r.reason != "") {
		return review{}, false
	}
	return r, true
}

// check decides from the ledger's current record whether r may apply. done
// means an earlier identical command already applied it; refusal, when not
// empty, is the reason it may not.
func (r review) check(t issues.Task) (refusal string, done bool) {
	if _, _, ok := intakeMarker(t.Description); !ok {
		return fmt.Sprintf("%s 不是需求助手提交的票，不处理", r.ticket), false
	}
	ready := slices.ContainsFunc(t.Labels.Nodes, func(l task.NamedID) bool { return l.Name == readyForAgent })
	state := t.Metadata.State
	switch {
	case r.approve && ready:
		return "", true
	case !r.approve && state == "Canceled":
		return "", true
	case !r.approve && ready:
		return fmt.Sprintf("%s 已放行，不能驳回", r.ticket), false
	case state != "Todo":
		return fmt.Sprintf("%s 状态为 %s，只有 Todo 可以%s", r.ticket, state, r.verb()), false
	}
	if !r.approve {
		return "", false
	}
	m := laneHeading.FindStringSubmatch(t.Description)
	switch {
	case m == nil:
		return fmt.Sprintf("%s 票面缺少 LANE 预判，不放行", r.ticket), false
	case m[1] == "3":
		return lane3Reply, false
	}
	return "", false
}

// outcome is what the command does to the ticket, for replies.
func (r review) outcome() string {
	if r.approve {
		return "放行"
	}
	return "驳回"
}

func (r review) notice(requester string) string {
	if r.approve {
		return fmt.Sprintf(`<at user_id="%s"></at> %s 已由审批人放行，进入派工队列`, requester, r.ticket)
	}
	return fmt.Sprintf(`<at user_id="%s"></at> %s 已被审批人驳回，理由：%s`, requester, r.ticket, r.reason)
}

// handleReview applies an approver's command through the ticket project's
// ledger and notifies the requester in the thread the ticket came from. The
// event is written before the label or state change and skipped when already
// present, so a retry after a crash between the two writes converges without
// a second event.
func (g *Gateway) handleReview(ctx context.Context, id, sender string, r review) error {
	if !slices.Contains(g.approvers, sender) {
		return g.reply(ctx, id, noApprovalReply)
	}
	g.addReceipt(ctx, id)
	p := g.project(r.prefix)
	if p.Ledger == nil {
		return g.reply(ctx, id, fmt.Sprintf("未找到项目 %s 的账本", r.prefix))
	}
	unlock := g.lockThread("review/" + r.ticket)
	defer unlock()
	t, err := p.Ledger.Read(ctx, r.ticket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "intake review read %s failed: %v\n", r.ticket, err)
		return g.reply(ctx, id, fmt.Sprintf("账本中读取 %s 失败", r.ticket))
	}
	if r.human {
		return g.handleHumanReview(ctx, id, sender, r, p.Ledger, t)
	}
	refusal, done := r.check(t)
	if done {
		return g.reply(ctx, id, r.ticket+" 已"+r.outcome())
	}
	if refusal != "" {
		return g.reply(ctx, id, refusal)
	}
	if err := g.applyReview(ctx, p.Ledger, t.Metadata.ID, sender, r); err != nil {
		fmt.Fprintf(os.Stderr, "intake review %s %s failed: %v\n", r.verb(), r.ticket, err)
		return g.reply(ctx, id, failedReply)
	}
	if err := g.reply(ctx, id, "已"+r.outcome()+" "+r.ticket); err != nil {
		return err
	}
	return g.notifyRequester(ctx, t.Description, r)
}

func latestHumanEvidence(comments []task.Comment) map[string]string {
	rows := slices.Clone(comments)
	sort.SliceStable(rows, func(first, second int) bool {
		return rows[first].CreatedAt.Before(rows[second].CreatedAt)
	})
	latest := map[string]string{}
	for _, comment := range rows {
		for _, line := range strings.Split(comment.Body, "\n") {
			for _, prefix := range []string{resolver.Prefix, merge.HandoffPrefix, merge.ProgressPrefix} {
				if raw, ok := strings.CutPrefix(line, prefix); ok {
					latest[prefix] = raw
				}
			}
		}
	}
	return latest
}

func humanReleaseRefusal(ticket string, description string, latest map[string]string) string {
	if raw, ok := latest[merge.HandoffPrefix]; ok {
		var handoff merge.Handoff
		if json.Unmarshal([]byte(raw), &handoff) != nil || handoff.Version != 1 || handoff.Lane < 1 || handoff.Lane > 3 {
			return ticket + " 证据冲突：merge-handoff-v1 解析失败"
		}
		if handoff.Lane == 3 {
			return lane3Reply
		}
	}
	if raw, ok := latest[merge.ProgressPrefix]; ok {
		var progress merge.Progress
		if json.Unmarshal([]byte(raw), &progress) != nil || progress.Version != 1 {
			return ticket + " 证据冲突：merge-progress-v1 解析失败"
		}
		if progress.Deployment != nil && progress.Deployment.State == "paused" {
			return ticket + " 部署屏障处于 paused，请走 resume-deployment"
		}
	}
	lane := laneHeading.FindStringSubmatch(description)
	if lane == nil {
		return ticket + " 票面缺少 LANE 预判，不放行"
	}
	if lane[1] == "3" {
		return lane3Reply
	}
	return ""
}

func (g *Gateway) handleHumanReview(ctx context.Context, id, sender string, r review, ledger Ledger, ticket issues.Task) error {
	if ticket.Metadata.State == "Canceled" {
		return g.reply(ctx, id, r.ticket+" 当前状态为 Canceled")
	}
	comments, err := ledger.ListComments(ctx, ticket.Metadata.ID)
	if err != nil {
		return g.reply(ctx, id, r.ticket+" 读取评论失败，请重试")
	}
	latest := latestHumanEvidence(comments)
	raw, ok := latest[resolver.Prefix]
	if !ok {
		return g.reply(ctx, id, r.ticket+" 当前没有 resolver-state，不处于 human 状态")
	}
	var record resolver.Record
	if json.Unmarshal([]byte(raw), &record) != nil || record.Version != 1 || record.Attempt < 1 || record.Phase == "" {
		return g.reply(ctx, id, r.ticket+" 证据冲突：resolver-state 解析失败")
	}
	if record.Phase != "human" {
		return g.reply(ctx, id, fmt.Sprintf("%s 当前 resolver-state phase 为 %s，只有 human 可以%s", r.ticket, record.Phase, r.verb()))
	}
	if r.approve {
		if refusal := humanReleaseRefusal(r.ticket, ticket.Description, latest); refusal != "" {
			return g.reply(ctx, id, refusal)
		}
	}
	body := fmt.Sprintf("%s：%s %s", r.verb(), sender, g.now().Format(time.RFC3339))
	if r.approve {
		body += fmt.Sprintf("\n%s{\"version\":1,\"attempt\":%d,\"phase\":\"waiting\"}", resolver.Prefix, record.Attempt)
	} else {
		body += "\n理由：" + strings.Join(strings.Fields(r.reason), " ")
	}
	if r.approve || !slices.ContainsFunc(comments, func(comment task.Comment) bool { return strings.HasPrefix(comment.Body, "取消：") }) {
		if _, err := ledger.CreateComment(ctx, ticket.Metadata.ID, body); err != nil {
			return g.reply(ctx, id, failedReply)
		}
	}
	if !r.approve {
		if _, err := ledger.UpdateIssue(ctx, ticket.Metadata.ID, "Canceled", nil, nil); err != nil {
			return g.reply(ctx, id, failedReply)
		}
	}
	return g.reply(ctx, id, "已"+r.verb()+" "+r.ticket)
}

func (g *Gateway) applyReview(ctx context.Context, ledger Ledger, ticket, sender string, r review) error {
	prefix := r.verb() + "："
	events, err := ledger.ListComments(ctx, ticket)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(events, func(c task.Comment) bool { return strings.HasPrefix(c.Body, prefix) }) {
		body := fmt.Sprintf("%s%s %s", prefix, sender, g.now().Format(time.RFC3339))
		if !r.approve {
			body += "\n理由：" + r.reason
		}
		if _, err := ledger.CreateComment(ctx, ticket, body); err != nil {
			return err
		}
	}
	if r.approve {
		_, err = ledger.UpdateIssue(ctx, ticket, "", []string{readyForAgent}, nil)
	} else {
		_, err = ledger.UpdateIssue(ctx, ticket, "Canceled", nil, nil)
	}
	return err
}

// notifyRequester replies in the ticket's origin thread, @-mentioning the
// requester this gateway recorded for that thread.
func (g *Gateway) notifyRequester(ctx context.Context, description string, r review) error {
	thread, _, _ := intakeMarker(description)
	if !threadRoot.MatchString(thread) {
		return fmt.Errorf("intake ticket %s marker names invalid thread", r.ticket)
	}
	session, ok, err := readSession(filepath.Join(g.dir, thread+".json"))
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "intake ticket %s thread %s has no session; requester not notified\n", r.ticket, thread)
		return nil
	}
	return g.reply(ctx, thread, r.notice(session.Requester))
}
