// Package intake handles the Feishu demand intake gateway.
package intake

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	recentLimit           = 10000
	idleClose             = 7 * 24 * time.Hour
	defaultIntakeMaxTurns = 30

	deniedReply   = "无权限"
	queuedReply   = "排队中"
	failedReply   = "处理失败，请重试"
	closedReply   = "本话题超过 7 天无消息，已关闭；请在群里重新 @需求助手 发起需求"
	textOnlyReply = "请在 @需求助手 后用文字描述需求"
	endedReply    = "已结束本话题的需求整理"

	// quietMarker is the fixed Agent reply (after trimming) that tells the
	// gateway to record the turn without sending anything back to the group.
	quietMarker = "<无需回复>"
)

type SessionState string

const (
	Chatting        SessionState = "对话中"
	AwaitingConfirm SessionState = "待确认"
	Submitted       SessionState = "已提交"
	Closed          SessionState = "已关闭"
)

// Session is one thread conversation, keyed by the thread's root message ID:
// a new thread is rooted at the first @ message, and later messages in it carry
// that message as root_id.
type Session struct {
	ID          string       `json:"session_id"`
	Requester   string       `json:"requester"`
	State       SessionState `json:"state"`
	LastMessage time.Time    `json:"last_message_at"`
	// Speakers holds the open_ids of non-requester members who have spoken in
	// this session, comma-joined in the order they first spoke. It survives a
	// restart so a member's "[成员N]" label never changes mid-conversation.
	Speakers string `json:"speakers,omitempty"`
	// Turns counts agent turns since Generation's last rollover; Generation
	// counts rollovers; PendingSummary, when non-empty, is the carried-over
	// summary a new generation's first turn prefixes onto its prompt.
	Turns          int    `json:"turns,omitempty"`
	Generation     int    `json:"generation,omitempty"`
	PendingSummary string `json:"pending_summary,omitempty"`
	// Project is the prefix of the project this thread was routed to; set
	// once and never changed again in this thread (OF-32). PendingPrompt
	// holds the thread's original first message while Project is still ""
	// and the gateway is waiting for the requester to pick one.
	Project       string `json:"project,omitempty"`
	PendingPrompt string `json:"pending_prompt,omitempty"`
	// Draft is the validated draft awaiting the requester's “确认提交”; it
	// stays until a newer valid draft replaces it or it is submitted.
	// Submitted lists the created tickets once the thread is 已提交 (OF-15).
	Draft     []PendingTicket `json:"draft,omitempty"`
	Submitted []string        `json:"submitted,omitempty"`
}

// speakerLabel tags a turn with who sent it, since promptText carries only the
// message body and the Agent cannot otherwise tell members apart. The
// requester is always "[需求人]"; other whitelisted members are numbered
// "[成员2]", "[成员3]", ... by the order they first speak in this session.
func (s *Session) speakerLabel(sender string) string {
	if sender == s.Requester {
		return "[需求人]"
	}
	var members []string
	if s.Speakers != "" {
		members = strings.Split(s.Speakers, ",")
	}
	if idx := slices.Index(members, sender); idx >= 0 {
		return fmt.Sprintf("[成员%d]", idx+2)
	}
	members = append(members, sender)
	s.Speakers = strings.Join(members, ",")
	return fmt.Sprintf("[成员%d]", len(members)+1)
}

func isEndCommand(prompt string) bool {
	return prompt == "结束" || prompt == "取消"
}

