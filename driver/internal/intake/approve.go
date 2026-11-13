package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
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
	reviewCommand = regexp.MustCompile(`(?s)^(批准|驳回|放行|取消|裁定|扩SCOPE)\s+([A-Z][A-Z0-9]*)-([0-9]+)(?:\s+(\S.*))?$`)
	laneHeading   = regexp.MustCompile(`(?m)^## LANE 预判\s*\n\s*([0-9])`)
	scopeHeading  = regexp.MustCompile(`(?m)^## SCOPE[^\n]*\n`)
	nextHeading   = regexp.MustCompile(`(?m)^## `)
	literalPath   = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_./@+-]*$`)
	ticketID      = regexp.MustCompile(`^([A-Z][A-Z0-9]*)-[0-9]+$`)

	// autoReleaseForbiddenReasonPhrases are reasons that require an explicit
	// human release because they can imply authorization, credentials, or
	// irreversible production/data changes.
	autoReleaseForbiddenReasonPhrases = [...]string{"需要授权", "凭证", "不可逆", "生产 DDL", "数据"}
)

// review carries intake approval/rejection or a decision on a human ticket.
// attempt is set for a card press, tying it to the escalation the card showed.
type review struct {
	verb           string
	approve        bool
	human          bool
	prefix, ticket string
	reason         string
	attempt        int
}

func newReview(verb, prefix, ticket, reason string) review {
	return review{
		verb:    verb,
		approve: verb == "批准" || verb == feishu.VerbRelease,
		human:   verb != "批准" && verb != "驳回",
		prefix:  prefix, ticket: ticket, reason: reason,
	}
}

// parseReview matches the whole prompt: approval/release takes no trailing
// text; rejection, cancellation and ruling require a reason, and a scope
// extension requires its paths.
func parseReview(prompt string) (review, bool) {
	m := reviewCommand.FindStringSubmatch(prompt)
	if m == nil {
		return review{}, false
	}
	r := newReview(m[1], m[2], m[2]+"-"+m[3], strings.TrimSpace(m[4]))
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
		return fmt.Sprintf("%s 状态为 %s，只有 Todo 可以%s", r.ticket, state, r.verb), false
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
		reply, _ := g.applyHumanReview(ctx, sender, r, p, t)
		return g.reply(ctx, id, reply)
	}
	refusal, done := r.check(t)
	if done {
		return g.reply(ctx, id, r.ticket+" 已"+r.outcome())
	}
	if refusal != "" {
		return g.reply(ctx, id, refusal)
	}
	if err := g.applyReview(ctx, p.Ledger, t.Metadata.ID, sender, r); err != nil {
		fmt.Fprintf(os.Stderr, "intake review %s %s failed: %v\n", r.verb, r.ticket, err)
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

func autoReleaseReasonRefusal(reason string) string {
	for _, phrase := range autoReleaseForbiddenReasonPhrases {
		if strings.Contains(reason, phrase) {
			return "转人原因含禁止字样：" + phrase
		}
	}
	return ""
}

func applyHumanRelease(ctx context.Context, ledger ScopeLedger, ticket issues.Task, record resolver.Record, sender string, now time.Time) error {
	body := fmt.Sprintf("放行：%s %s\n%s{\"version\":1,\"attempt\":%d,\"phase\":\"waiting\"}", sender, now.Format(time.RFC3339), resolver.Prefix, record.Attempt)
	_, err := ledger.CreateComment(ctx, ticket.Metadata.ID, body)
	return err
}

// scopeAddition resolves the paths a scope extension adds: the command's own,
// or for a card press the list the latest escalation declared. refusal, when
// not empty, is why they may not be added.
func scopeAddition(r review, record resolver.Record, lane3Globs string) (paths []string, refusal string) {
	paths = strings.Fields(r.reason)
	if len(paths) == 0 {
		if decision, err := resolver.ParseResult(record.Result); err == nil {
			paths = decision.ScopeRequest()
		}
	}
	if len(paths) == 0 {
		return nil, r.ticket + " 处置 Agent 没有申请 SCOPE，请发 扩SCOPE " + r.ticket + " <路径...>"
	}
	return validateScopeAddition(paths, lane3Globs)
}

func validateScopeAddition(paths []string, lane3Globs string) ([]string, string) {
	for _, p := range paths {
		segments := strings.Split(p, "/")
		if !literalPath.MatchString(p) || slices.Contains(segments, "") || slices.Contains(segments, ".") || slices.Contains(segments, "..") {
			return nil, fmt.Sprintf("路径 %s 不是仓库内的字面路径，不放行", p)
		}
		for _, glob := range strings.Fields(lane3Globs) {
			if task.ScopeMatch(glob, p) {
				return nil, fmt.Sprintf("路径 %s 命中 lane 3，不放行", p)
			}
		}
	}
	return paths, ""
}

// ExtendWorkerScope approves only same-package Go tests and README files beside
// literal Go sources already in scope. A refusal leaves the whole request untouched.
// If the ticket scope already contains the paths, no comment or edit is written.
// Otherwise the audit precedes the edit; retrying a failed edit writes the same audit again.
func ExtendWorkerScope(ctx context.Context, ledger ScopeLedger, id, scope string, additions []string, lane3Globs string) (bool, error) {
	paths, refusal := validateScopeAddition(additions, lane3Globs)
	if refusal != "" || len(paths) == 0 {
		return false, nil
	}
	var audit []string
	for _, file := range paths {
		category := safeWorkerScopeFile(file, strings.Fields(scope))
		if category == "" {
			return false, nil
		}
		audit = append(audit, "自动扩 SCOPE："+file+"，依据："+category)
	}
	ticket, err := ledger.Read(ctx, id)
	if err != nil {
		return false, err
	}
	if ticket.Metadata.State == "Canceled" {
		return false, nil
	}
	if lane := laneHeading.FindStringSubmatch(ticket.Description); len(lane) > 1 && lane[1] == "3" {
		return false, nil
	}
	face, changed, found := withScope(ticket.Description, paths)
	if !found {
		return false, nil
	}
	if changed {
		if _, err := ledger.CreateComment(ctx, id, strings.Join(audit, "\n")); err != nil {
			return false, err
		}
		patch, err := json.Marshal(map[string]string{"description": face})
		if err != nil {
			return false, err
		}
		if _, err := ledger.Edit(ctx, id, patch); err != nil {
			return false, err
		}
	}
	return true, nil
}

func safeWorkerScopeFile(file string, scope []string) string {
	for _, segment := range strings.Split(strings.ToLower(file), "/") {
		if strings.HasPrefix(segment, ".env") || strings.Contains(segment, "secret") || strings.Contains(segment, "credential") {
			return ""
		}
		switch segment {
		case ".github", "agent-gates", "deploy", "prod", "production", "migrations", "data":
			return ""
		}
	}
	category := ""
	switch {
	case strings.HasSuffix(file, "_test.go"):
		category = "同包 Go 测试"
	case path.Base(file) == "README.md":
		category = "同包 README"
	default:
		return ""
	}
	for _, source := range scope {
		if _, refusal := validateScopeAddition([]string{source}, ""); refusal == "" &&
			strings.HasSuffix(source, ".go") && !strings.HasSuffix(source, "_test.go") && path.Dir(source) == path.Dir(file) {
			return category
		}
	}
	return ""
}

// withScope merges paths into the ticket face's SCOPE section. Paths an
// existing pattern already covers are not added, so a retry changes nothing.
func withScope(description string, paths []string) (face string, changed, ok bool) {
	heading := scopeHeading.FindStringIndex(description)
	if heading == nil {
		return description, false, false
	}
	end := len(description)
	if next := nextHeading.FindStringIndex(description[heading[1]:]); next != nil {
		end = heading[1] + next[0]
	}
	section := description[heading[1]:end]
	scope := strings.Fields(section)
	for _, p := range paths {
		if !slices.ContainsFunc(scope, func(pattern string) bool { return task.ScopeMatch(pattern, p) }) {
			scope = append(scope, p)
			changed = true
		}
	}
	trailing := section[len(strings.TrimRight(section, " \t\r\n")):]
	return description[:heading[1]] + strings.Join(scope, " ") + trailing + description[end:], changed, true
}

// applyHumanReview writes a decision on a human ticket and returns the reply
// and whether the decision was applied. A scope extension edits the face
// before its event, so a retry after a crash between the two converges.
func (g *Gateway) applyHumanReview(ctx context.Context, sender string, r review, p Project, ticket issues.Task) (string, bool) {
	if ticket.Metadata.State == "Canceled" {
		return r.ticket + " 当前状态为 Canceled", false
	}
	comments, err := p.Ledger.ListComments(ctx, ticket.Metadata.ID)
	if err != nil {
		return r.ticket + " 读取评论失败，请重试", false
	}
	latest := latestHumanEvidence(comments)
	raw, ok := latest[resolver.Prefix]
	if !ok {
		return r.ticket + " 当前没有 resolver-state，不处于 human 状态", false
	}
	var record resolver.Record
	if json.Unmarshal([]byte(raw), &record) != nil || record.Version != 1 || record.Attempt < 1 || record.Phase == "" {
		return r.ticket + " 证据冲突：resolver-state 解析失败", false
	}
	if r.attempt != 0 && r.attempt != record.Attempt {
		return r.ticket + " 这张卡片已过期，请看最新通知", false
	}
	if record.Phase != "human" {
		return fmt.Sprintf("%s 当前 resolver-state phase 为 %s，只有 human 可以%s", r.ticket, record.Phase, r.verb), false
	}
	if r.verb != feishu.VerbCancel {
		if refusal := humanReleaseRefusal(r.ticket, ticket.Description, latest); refusal != "" {
			return refusal, false
		}
	}
	if r.verb == feishu.VerbRelease {
		if err := applyHumanRelease(ctx, p.Ledger, ticket, record, sender, g.now()); err != nil {
			return failedReply, false
		}
		return "已放行 " + r.ticket, true
	}
	if r.verb == feishu.VerbRuling {
		if err := applyRuling(ctx, p.Ledger, ticket, record, sender, r.reason, g.now()); err != nil {
			return failedReply, false
		}
		return "已裁定 " + r.ticket, true
	}
	body := fmt.Sprintf("%s：%s %s", r.verb, sender, g.now().Format(time.RFC3339))
	note := strings.Join(strings.Fields(r.reason), " ")
	switch r.verb {
	case feishu.VerbCancel:
		body += "\n理由：" + note
	case feishu.VerbScope:
		paths, refusal := scopeAddition(r, record, p.Lane3Globs)
		if refusal != "" {
			return refusal, false
		}
		refusal, err := applyScopeReview(ctx, p.Ledger, ticket, record, paths, body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "intake scope review %s failed: %v\n", r.ticket, err)
			return failedReply, false
		}
		if refusal != "" {
			return r.ticket + " " + refusal, false
		}
		return "已" + r.verb + " " + r.ticket, true
	}
	if r.verb != feishu.VerbCancel {
		body += fmt.Sprintf("\n%s{\"version\":1,\"attempt\":%d,\"phase\":\"waiting\"}", resolver.Prefix, record.Attempt)
	}
	if r.verb != feishu.VerbCancel || !slices.ContainsFunc(comments, func(comment task.Comment) bool { return strings.HasPrefix(comment.Body, "取消：") }) {
		if _, err := p.Ledger.CreateComment(ctx, ticket.Metadata.ID, body); err != nil {
			return failedReply, false
		}
	}
	if r.verb == feishu.VerbCancel {
		if _, err := p.Ledger.UpdateIssue(ctx, ticket.Metadata.ID, "Canceled", nil, nil); err != nil {
			return failedReply, false
		}
	}
	return "已" + r.verb + " " + r.ticket, true
}

func isHumanAction(event *callback.CardActionTriggerEvent) bool {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return false
	}
	action, _ := event.Event.Action.Value[feishu.HumanActionKey].(string)
	return action == feishu.HumanAction
}

// parseHumanAction reads a press on the human-ticket card. The chat must be
// one the gateway serves.
func (g *Gateway) parseHumanAction(event *callback.CardActionTriggerEvent) (review, bool) {
	e := event.Event
	if e.Operator == nil || e.Operator.OpenID == "" || e.Context == nil || !threadRoot.MatchString(e.Context.OpenMessageID) {
		return review{}, false
	}
	if _, owned := g.chatProject(e.Context.OpenChatID); e.Context.OpenChatID != g.chat && !owned {
		return review{}, false
	}
	verb, _ := e.Action.Value[feishu.HumanVerbKey].(string)
	ticket, _ := e.Action.Value[feishu.HumanTicketKey].(string)
	rawAttempt, _ := e.Action.Value[feishu.HumanAttemptKey].(string)
	attempt, err := strconv.Atoi(rawAttempt)
	id := ticketID.FindStringSubmatch(ticket)
	if err != nil || attempt < 1 || id == nil || !slices.Contains([]string{feishu.VerbRelease, feishu.VerbRuling, feishu.VerbScope, feishu.VerbCancel}, verb) {
		return review{}, false
	}
	note, _ := e.Action.FormValue[feishu.HumanNoteField].(string)
	r := newReview(verb, id[1], ticket, strings.TrimSpace(note))
	r.attempt = attempt
	return r, true
}

// handleHumanAction answers a card press within the callback deadline with a
// toast. The ledger read and write run after the response under the same lock
// and rules as the text commands, so the ledger stays the decision's record.
func (g *Gateway) handleHumanAction(event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, func(context.Context) error) {
	r, ok := g.parseHumanAction(event)
	if !ok {
		return toast("error", "无法识别的卡片操作"), nil
	}
	operator, card := event.Event.Operator.OpenID, event.Event.Context.OpenMessageID
	if !slices.Contains(g.approvers, operator) {
		return toast("error", noApprovalReply), nil
	}
	needsReason := r.verb == feishu.VerbRuling || r.verb == feishu.VerbCancel
	if needsReason && r.reason == "" {
		return toast("error", "请先在输入框填写理由"), nil
	}
	if !needsReason && r.reason != "" {
		return toast("error", r.verb+"不带理由，需要说明时点裁定"), nil
	}
	return toast("success", "正在处理 "+r.ticket), func(ctx context.Context) error {
		p := g.project(r.prefix)
		if p.Ledger == nil {
			return g.reply(ctx, card, fmt.Sprintf("未找到项目 %s 的账本", r.prefix))
		}
		unlock := g.lockThread("review/" + r.ticket)
		defer unlock()
		t, err := p.Ledger.Read(ctx, r.ticket)
		if err != nil {
			fmt.Fprintf(os.Stderr, "intake review read %s failed: %v\n", r.ticket, err)
			return g.reply(ctx, card, fmt.Sprintf("账本中读取 %s 失败", r.ticket))
		}
		reply, applied := g.applyHumanReview(ctx, operator, r, p, t)
		if !applied {
			return g.reply(ctx, card, reply)
		}
		return g.updateCard(ctx, card, feishu.SettledHumanCard(r.ticket, t.Title, r.verb, operator, r.reason))
	}
}

func (g *Gateway) applyReview(ctx context.Context, ledger Ledger, ticket, sender string, r review) error {
	prefix := r.verb + "："
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
