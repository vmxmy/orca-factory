package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

// ReplyInThread replies to a received message, creating a thread if needed.
func (c *Client) ReplyInThread(ctx context.Context, messageID, text string) error {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	var data struct {
		MessageID string `json:"message_id"`
		ThreadID  string `json:"thread_id"`
	}
	err = c.call(ctx, http.MethodPost, "/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/reply", map[string]any{"msg_type": "text", "content": string(content), "reply_in_thread": true}, &data)
	if err != nil {
		return err
	}
	if data.MessageID == "" || data.ThreadID == "" {
		return errors.New("feishu thread reply unconfirmed")
	}
	return nil
}

// AddReaction adds a THUMBSUP reaction to a received message. Adding it to
// the same message twice returns the existing reaction_id (idempotent).
func (c *Client) AddReaction(ctx context.Context, messageID string) error {
	var data struct {
		ReactionID string `json:"reaction_id"`
	}
	err := c.call(ctx, http.MethodPost, "/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/reactions", map[string]any{"reaction_type": map[string]string{"emoji_type": "THUMBSUP"}}, &data)
	if err != nil {
		return err
	}
	if data.ReactionID == "" {
		return errors.New("feishu reaction unconfirmed")
	}
	return nil
}