type Gateway struct {
	mu             sync.Mutex
	dir, chat, bot string
	users, recent  []string
	approvers      []string
	threads        map[string]*sync.Mutex
	slots          chan struct{}
	maxTurns       int
	// projects is this gateway's current routable set, possibly empty (no
	// discovery has ever succeeded, or none is eligible right now): every new
	// topic still goes through selectProject, whose picker card then shows
	// "暂无可接管的项目" instead of calling the Agent with an unresolved
	// project (OF-48). defaultRemote is the configured group default,
	// compared against each Project's Remote and only preselected on the
	// picker card (OF-47), never chosen on the requester's behalf.
	projects []Project
	// onboardable is every repository discovery found in Orca's inventory but
	// this driver has never onboarded (OF-49): listed in the picker card with
	// an "接入" button only an approver's press acts on (OF-50), through
	// onboard. Unlike projects, nothing in a thread ever commits to one of
	// these, so SetOnboardable just replaces the set wholesale.
	onboardable []Onboardable
	onboard     Onboarder
	// pendingEnable is every repository discovery found already onboarded
	// (pipeline installed, check passing or only core-behind) but not yet
	// enabled in the ledger (OF-54): listed in the picker card with an "启用"
	// button only an approver's press acts on, through enable. Like
	// onboardable, nothing in a thread ever commits to one of these.
	pendingEnable []PendingEnable
	enable        Enabler
	defaultRemote string
	agent         func(context.Context, Turn) (string, error)
	reply         func(context.Context, string, string) error
	react         func(context.Context, string) error
	// replyCard sends an interactive card as a thread reply to a message;
	// updateCard replaces a sent card's content (OF-47).
	replyCard, updateCard func(context.Context, string, string) error
	now                   func() time.Time
}

var threadRoot = regexp.MustCompile(`^om_[0-9A-Za-z_]+$`)

func New(dir, chat, bot string, users, approvers []string, concurrency int, projects []Project, defaultRemote string, agent func(context.Context, Turn) (string, error), reply func(context.Context, string, string) error, react func(context.Context, string) error, replyCard, updateCard func(context.Context, string, string) error) (*Gateway, error) {
	if concurrency < 1 {
		return nil, errors.New("intake concurrency must be positive")
	}
	maxTurns := defaultIntakeMaxTurns
	if raw := os.Getenv("AGENT_INTAKE_MAX_TURNS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, errors.New("AGENT_INTAKE_MAX_TURNS must be a positive integer")
		}
		maxTurns = n
	}
	if err := validateProjects(projects); err != nil {
		return nil, err
	}
	g := &Gateway{dir: dir, chat: chat, bot: bot, users: users, approvers: approvers, threads: map[string]*sync.Mutex{}, slots: make(chan struct{}, concurrency), maxTurns: maxTurns, projects: projects, defaultRemote: defaultRemote, agent: agent, reply: reply, react: react, replyCard: replyCard, updateCard: updateCard, now: time.Now}
	b, err := os.ReadFile(filepath.Join(dir, "recent.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(b, &g.recent); err != nil {
			return nil, errors.New("invalid intake recent.json")
		}
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return g, nil
}

func validateProjects(projects []Project) error {
	seen := map[string]bool{}
	for _, p := range projects {
		if p.Prefix == "" {
			return errors.New("intake project prefix required")
		}
		if seen[p.Prefix] {
			return errors.New("intake project prefix duplicated")
		}
		seen[p.Prefix] = true
	}
	return nil
}

// SetProjects merges a freshly discovered project set into the gateway's
// routable projects (OF-42): a new Feishu topic rediscovers the live Orca
// repository inventory and ledger project table before picking a project,
// since both can change while this long-running gateway keeps serving older
// topics. fresh entries take priority over a same-prefix stale one; a stale
// project absent from fresh is kept (after every fresh entry, in its prior
// order) so a thread that already committed to it keeps working.
func (g *Gateway) SetProjects(fresh []Project) error {
	if err := validateProjects(fresh); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	seen := make(map[string]bool, len(fresh))
	merged := append([]Project{}, fresh...)
	for _, p := range fresh {
		seen[p.Prefix] = true
	}
	for _, stale := range g.projects {
		if !seen[stale.Prefix] {
			merged = append(merged, stale)
			seen[stale.Prefix] = true
		}
	}
	g.projects = merged
	return nil
}

func (g *Gateway) projectsSnapshot() []Project {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.projects
}

// SetOnboardable replaces the gateway's OF-49 onboardable set with fresh, a
// fresh discovery round's repositories that exist in Orca's inventory but
// have never been onboarded (scripts/agent-gates/ does not exist at all).
func (g *Gateway) SetOnboardable(fresh []Onboardable) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onboardable = fresh
}

func (g *Gateway) onboardableSnapshot() []Onboardable {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.onboardable
}

