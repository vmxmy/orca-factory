package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

// Ledger is the part of a project's issues.Store the gateway writes through:
// List for dependency and duplicate lookups, Create for submission (OF-15);
// Read, the event history and UpdateIssue for approval and rejection (OF-16).
type Ledger interface {
	List(context.Context) ([]issues.Task, error)
	Create(context.Context, issues.CreateInput) (issues.Task, error)
	Read(context.Context, string) (issues.Task, error)
	ListComments(context.Context, string) ([]linear.Comment, error)
	CreateComment(context.Context, string, string) (linear.Comment, error)
	UpdateIssue(context.Context, string, string, []string, []string) (linear.Issue, error)
}

const (
	confirmCommand = "确认提交"
	// submitMarker tags every ticket the gateway creates with its thread (and
	// draft key, for multi-ticket drafts), so a repeated confirm or a restart
	// finds the ticket in the ledger instead of creating it again.
	submitMarker = "intake-chat-v1: "
	// draftCorrections bounds the extra Agent turns spent fixing a draft that
	// failed validation within one requester message.
	draftCorrections = 2

	requesterOnlyReply = "仅需求人可以确认提交"
	noDraftReply       = "当前没有待确认的草稿"
)

var (
	draftBlock = regexp.MustCompile("(?s)```intake-draft-v1\\s*\\n(.*?)\\n```")
	draftKey   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	validSizes = []float64{1, 2, 3, 5, 8}
)

// draftWire is the Agent's intake-draft-v1 JSON: either one ticket at the
// top level, or a tickets array whose entries carry a local key and
// depends_on. A lane in the draft is ignored; the gateway computes it.
type draftWire struct {
	draftTicketWire
	Tickets []draftTicketWire `json:"tickets"`
}

type draftTicketWire struct {
	Key       string   `json:"key"`
	DependsOn []string `json:"depends_on"`
	Title     string   `json:"title"`
	Todo      []string `json:"要做"`
	Check     string   `json:"CHECK"`
	Scope     string   `json:"SCOPE"`
	Body      struct {
		Now    string `json:"现状"`
		Assert string `json:"断言"`
		Deps   string `json:"依赖说明"`
	} `json:"正文"`
	Estimate float64  `json:"estimate"`
	Blockers []string `json:"blockers"`
	Related  []string `json:"related"`
}

