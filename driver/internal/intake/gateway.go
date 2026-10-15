// Package intake handles the Feishu demand intake gateway.
package intake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"

	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const recentLimit = 10000

type Gateway struct {
	mu              sync.Mutex
	path, chat, bot string
	users           []string
	recent          []string
	reply           func(context.Context, string, string) error
}

func New(dir, chat, bot string, users []string, reply func(context.Context, string, string) error) (*Gateway, error) {
	g := &Gateway{path: filepath.Join(dir, "recent.json"), chat: chat, bot: bot, users: users, reply: reply}
	b, err := os.ReadFile(g.path)
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
	if !mentioned {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	id := *m.MessageId
	if slices.Contains(g.recent, id) {
		return nil
	}
	recent := append(slices.Clone(g.recent), id)
	if len(recent) > recentLimit {
		recent = recent[len(recent)-recentLimit:]
	}
	// Persist before the external write: an interrupted or ambiguous send must
	// never produce a second reply after restart.
	b, err := json.Marshal(recent)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(g.path), ".recent-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), g.path); err != nil {
		return err
	}
	g.recent = recent
	text := "无权限"
	if slices.Contains(g.users, *s.SenderId.OpenId) {
		text = "已收到，需求整理功能即将上线"
	}
	// reply_in_thread on the received message creates a thread for a new message
	// and preserves the existing thread_id for a reply; root_id may be absent.
	return g.reply(ctx, id, text)
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