// SetPendingEnable replaces the gateway's OF-54 pending-enable set with
// fresh, a fresh discovery round's repositories that are onboarded and
// passing check but not yet enabled.
func (g *Gateway) SetPendingEnable(fresh []PendingEnable) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pendingEnable = fresh
}

func (g *Gateway) pendingEnableSnapshot() []PendingEnable {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pendingEnable
}

func (g *Gateway) Handle(ctx context.Context, event *im.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil {
		return nil
	}
	m, s := event.Event.Message, event.Event.Sender
	if value(m.ChatId) != g.chat || value(m.ChatType) != "group" || value(m.MessageId) == "" || s.SenderId == nil || value(s.SenderId.OpenId) == "" || value(s.SenderType) != "user" {
		return nil
	}
	mentioned := false
	for _, mention := range m.Mentions {
		if mention != nil && mention.Id != nil && value(mention.MentionedType) == "bot" && value(mention.Id.OpenId) == g.bot {
			mentioned = true
			break
		}
	}
	id, sender := *m.MessageId, *s.SenderId.OpenId
	if !mentioned {
		return g.handleUnmentioned(ctx, m, id, sender)
	}
	if first, err := g.claim(ctx, id); err != nil || !first {
		return err
	}
	if r, ok := parseReview(promptText(m)); ok {
		return g.handleReview(ctx, id, sender, r)
	}
	// reply_in_thread on the received message creates a thread for a new message
	// and preserves the existing thread_id for a reply.
	if !slices.Contains(g.users, sender) {
		return g.reply(ctx, id, deniedReply)
	}
	thread := id
	if value(m.ThreadId) != "" {
		thread = value(m.RootId)
	}
	if !threadRoot.MatchString(thread) {
		return errors.New("invalid intake thread root")
	}
	unlock := g.lockThread(thread)
	defer unlock()
	path := filepath.Join(g.dir, thread+".json")
	session, resume, err := readSession(path)
	if err != nil {
		return err
	}
	now := g.now()
	if resume && (session.State == Closed || now.Sub(session.LastMessage) > idleClose) {
		session.State = Closed
		if err = writeJSON(path, session); err != nil {
			return err
		}
		return g.reply(ctx, id, closedReply)
	}
	prompt := promptText(m)
	if prompt == "" {
		return g.reply(ctx, id, textOnlyReply)
	}
	if resume && isEndCommand(prompt) {
		g.addReceipt(ctx, id)
		session.State = Closed
		if err = writeJSON(path, session); err != nil {
			return err
		}
		return g.reply(ctx, id, endedReply)
	}
	if !resume {
		session = Session{ID: newSessionID(), Requester: sender}
	}
	if session.Project == "" && (!resume || session.PendingPrompt != "") {
		return g.selectProject(ctx, id, thread, path, session, resume, sender, prompt)
	}
	if resume && prompt == confirmCommand {
		return g.confirm(ctx, id, thread, path, session, sender)
	}
	return g.respond(ctx, id, thread, path, session, resume, sender, prompt)
}

