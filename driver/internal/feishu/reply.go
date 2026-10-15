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
