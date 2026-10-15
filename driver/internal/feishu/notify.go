package feishu

import (
	"context"
	"encoding/json"
	"net/http"
)

// SendText sends once as the application. An uncertain response is not retried.
func (c *Client) SendText(ctx context.Context, chat, text string) error {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodPost, "/open-apis/im/v1/messages?receive_id_type=chat_id", map[string]string{
		"receive_id": chat, "msg_type": "text", "content": string(content),
	}, nil)
}