// handleUnmentioned continues a thread session this gateway already opened,
// for a reply that does not @ the bot: the group now delivers every message,
// not only the ones that mention it. Anything short of an open, whitelisted
// session this gateway recognizes is dropped with no trace — no reply, no
// dedup claim, no state write, no logging of the message body — since most
// such messages are not addressed to the bot at all. An exact approval
// command is the exception: it counts in any thread this gateway opened,
// whatever its state and whoever sends it, and handleReview checks the
// sender against the approvers.
func (g *Gateway) handleUnmentioned(ctx context.Context, m *im.EventMessage, id, sender string) error {
	thread := value(m.RootId)
	if value(m.ThreadId) == "" || !threadRoot.MatchString(thread) {
		return nil
	}
	path := filepath.Join(g.dir, thread+".json")
	if r, ok := parseReview(promptText(m)); ok {
		if _, resume, err := readSession(path); err != nil || !resume {
			return err
		}
		if first, err := g.claim(ctx, id); err != nil || !first {
			return err
		}
		return g.handleReview(ctx, id, sender, r)
	}
	if !slices.Contains(g.users, sender) {
		return nil
	}
	unlock := g.lockThread(thread)
	defer unlock()
	session, resume, err := readSession(path)
	if err != nil {
		return err
	}
	if !resume || session.State == Closed || g.now().Sub(session.LastMessage) > idleClose {
		return nil
	}
	if first, err := g.claim(ctx, id); err != nil || !first {
		return err
	}
	prompt := promptText(m)
	if prompt == "" {
		return g.reply(ctx, id, textOnlyReply)
	}
	if isEndCommand(prompt) {
		g.addReceipt(ctx, id)
		session.State = Closed
		if err := writeJSON(path, session); err != nil {
			return err
		}
		return g.reply(ctx, id, endedReply)
	}
	if session.Project == "" && session.PendingPrompt != "" {
		return g.selectProject(ctx, id, thread, path, session, true, sender, prompt)
	}
	if prompt == confirmCommand {
		return g.confirm(ctx, id, thread, path, session, sender)
	}
	return g.respond(ctx, id, thread, path, session, true, sender, prompt)
}

// addReceipt marks id as received with a 👍 before the Agent runs, so the
// requester can tell "received" apart from "no reply needed" and from
// never having been delivered at all. A failure here only logs a warning
// (never the message text) and is not retried on this delivery.
func (g *Gateway) addReceipt(ctx context.Context, id string) {
	if err := g.react(ctx, id); err != nil {
		fmt.Fprintf(os.Stderr, "intake reaction failed for message %s: %v\n", id, err)
	}
}

