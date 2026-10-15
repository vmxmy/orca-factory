package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
)

func recordedEvent(t *testing.T, name string) *im.P2MessageReceiveV1 {
	t.Helper()
	b, err := os.ReadFile("../../testdata/recorded/feishu/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Event *im.P2MessageReceiveV1 `json:"event"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Event
}

func TestFeishuGatewayRecorded(t *testing.T) {
	for _, name := range []string{"bot-event-message-at", "bot-event-thread-at-reply"} {
		for _, scenario := range []string{"allowed", "denied", "other-chat", "private", "other-bot", "user-mention", "no-mention"} {
			t.Run(name+"/"+scenario, func(t *testing.T) {
				event := recordedEvent(t, name)
				switch scenario {
				case "denied":
					event.Event.Sender.SenderId.OpenId = ptr("ou_outside")
				case "other-chat":
					event.Event.Message.ChatId = ptr("oc_other")
				case "private":
					event.Event.Message.ChatType = ptr("p2p")
				case "other-bot":
					event.Event.Message.Mentions[0].Id.OpenId = ptr("ou_other")
				case "user-mention":
					event.Event.Message.Mentions[0].MentionedType = ptr("user")
				case "no-mention":
					event.Event.Message.Mentions = nil
				}
				calls := 0
				reply := func(ctx context.Context, id, text string) error {
					calls++
					if id != *event.Event.Message.MessageId {
						t.Errorf("reply id %s", id)
					}
					want := "已整理" + continueHint
					if scenario == "denied" {
						want = "无权限"
					}
					if text != want {
						t.Errorf("text=%q", text)
					}
					return nil
				}
				dir := t.TempDir()
				g, err := New(dir, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, 1, fixedAgent, reply)
				if err != nil {
					t.Fatal(err)
				}
				var wg sync.WaitGroup
				for range 4 {
					wg.Go(func() {
						if err := g.Handle(context.Background(), event); err != nil {
							t.Error(err)
						}
					})
				}
				wg.Wait()
				g, err = New(dir, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, 1, fixedAgent, reply)
				if err != nil {
					t.Fatal(err)
				}
				if err := g.Handle(context.Background(), event); err != nil {
					t.Fatal(err)
				}
				want := 0
				if scenario == "allowed" || scenario == "denied" {
					want = 1
				}
				if calls != want {
					t.Fatalf("calls=%d want=%d", calls, want)
				}
			})
		}
	}
}

func TestFeishuGatewayCorruptState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/recent.json", []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir, "chat", "bot", []string{"user"}, 1, nil, nil); err == nil {
		t.Fatal("corrupt state accepted")
	}
}

func ptr(s string) *string { return &s }

func fixedAgent(context.Context, Turn) (string, error) { return "已整理", nil }

// The recorder stores the decoded SDK response rather than the OpenAPI envelope.
// Only the code/data wrapper and token endpoint below are synthetic.
func TestFeishuGatewayRecordedHTTP(t *testing.T) {
	b, err := os.ReadFile("../../testdata/recorded/feishu/bot-message-reply-in-thread.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording struct {
		Request struct {
			Method, Path string
			Body         map[string]any
		}
		Response struct{ Data json.RawMessage }
	}
	if err := json.Unmarshal(b, &recording); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = fmt.Fprint(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		requests++
		if r.Method != recording.Request.Method || r.URL.Path != recording.Request.Path {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["msg_type"] != recording.Request.Body["msg_type"] || body["reply_in_thread"] != recording.Request.Body["reply_in_thread"] {
			t.Errorf("reply contract: %v", body)
		}
		var content map[string]string
		raw, ok := body["content"].(string)
		if !ok {
			t.Error("missing content")
		} else if err := json.Unmarshal([]byte(raw), &content); err != nil {
			t.Error(err)
		}
		if content["text"] != "已整理"+continueHint {
			t.Errorf("content=%v", content)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing tenant auth")
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": recording.Response.Data}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client, err := feishu.New(feishu.Options{Endpoint: server.URL, AppID: "test", AppSecret: "test"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, 1, fixedAgent, client.ReplyInThread)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../testdata/recorded/feishu/bot-event-message-at.json")
	if err != nil {
		t.Fatal(err)
	}
	var wrapper struct{ Event json.RawMessage }
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatal(err)
	}
	handler := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(g.Handle)
	if _, err := handler.Do(context.Background(), wrapper.Event); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("reply requests=%d", requests)
	}
}

func TestFeishuGatewayUncertainSendAndStateFailure(t *testing.T) {
	event := recordedEvent(t, "bot-event-message-at")
	dir := t.TempDir()
	calls := 0
	send := func(context.Context, string, string) error { calls++; return errors.New("send unconfirmed") }
	g, err := New(dir, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, 1, fixedAgent, send)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Handle(context.Background(), event); err == nil {
		t.Fatal("send error hidden")
	}
	g, err = New(dir, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, 1, fixedAgent, send)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Handle(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("uncertain send replayed %d times", calls)
	}
	event.Event.Message.MessageId = ptr("om_new")
	if err := os.Remove(filepath.Join(dir, "recent.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "recent.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := g.Handle(context.Background(), event); err == nil {
		t.Fatal("state write failure hidden")
	}
	if calls != 1 {
		t.Fatal("sent despite failed persistence")
	}
}