// PendingTicket is one validated draft ticket awaiting the requester's
// confirmation. Blockers holds draft keys (created earlier in the same
// submission) or ledger identifiers; Related holds ledger identifiers.
// Description is the full ticket face, including the gateway-computed lane,
// the proposer and the thread marker.
type PendingTicket struct {
	Key         string   `json:"key,omitempty"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Scope       string   `json:"scope"`
	Check       string   `json:"check"`
	Estimate    float64  `json:"estimate"`
	Lane        int      `json:"lane"`
	Blockers    []string `json:"blockers"`
	Related     []string `json:"related"`
}

func hasDraft(answer string) bool { return draftBlock.MatchString(answer) }

// validateDraft parses the draft in answer and checks it against the
// project checkout and ledger. It returns the tickets in dependency order,
// or the problems to hand back to the Agent.
func validateDraft(ctx context.Context, p Project, thread, requester, answer string) ([]PendingTicket, []string) {
	m := draftBlock.FindStringSubmatch(answer)
	var wire draftWire
	if err := json.Unmarshal([]byte(m[1]), &wire); err != nil {
		return nil, []string{"草稿不是合法 JSON：" + err.Error()}
	}
	drafts := wire.Tickets
	multi := len(drafts) > 0
	if !multi {
		drafts = []draftTicketWire{wire.draftTicketWire}
	}
	tasks, err := p.Ledger.List(ctx)
	if err != nil {
		return nil, []string{"账本读取失败，无法核对依赖：" + err.Error()}
	}
	ledger := map[string]issues.Task{}
	for _, t := range tasks {
		for _, ref := range []string{t.Metadata.Identifier, t.Metadata.ID} {
			if ref != "" {
				ledger[ref] = t
			}
		}
	}
	files, err := checkoutFiles(p.Checkout)
	if err != nil {
		return nil, []string{"检出读取失败，无法核对 SCOPE：" + err.Error()}
	}
	keys := map[string]bool{}
	for _, d := range drafts {
		keys[d.Key] = true
	}
	var problems []string
	out := make([]PendingTicket, 0, len(drafts))
	for i, d := range drafts {
		name := fmt.Sprintf("第 %d 张票", i+1)
		if multi {
			name = fmt.Sprintf("票 %q", d.Key)
			if !draftKey.MatchString(d.Key) || slices.ContainsFunc(drafts[:i], func(o draftTicketWire) bool { return o.Key == d.Key }) {
				problems = append(problems, fmt.Sprintf("%s：key 必须唯一且只含字母、数字、-、_", name))
			}
		} else if d.Key != "" || len(d.DependsOn) > 0 {
			problems = append(problems, "单票草稿不写 key 与 depends_on；多张票请用 tickets 数组")
		}
		problems = append(problems, missingFields(name, d)...)
		scope := strings.Fields(d.Scope)
		for _, glob := range scope {
			if _, err := path.Match(glob, ""); err != nil || !slices.ContainsFunc(files, func(f string) bool { return globMatch(glob, f) }) {
				problems = append(problems, fmt.Sprintf("%s：SCOPE %s 在检出中没有匹配的路径", name, glob))
			}
		}
		blockers := slices.Concat(d.Blockers, d.DependsOn)
		refs := slices.Concat(blockers, d.Related)
		if len(slices.Compact(slices.Sorted(slices.Values(refs)))) != len(refs) {
			problems = append(problems, fmt.Sprintf("%s：blockers、depends_on、related 中有重复的票", name))
		}
		for _, ref := range refs {
			if multi && keys[ref] && ref != d.Key && slices.Contains(blockers, ref) {
				continue
			}
			t, ok := ledger[ref]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s：依赖 %s 在账本和本草稿中都不存在", name, ref))
			} else if !t.Metadata.Complete {
				problems = append(problems, fmt.Sprintf("%s：依赖 %s 尚未纳管", name, ref))
			}
		}
		lane := predictLane(files, scope, strings.Fields(p.Lane2Globs), strings.Fields(p.Lane3Globs))
		out = append(out, PendingTicket{Key: d.Key, Title: strings.TrimSpace(d.Title), Description: ticketFace(d, lane, thread, requester), Scope: strings.Join(scope, " "), Check: strings.TrimSpace(d.Check),
			Estimate: d.Estimate, Lane: lane, Blockers: nonEmpty(blockers), Related: nonEmpty(d.Related)})
	}
	if len(problems) > 0 {
		return nil, problems
	}
	ordered, ok := topoOrder(out)
	if !ok {
		return nil, []string{"票之间的 depends_on 成环，请拆开循环依赖"}
	}
	return ordered, nil
}

func missingFields(name string, d draftTicketWire) []string {
	var missing []string
	for field, v := range map[string]string{"title": d.Title, "CHECK": d.Check, "SCOPE": d.Scope, "正文.现状": d.Body.Now, "正文.断言": d.Body.Assert, "正文.依赖说明": d.Body.Deps} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, field)
		}
	}
	if len(d.Todo) == 0 || slices.ContainsFunc(d.Todo, func(s string) bool { return strings.TrimSpace(s) == "" }) {
		missing = append(missing, "要做")
	}
	if !slices.Contains(validSizes, d.Estimate) {
		missing = append(missing, "estimate（1、2、3、5、8）")
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return []string{fmt.Sprintf("%s：缺少或不合格的字段 %s", name, strings.Join(missing, "、"))}
}

func nonEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// topoOrder sorts tickets so every draft-local blocker comes first, keeping
// draft order among independent tickets. ok is false on a cycle.
func topoOrder(tickets []PendingTicket) ([]PendingTicket, bool) {
	local := map[string]bool{}
	for _, t := range tickets {
		local[t.Key] = true
	}
	placed := map[string]bool{}
	out := make([]PendingTicket, 0, len(tickets))
	for len(out) < len(tickets) {
		progressed := false
		for _, t := range tickets {
			if placed[t.Key] || slices.ContainsFunc(t.Blockers, func(b string) bool { return local[b] && !placed[b] }) {
				continue
			}
			placed[t.Key], progressed = true, true
			out = append(out, t)
		}
		if !progressed {
			return nil, false
		}
	}
	return out, true
}

// checkoutFiles lists the checkout's files as slash paths, skipping git
// metadata and the gateway's own .intake/ context.
func checkoutFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && (e.Name() == ".git" || e.Name() == ".intake") {
			return filepath.SkipDir
		}
		if e.Type().IsRegular() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files, err
}

// globMatch matches like core/gate.sh's [[ $f == $glob ]]: * crosses /.
func globMatch(glob, file string) bool {
	matched, err := path.Match(strings.ReplaceAll(glob, "/", "\x00"), strings.ReplaceAll(file, "/", "\x00"))
	return err == nil && matched
}

func matchAny(file string, globs []string) bool {
	return slices.ContainsFunc(globs, func(g string) bool { return globMatch(g, file) })
}

// predictLane applies core/gate.sh's lane rule to every checkout file the
// SCOPE covers: any lane-3 hit makes 3, otherwise any lane-2 hit makes 2.
func predictLane(files, scope, lane2, lane3 []string) int {
	lane := 1
	for _, f := range files {
		if !matchAny(f, scope) {
			continue
		}
		if matchAny(f, lane3) {
			return 3
		}
		if matchAny(f, lane2) {
			lane = 2
		}
	}
	return lane
}

// ticketFace renders the intake ticket template issues.Create parses:
// everything outside 要做, 验收 and SCOPE lands in the ledger's 正文,
// including the proposer and the thread marker.
func ticketFace(d draftTicketWire, lane int, thread, requester string) string {
	marker := submitMarker + thread
	if d.Key != "" {
		marker += " " + d.Key
	}
	var todo strings.Builder
	for i, item := range d.Todo {
		fmt.Fprintf(&todo, "%d. %s\n", i+1, strings.TrimSpace(item))
	}
	return fmt.Sprintf("## 现状\n%s\n\n## 断言\n%s\n\n## 依赖说明\n%s\n\n## LANE 预判\n%d（网关按 AGENT_LANE2_GLOBS / AGENT_LANE3_GLOBS 计算）\n\n## 来源\n提出人：%s（飞书话题 %s）\n%s\n\n## 要做\n%s\n## 验收\nCHECK: %s\n\n## SCOPE\n%s\n",
		strings.TrimSpace(d.Body.Now), strings.TrimSpace(d.Body.Assert), strings.TrimSpace(d.Body.Deps), lane, requester, thread, marker,
		todo.String(), strings.TrimSpace(d.Check), strings.Join(strings.Fields(d.Scope), " "))
}

// draftSummary is the ticket-face summary posted for confirmation, after any
// text the Agent wrote around its draft block.
func draftSummary(answer string, tickets []PendingTicket) string {
	var b strings.Builder
	if rest := strings.TrimSpace(draftBlock.ReplaceAllString(answer, "")); rest != "" {
		b.WriteString(rest + "\n\n")
	}
	fmt.Fprintf(&b, "票面摘要（共 %d 张）：\n", len(tickets))
	for i, t := range tickets {
		deps := "无"
		if len(t.Blockers) > 0 {
			deps = strings.Join(t.Blockers, ", ")
		}
		fmt.Fprintf(&b, "%d. %s\n   LANE 预判：%d\n   SCOPE：%s\n   CHECK：%s\n   依赖：%s\n", i+1, t.Title, t.Lane, t.Scope, t.Check, deps)
	}
	b.WriteString("需求人回复“确认提交”以提交")
	return b.String()
}

func correctionPrompt(problems []string) string {
	return "[网关] 草稿未通过校验，请修正后重新输出完整的 intake-draft-v1 草稿；需要需求人决定时先问需求人：\n- " + strings.Join(problems, "\n- ")
}

// submit creates the pending tickets in order. A ticket whose thread marker
// is already in the ledger is reused, not created again, so a repeated
// confirm or a crash between Create and the session write never duplicates.
func submit(ctx context.Context, ledger Ledger, thread string, pending []PendingTicket) ([]issues.Task, error) {
	tasks, err := ledger.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	existing := map[string]issues.Task{}
	for _, t := range tasks {
		ids[t.Metadata.Identifier], ids[t.Metadata.ID] = t.Metadata.ID, t.Metadata.ID
		if marked, key, ok := intakeMarker(t.Description); ok && marked == thread {
			existing[key] = t
		}
	}
	created := map[string]issues.Task{}
	out := make([]issues.Task, 0, len(pending))
	for _, p := range pending {
		task, ok := existing[p.Key]
		if !ok {
			if task, err = createTicket(ctx, ledger, p, ids, created); err != nil {
				return nil, err
			}
		}
		created[p.Key] = task
		out = append(out, task)
	}
	return out, nil
}

func createTicket(ctx context.Context, ledger Ledger, p PendingTicket, ids map[string]string, created map[string]issues.Task) (issues.Task, error) {
	resolve := func(refs []string, local bool) ([]string, error) {
		out := make([]string, 0, len(refs))
		for _, ref := range refs {
			if t, ok := created[ref]; ok && local {
				out = append(out, t.Metadata.ID)
			} else if id, ok := ids[ref]; ok {
				out = append(out, id)
			} else {
				return nil, fmt.Errorf("intake dependency %s unavailable", ref)
			}
		}
		return out, nil
	}
	blockers, err := resolve(p.Blockers, true)
	if err != nil {
		return issues.Task{}, err
	}
	related, err := resolve(p.Related, false)
	if err != nil {
		return issues.Task{}, err
	}
	estimate := p.Estimate
	task, err := ledger.Create(ctx, issues.CreateInput{Title: p.Title, Description: p.Description, State: "Todo", Labels: []string{}, Estimate: &estimate, Blockers: blockers, Related: related})
	if err != nil {
		return issues.Task{}, fmt.Errorf("intake create %q: %w", p.Title, err)
	}
	return task, nil
}

// intakeMarker returns the thread and draft key ("" for a single-ticket
// draft) from the marker line the gateway wrote into a ticket it submitted.
func intakeMarker(description string) (thread, key string, ok bool) {
	for _, line := range strings.Split(description, "\n") {
		rest, found := strings.CutPrefix(strings.TrimSpace(line), submitMarker)
		fields := strings.Fields(rest)
		if !found || len(fields) == 0 {
			continue
		}
		if len(fields) > 1 {
			key = fields[1]
		}
		return fields[0], key, true
	}
	return "", "", false
}

func submittedReply(tasks []string, approvers []string) string {
	var b strings.Builder
	b.WriteString("已提交，待审批人放行：\n" + strings.Join(tasks, "\n"))
	if len(approvers) > 0 {
		b.WriteString("\n请审批：")
		for _, a := range approvers {
			fmt.Fprintf(&b, `<at user_id="%s"></at>`, a)
		}
	}
	return b.String()
}

// confirm handles “确认提交”: only the session requester can submit, only a
// pending draft is submitted, and a submitted thread just repeats its
// tickets.
func (g *Gateway) confirm(ctx context.Context, id, thread, path string, session Session, sender string) error {
	g.addReceipt(ctx, id)
	if sender != session.Requester {
		return g.reply(ctx, id, requesterOnlyReply)
	}
	if session.State == Submitted {
		return g.reply(ctx, id, "本话题已提交：\n"+strings.Join(session.Submitted, "\n"))
	}
	p := g.project(session.Project)
	if session.State != AwaitingConfirm || len(session.Draft) == 0 || p.Ledger == nil {
		return g.reply(ctx, id, noDraftReply)
	}
	tasks, err := submit(ctx, p.Ledger, thread, session.Draft)
	if err != nil {
		fmt.Fprintf(os.Stderr, "intake submit failed for thread %s: %v\n", thread, err)
		return g.reply(ctx, id, failedReply)
	}
	session.Submitted = make([]string, 0, len(tasks))
	for _, t := range tasks {
		session.Submitted = append(session.Submitted, fmt.Sprintf("%s %s %s", t.Metadata.Identifier, t.Title, t.URL))
	}
	session.State, session.Draft, session.LastMessage = Submitted, nil, g.now()
	if err := writeJSON(path, session); err != nil {
		return err
	}
	return g.reply(ctx, id, submittedReply(session.Submitted, g.approvers))
}

// reviewDraft validates a draft in the Agent's answer, giving the Agent up
// to draftCorrections extra turns to fix it before the requester sees
// anything. It returns the text to post and the session with the draft
// recorded; an answer without a draft passes through unchanged.
func (g *Gateway) reviewDraft(ctx context.Context, thread string, session Session, answer string) (string, Session, error) {
	p := g.project(session.Project)
	if p.Ledger == nil {
		return answer, session, nil
	}
	for attempt := 0; hasDraft(answer); attempt++ {
		tickets, problems := validateDraft(ctx, p, thread, session.Requester, answer)
		if problems == nil {
			session.State, session.Draft = AwaitingConfirm, tickets
			return draftSummary(answer, tickets), session, nil
		}
		if attempt == draftCorrections {
			fmt.Fprintf(os.Stderr, "intake draft rejected for thread %s after %d corrections\n", thread, attempt)
			return "草稿未通过网关校验：\n- " + strings.Join(problems, "\n- "), session, nil
		}
		var err error
		if answer, err = g.turn(ctx, Turn{Session: session.ID, Resume: true, Prompt: correctionPrompt(problems), Project: p}); err != nil {
			return "", session, err
		}
		session.Turns++
	}
	return answer, session, nil
}