// respond runs one agent turn under the concurrency limit, persists the
// session, and sends the reply.
func (g *Gateway) respond(ctx context.Context, id, thread, path string, session Session, resume bool, sender, prompt string) error {
	g.addReceipt(ctx, id)
	label := session.speakerLabel(sender)
	select {
	case g.slots <- struct{}{}:
	default:
		if err := g.reply(ctx, id, queuedReply); err != nil {
			return err
		}
		select {
		case g.slots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	turnPrompt := label + " " + prompt
	callResume := resume && session.PendingSummary == ""
	if session.PendingSummary != "" {
		turnPrompt = session.PendingSummary + "\n\n" + turnPrompt
	}
	answer, err := g.agent(ctx, Turn{Session: session.ID, Resume: callResume, Prompt: turnPrompt, Project: g.project(session.Project)})
	<-g.slots
	if err != nil {
		fmt.Fprintf(os.Stderr, "intake agent turn failed for thread %s: %v\n", thread, err)
		return g.reply(ctx, id, failedReply)
	}
	session.PendingSummary = ""
	session.Turns++
	if session.State != AwaitingConfirm && session.State != Submitted {
		session.State = Chatting
	}
	session.LastMessage = g.now()
	answer, session, err = g.reviewDraft(ctx, thread, session, answer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "intake draft correction failed for thread %s: %v\n", thread, err)
		if err := writeJSON(path, session); err != nil {
			return err
		}
		return g.reply(ctx, id, failedReply)
	}
	if session.Turns >= g.maxTurns {
		if summary, ok := g.rollSummary(ctx, session); ok {
			session.ID = newSessionID()
			session.Generation++
			session.Turns = 0
			session.PendingSummary = summary
		} else {
			// Back off instead of requesting a summary again every turn
			// until the cap is reached once more.
			session.Turns = g.maxTurns / 2
			fmt.Fprintf(os.Stderr, "intake rolling summary rejected for thread %s\n", thread)
		}
	}
	if err = writeJSON(path, session); err != nil {
		return err
	}
	// quietMarker lets the Agent sit out a turn (members chatting among
	// themselves, a bare acknowledgment) while the session still advances.
	if strings.TrimSpace(answer) == quietMarker {
		return nil
	}
	return g.reply(ctx, id, answer)
}

// rollSummary runs one extra agent turn inside session asking it to compress
// the conversation so far, under the same concurrency slot as an ordinary
// turn. It returns false when the agent call fails or the reply has no valid
// intakeSummary block, in which case the caller keeps the current session.
func (g *Gateway) rollSummary(ctx context.Context, session Session) (string, bool) {
	answer, err := g.turn(ctx, Turn{Session: session.ID, Resume: true, Prompt: summaryRequestPrompt(g.maxTurns), Project: g.project(session.Project)})
	if err != nil {
		return "", false
	}
	return parseSummary(answer)
}

// turn runs one gateway-initiated agent call (summary request, draft
// correction) under the concurrency limit, waiting for a slot silently.
func (g *Gateway) turn(ctx context.Context, t Turn) (string, error) {
	select {
	case g.slots <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-g.slots }()
	return g.agent(ctx, t)
}

func summaryRequestPrompt(maxTurns int) string {
	return fmt.Sprintf("[网关] 本话题已进行 %d 轮，请把目前的讨论压缩成摘要，只输出下面这个代码块，不要附加其他文字：\n\n"+
		"```intake-summary-v1\n{\n  \"需求摘要\": \"...\",\n  \"已定决策\": \"...\",\n  \"未决问题\": \"...\",\n  \"当前草稿\": \"...\"\n}\n```", maxTurns)
}

var summaryBlock = regexp.MustCompile("(?s)```intake-summary-v1\\s*\\n(.*?)\\n```")

type intakeSummary struct {
	Need    string `json:"需求摘要"`
	Decided string `json:"已定决策"`
	Open    string `json:"未决问题"`
	Draft   string `json:"当前草稿"`
}

// parseSummary extracts and validates the intake-summary-v1 code block from
// an agent reply, returning the next generation's carried-over prompt prefix.
func parseSummary(answer string) (string, bool) {
	m := summaryBlock.FindStringSubmatch(answer)
	if m == nil {
		return "", false
	}
	var s intakeSummary
	if json.Unmarshal([]byte(m[1]), &s) != nil {
		return "", false
	}
	if strings.TrimSpace(s.Need) == "" || strings.TrimSpace(s.Decided) == "" || strings.TrimSpace(s.Open) == "" || strings.TrimSpace(s.Draft) == "" {
		return "", false
	}
	return fmt.Sprintf("[网关] 新会话续接，此前摘要：\n需求摘要：%s\n已定决策：%s\n未决问题：%s\n当前草稿：%s", s.Need, s.Decided, s.Open, s.Draft), true
}

// claim records the message ID before any external write: an interrupted or
// ambiguous send must never produce a second reply after restart.
func (g *Gateway) claim(ctx context.Context, id string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if slices.Contains(g.recent, id) {
		return false, nil
	}
	recent := append(slices.Clone(g.recent), id)
	if len(recent) > recentLimit {
		recent = recent[len(recent)-recentLimit:]
	}
	if err := writeJSON(filepath.Join(g.dir, "recent.json"), recent); err != nil {
		return false, err
	}
	g.recent = recent
	return true, nil
}

func (g *Gateway) lockThread(thread string) func() {
	g.mu.Lock()
	lock := g.threads[thread]
	if lock == nil {
		lock = &sync.Mutex{}
		g.threads[thread] = lock
	}
	g.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func readSession(path string) (Session, bool, error) {
	var s Session
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	if json.Unmarshal(b, &s) != nil || s.ID == "" {
		return s, false, fmt.Errorf("invalid intake session %s", filepath.Base(path))
	}
	return s, true, nil
}

// promptText is the message text with this bot's mention removed and other
// mentions shown by name; non-text messages yield "".
func promptText(m *im.EventMessage) string {
	if value(m.MessageType) != "text" {
		return ""
	}
	var content struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(value(m.Content)), &content) != nil {
		return ""
	}
	text := content.Text
	for _, mention := range m.Mentions {
		if mention == nil || value(mention.Key) == "" {
			continue
		}
		name := "@" + value(mention.Name)
		if value(mention.MentionedType) == "bot" {
			name = ""
		}
		text = strings.ReplaceAll(text, value(mention.Key), name)
	}
	return strings.TrimSpace(text)
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

// writeAtomic writes b to path via a sibling temp file and rename, so a
// reader never observes a partial write.
func writeAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".intake-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
