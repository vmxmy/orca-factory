package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestFeishuNotifyRecorded(t *testing.T) {
	for _, name := range []string{"bot-message-send.json", "bot-message-invalid-chat.json", "bot-message-bot-not-in-chat.json", "bot-scope-denied.json"} {
		t.Run(name, func(t *testing.T) {
			rec := loadRecording(t, name)
			input := loadRecording(t, "bot-message-send.json")
			if name != "bot-scope-denied.json" {
				input = rec
			}
			var body struct {
				ReceiveID string `json:"receive_id"`
				Content   string `json:"content"`
			}
			if err := json.Unmarshal(input.Request.Body, &body); err != nil {
				t.Fatal(err)
			}
			var content struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(body.Content), &content); err != nil {
				t.Fatal(err)
			}
			calls := 0
			c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				var got, want any
				if r.Method != input.Request.Method || r.URL.Path != input.Request.Path || r.URL.Query().Get("receive_id_type") != "chat_id" || json.NewDecoder(r.Body).Decode(&got) != nil || json.Unmarshal(input.Request.Body, &want) != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("request %s %s body=%v want=%v", r.Method, r.URL, got, want)
				}
				violations := []map[string]string{}
				for _, scope := range rec.Response.Error.MissingScopes {
					violations = append(violations, map[string]string{"subject": scope})
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"code": rec.Response.Error.Code, "data": rec.Response.Data, "error": map[string]any{"permission_violations": violations}}); err != nil {
					t.Error(err)
				}
			}, Options{})
			err := c.SendText(context.Background(), body.ReceiveID, content.Text)
			if calls != 1 {
				t.Fatalf("requests=%d", calls)
			}
			if rec.Response.Error.Code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != rec.Response.Error.Code || !reflect.DeepEqual(apiErr.MissingScopes, rec.Response.Error.MissingScopes) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
