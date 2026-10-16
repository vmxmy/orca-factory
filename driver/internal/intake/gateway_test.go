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
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
					want := "已整理"
					if scenario == "denied" {
						want = "无权限"
					}
					if text != want {
						t.Errorf("text=%q", text)
					}
					return nil
				}
				dir := t.TempDir()
				g, err := New(dir, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, nil, "", fixedAgent, reply, noopReact, nil, nil)
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
				g, err = New(dir, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, nil, "", fixedAgent, reply, noopReact, nil, nil)
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
	if _, err := New(dir, "chat", "bot", []string{"user"}, nil, 1, nil, "", nil, nil, nil, nil, nil); err == nil {
		t.Fatal("corrupt state accepted")
	}
}

func ptr(s string) *string { return &s }

func fixedAgent(context.Context, Turn) (string, error) { return "已整理", nil }

func noopReact(context.Context, string) error { return nil }

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
		if content["text"] != "已整理" {
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
	g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, nil, "", fixedAgent, client.ReplyInThread, noopReact, nil, nil)
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
	g, err := New(dir, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, nil, "", fixedAgent, send, noopReact, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Handle(context.Background(), event); err == nil {
		t.Fatal("send error hidden")
	}
	g, err = New(dir, "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, nil, "", fixedAgent, send, noopReact, nil, nil)
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

// TestIntakeThreadReplyWithoutMention covers OF-29: now that the group
// delivers every message, a thread reply that does not @ the bot continues
// an open, whitelisted session exactly as an @ reply would, and anything
// short of that (no session, a closed or idle-expired one, an unlisted
// sender, or a plain group message with no thread at all) is dropped with
// no trace.
func TestIntakeThreadReplyWithoutMention(t *testing.T) {
	const thread = "om_x100b62338a9e9890df2772823e2edec"
	existing := Session{ID: "existing-session", Requester: "ou_REDACTED", State: Chatting, LastMessage: time.Now().Add(-time.Hour)}

	for _, scenario := range []string{"resumes", "no-session", "closed-session", "idle-expired", "unlisted-sender", "group-no-thread"} {
		t.Run(scenario, func(t *testing.T) {
			name := "bot-event-thread-reply-no-at"
			if scenario == "group-no-thread" {
				name = "bot-event-group-message-no-at"
			}
			event := recordedEvent(t, name)
			dir := t.TempDir()
			onDisk := existing
			switch scenario {
			case "closed-session":
				onDisk.State = Closed
			case "idle-expired":
				onDisk.LastMessage = time.Now().Add(-idleClose - time.Hour)
			}
			switch scenario {
			case "resumes", "group-no-thread", "closed-session", "idle-expired":
				if err := writeJSON(filepath.Join(dir, thread+".json"), onDisk); err != nil {
					t.Fatal(err)
				}
			}
			users := []string{"ou_REDACTED"}
			if scenario == "unlisted-sender" {
				users = []string{"ou_someone_else"}
			}
			calls := 0
			agent := func(_ context.Context, turn Turn) (string, error) {
				calls++
				if turn.Session != existing.ID || !turn.Resume {
					t.Errorf("turn=%+v", turn)
				}
				return "已续聊", nil
			}
			replies := 0
			reply := func(_ context.Context, id, text string) error {
				replies++
				if id != *event.Event.Message.MessageId || text != "已续聊" {
					t.Errorf("reply id=%s text=%q", id, text)
				}
				return nil
			}
			g, err := New(dir, "oc_REDACTED", "ou_BOT_REDACTED", users, nil, 1, nil, "", agent, reply, noopReact, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := g.Handle(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			wantCalls, wantReplies := 0, 0
			if scenario == "resumes" {
				wantCalls, wantReplies = 1, 1
			}
			if calls != wantCalls || replies != wantReplies {
				t.Fatalf("calls=%d replies=%d want %d/%d", calls, replies, wantCalls, wantReplies)
			}
			claimed := false
			if b, err := os.ReadFile(filepath.Join(dir, "recent.json")); err == nil {
				var recent []string
				if err := json.Unmarshal(b, &recent); err != nil {
					t.Fatal(err)
				}
				claimed = slices.Contains(recent, *event.Event.Message.MessageId)
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if claimed != (scenario == "resumes") {
				t.Fatalf("claimed=%v want=%v", claimed, scenario == "resumes")
			}
			switch scenario {
			case "no-session", "unlisted-sender":
				if _, err := os.Stat(filepath.Join(dir, thread+".json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("session file created for a dropped message")
				}
			case "closed-session", "idle-expired", "group-no-thread":
				got := session(t, dir, thread)
				if got.ID != onDisk.ID || got.Requester != onDisk.Requester || got.State != onDisk.State || !got.LastMessage.Equal(onDisk.LastMessage) {
					t.Fatalf("session mutated: %+v want %+v", got, onDisk)
				}
			case "resumes":
				got := session(t, dir, thread)
				if got.ID != existing.ID || got.State != Chatting || !got.LastMessage.After(existing.LastMessage) {
					t.Fatalf("session not resumed: %+v", got)
				}
			}
		})
	}
}

// TestIntakeQuietAndEnd covers OF-30: every turn's prompt is tagged with who
// is speaking ("[需求人]" or, by first-appearance order, "[成员2]", ...); the
// Agent may skip a reply with the fixed quietMarker while the session still
// advances; and "结束"/"取消" closes the session without invoking the Agent,
// after which further un-@ messages in that topic are dropped silently, as
// TestIntakeThreadReplyWithoutMention's closed-session case already covers.
func TestIntakeQuietAndEnd(t *testing.T) {
	const thread = "om_x100b62338a9e9890df2772823e2edec"
	requester, member2 := "ou_REDACTED", "ou_member2"
	existing := Session{ID: "existing-session", Requester: requester, State: Chatting, LastMessage: time.Now().Add(-time.Hour)}
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(dir, thread+".json"), existing); err != nil {
		t.Fatal(err)
	}

	var prompts []string
	answers := []string{"已跟进", quietMarker}
	calls := 0
	agent := func(_ context.Context, turn Turn) (string, error) {
		prompts = append(prompts, turn.Prompt)
		if turn.Session != existing.ID || !turn.Resume {
			t.Errorf("turn=%+v", turn)
		}
		answer := answers[calls]
		calls++
		return answer, nil
	}
	var replies []string
	reply := func(_ context.Context, _, text string) error {
		replies = append(replies, text)
		return nil
	}
	g, err := New(dir, "oc_REDACTED", "ou_BOT_REDACTED", []string{requester, member2}, nil, 1, nil, "", agent, reply, noopReact, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	g.now = func() time.Time { clock = clock.Add(time.Minute); return clock }

	send := func(id, sender, text string) {
		t.Helper()
		event := recordedEvent(t, "bot-event-thread-reply-no-at")
		event.Event.Message.MessageId = ptr(id)
		event.Event.Message.Content = ptr(`{"text":"` + text + `"}`)
		event.Event.Sender.SenderId.OpenId = ptr(sender)
		if err := g.Handle(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}

	send("om_quiet_turn1", requester, "进展如何")
	if calls != 1 || prompts[0] != "[需求人] 进展如何" {
		t.Fatalf("turn1 prompt=%q calls=%d", prompts, calls)
	}
	if !slices.Equal(replies, []string{"已跟进"}) {
		t.Fatalf("turn1 replies=%q", replies)
	}
	before := session(t, dir, thread)

	replies = nil
	send("om_quiet_turn2", member2, "好的，谢谢")
	if calls != 2 || !strings.HasPrefix(prompts[1], "[成员2]") {
		t.Fatalf("turn2 prompt=%q calls=%d", prompts, calls)
	}
	if len(replies) != 0 {
		t.Fatalf("quiet marker sent a reply: %q", replies)
	}
	after := session(t, dir, thread)
	if !after.LastMessage.After(before.LastMessage) {
		t.Fatal("quiet turn did not update last message time")
	}

	send("om_quiet_turn3", requester, "结束")
	if calls != 2 {
		t.Fatalf("end command invoked the agent: calls=%d", calls)
	}
	if !slices.Equal(replies, []string{endedReply}) {
		t.Fatalf("end replies=%q", replies)
	}
	if got := session(t, dir, thread); got.State != Closed {
		t.Fatalf("session not closed: %+v", got)
	}

	send("om_quiet_turn4", member2, "还在吗")
	if calls != 2 || len(replies) != 1 {
		t.Fatalf("closed topic answered: calls=%d replies=%q", calls, replies)
	}
}

// reactionFixture serves a recorded feishu reaction response (success or
// error) over a real feishu.Client, so AddReaction exercises the exact
// captured request and response shape. It returns the client, the request
// counter, and the message ID the recording's path was captured against.
func reactionFixture(t *testing.T, name string) (*feishu.Client, *int, string) {
	t.Helper()
	b, err := os.ReadFile("../../testdata/recorded/feishu/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Request struct {
			Method string          `json:"method"`
			Path   string          `json:"path"`
			Body   json.RawMessage `json:"body"`
		} `json:"request"`
		Response struct {
			Data  json.RawMessage `json:"data"`
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(rec.Request.Path, "/open-apis/im/v1/messages/"), "/reactions")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = fmt.Fprint(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		requests++
		var got, want any
		if r.Method != rec.Request.Method || r.URL.Path != rec.Request.Path ||
			json.NewDecoder(r.Body).Decode(&got) != nil || json.Unmarshal(rec.Request.Body, &want) != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("reaction request %s %s", r.Method, r.URL.Path)
		}
		if rec.Response.Error.Code != 0 {
			if err := json.NewEncoder(w).Encode(map[string]any{"code": rec.Response.Error.Code, "msg": rec.Response.Error.Message}); err != nil {
				t.Error(err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": rec.Response.Data}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := feishu.New(feishu.Options{Endpoint: server.URL, AppID: "test", AppSecret: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return client, &requests, id
}

// TestIntakeReceiptReaction covers OF-34: a message accepted into a session
// gets a 👍 (THUMBSUP) reaction before the Agent runs, in that order, for an
// ordinary turn, and for an end/cancel command that never reaches the Agent
// at all; a denied sender, a silently dropped message, and a non-text
// message get no reaction; a reaction failure (real recorded 99992354) only
// logs a warning and the Agent still runs, with no retry. Real captures:
// bot-reaction-add.json, bot-reaction-add-invalid-message.json (PR #443).
func TestIntakeReceiptReaction(t *testing.T) {
	const thread = "om_x100b62338a9e9890df2772823e2edec"
	existing := Session{ID: "existing-session", Requester: "ou_REDACTED", State: Chatting, LastMessage: time.Now().Add(-time.Hour)}

	for _, scenario := range []string{"accepted", "quiet-marker", "end-command", "denied", "dropped", "non-text", "reaction-fails"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := "bot-reaction-add.json"
			if scenario == "reaction-fails" {
				fixture = "bot-reaction-add-invalid-message.json"
			}
			client, requests, reactionID := reactionFixture(t, fixture)

			var order []string
			react := func(ctx context.Context, id string) error {
				order = append(order, "react")
				return client.AddReaction(ctx, id)
			}
			agentCalls := 0
			agent := func(context.Context, Turn) (string, error) {
				agentCalls++
				order = append(order, "agent")
				if scenario == "quiet-marker" {
					return quietMarker, nil
				}
				return "已整理", nil
			}
			var replies []string
			reply := func(_ context.Context, _, text string) error {
				replies = append(replies, text)
				return nil
			}

			dir := t.TempDir()
			users := []string{"ou_REDACTED"}
			var event *im.P2MessageReceiveV1
			switch scenario {
			case "dropped":
				event = recordedEvent(t, "bot-event-thread-reply-no-at")
			case "end-command", "quiet-marker":
				if err := writeJSON(filepath.Join(dir, thread+".json"), existing); err != nil {
					t.Fatal(err)
				}
				event = recordedEvent(t, "bot-event-thread-reply-no-at")
				event.Event.Message.MessageId = ptr(reactionID)
				text := "继续推进"
				if scenario == "end-command" {
					text = "结束"
				}
				event.Event.Message.Content = ptr(`{"text":"` + text + `"}`)
			default:
				event = recordedEvent(t, "bot-event-message-at")
				event.Event.Message.MessageId = ptr(reactionID)
				if scenario == "denied" {
					users = []string{"ou_someone_else"}
				}
				if scenario == "non-text" {
					event.Event.Message.MessageType = ptr("image")
				}
			}

			g, err := New(dir, "oc_REDACTED", "ou_BOT_REDACTED", users, nil, 1, nil, "", agent, reply, react, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := g.Handle(context.Background(), event); err != nil {
				t.Fatal(err)
			}

			switch scenario {
			case "accepted", "reaction-fails":
				if !slices.Equal(order, []string{"react", "agent"}) || *requests != 1 || !slices.Equal(replies, []string{"已整理"}) {
					t.Fatalf("order=%v requests=%d replies=%q", order, *requests, replies)
				}
			case "quiet-marker":
				if !slices.Equal(order, []string{"react", "agent"}) || *requests != 1 || len(replies) != 0 {
					t.Fatalf("order=%v requests=%d replies=%q", order, *requests, replies)
				}
			case "end-command":
				if !slices.Equal(order, []string{"react"}) || *requests != 1 || agentCalls != 0 || !slices.Equal(replies, []string{endedReply}) {
					t.Fatalf("order=%v requests=%d agentCalls=%d replies=%q", order, *requests, agentCalls, replies)
				}
			case "denied":
				if len(order) != 0 || *requests != 0 || !slices.Equal(replies, []string{deniedReply}) {
					t.Fatalf("order=%v requests=%d replies=%q", order, *requests, replies)
				}
			case "dropped":
				if len(order) != 0 || *requests != 0 || len(replies) != 0 {
					t.Fatalf("order=%v requests=%d replies=%q", order, *requests, replies)
				}
			case "non-text":
				if len(order) != 0 || *requests != 0 || !slices.Equal(replies, []string{textOnlyReply}) {
					t.Fatalf("order=%v requests=%d replies=%q", order, *requests, replies)
				}
			}
		})
	}
}
