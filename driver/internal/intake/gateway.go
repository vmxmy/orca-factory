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
	"strings"
	"sync"
	"time"

	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	recentLimit = 10000
	idleClose   = 7 * 24 * time.Hour

	deniedReply   = "无权限"
	queuedReply   = "排队中"
	failedReply   = "处理失败，请重试"
	closedReply   = "本话题超过 7 天无消息，已关闭；请在群里重新 @需求助手 发起需求"
	textOnlyReply = "请在 @需求助手 后用文字描述需求"
)

type SessionState string

const (
	Chatting SessionState = "对话中"
	Closed   SessionState = "已关闭"
)

// Session is one thread conversation, keyed by the thread's root message ID:
// a new thread is rooted at the first @ message, and later messages in it carry
// that message as root_id.
type Session struct {
	ID          string       `json:"session_id"`
	Requester   string       `json:"requester"`
	State       SessionState `json:"state"`
	LastMessage time.Time    `json:"last_message_at"`
}

type Gateway struct {
	mu             sync.Mutex
	dir, chat, bot string
	users, recent  []string
	threads        map[string]*sync.Mutex
	slots          chan struct{}
	agent          func(context.Context, Turn) (string, error)
	reply          func(context.Context, string, string) error
	now            func() time.Time
}

var threadRoot = regexp.MustCompile(`^om_[0-9A-Za-z_]+$`)

func New(dir, chat, bot string, users []string, concurrency int, agent func(context.Context, Turn) (string, error), reply func(context.Context, string, string) error) (*Gateway, error) {
	if concurrency < 1 {
		return nil, errors.New("intake concurrency must be positive")
	}
	g := &Gateway{dir: dir, chat: chat, bot: bot, users: users, threads: map[string]*sync.Mutex{}, slots: make(chan struct{}, concurrency), agent: agent, reply: reply, now: time.Now}
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
	if !resume {
		session = Session{ID: newSessionID(), Requester: sender}
	}
	return g.respond(ctx, id, thread, path, session, resume, prompt)
}

// handleUnmentioned continues a thread session this gateway already opened,
// for a reply that does not @ the bot: the group now delivers every message,
// not only the ones that mention it. Anything short of an open, whitelisted
// session this gateway recognizes is dropped with no trace — no reply, no
// dedup claim, no state write, no logging of the message body — since most
// such messages are not addressed to the bot at all.
func (g *Gateway) handleUnmentioned(ctx context.Context, m *im.EventMessage, id, sender string) error {
	if value(m.ThreadId) == "" || !slices.Contains(g.users, sender) {
		return nil
	}
	thread := value(m.RootId)
	if !threadRoot.MatchString(thread) {
		return nil
	}
	unlock := g.lockThread(thread)
	defer unlock()
	path := filepath.Join(g.dir, thread+".json")
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
	return g.respond(ctx, id, thread, path, session, true, prompt)
}

// respond runs one agent turn under the concurrency limit, persists the
// session, and sends the reply.
func (g *Gateway) respond(ctx context.Context, id, thread, path string, session Session, resume bool, prompt string) error {
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
	answer, err := g.agent(ctx, Turn{Session: session.ID, Resume: resume, Prompt: prompt})
	<-g.slots
	if err != nil {
		fmt.Fprintf(os.Stderr, "intake agent turn failed for thread %s: %v\n", thread, err)
		return g.reply(ctx, id, failedReply)
	}
	session.State, session.LastMessage = Chatting, g.now()
	if err = writeJSON(path, session); err != nil {
		return err
	}
	return g.reply(ctx, id, answer)
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
