package intake

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// recordedLedgerProject reads the real orca-factory row recorded in
// bot-project-records.json: today's only production project.
func recordedLedgerProject(t *testing.T) Project {
	t.Helper()
	b, err := os.ReadFile("../../testdata/recorded/feishu/bot-project-records.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording struct {
		Response struct {
			Data struct {
				Fields []string `json:"fields"`
				Data   [][]any  `json:"data"`
			} `json:"data"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &recording); err != nil {
		t.Fatal(err)
	}
	fields, row := recording.Response.Data.Fields, recording.Response.Data.Data[0]
	at := func(name string) any { return row[slices.Index(fields, name)] }
	return Project{Name: at("名称").(string), Enabled: at("启用").(bool), Remote: at("仓库远端").(string), Prefix: at("编号前缀").(string)}
}

// TestProjectChatBindsThreads covers OF-304: a project's own chat opens every
// thread on that project, while the shared chat keeps the picker and a chat no
// single project claims stays ignored.
func TestProjectChatBindsThreads(t *testing.T) {
	zy := Project{Name: "zhiyun", Remote: "github.com/vmxmy/zhiyun", Prefix: "ZY", Enabled: true, Repo: "/repo/zy", Chat: "oc_ZY"}
	an := Project{Name: "ansible", Remote: "github.com/vmxmy/ansible", Prefix: "AN", Enabled: true, Repo: "/repo/an", Chat: "oc_AN"}
	off := Project{Name: "retired", Remote: "github.com/vmxmy/retired", Prefix: "OLD", Repo: "/repo/old", Chat: "oc_OLD"}
	twinA := Project{Name: "twin-a", Remote: "github.com/vmxmy/twin-a", Prefix: "TA", Enabled: true, Repo: "/repo/ta", Chat: "oc_TWIN"}
	twinB := Project{Name: "twin-b", Remote: "github.com/vmxmy/twin-b", Prefix: "TB", Enabled: true, Repo: "/repo/tb", Chat: "oc_TWIN"}
	unset := Project{Name: "unset", Remote: "github.com/vmxmy/unset", Prefix: "UN", Enabled: true, Repo: "/repo/un"}
	for _, tc := range []struct {
		name, chat, text, wantPrefix, wantPrompt, wantReply string
	}{
		{name: "project chat binds without a picker", chat: "oc_ZY", text: "@_user_1 帮我整理", wantPrefix: "ZY", wantPrompt: "[需求人] 帮我整理", wantReply: "已整理"},
		{name: "own marker is stripped", chat: "oc_ZY", text: "@_user_1 #ZY 帮我整理", wantPrefix: "ZY", wantPrompt: "[需求人] 帮我整理", wantReply: "已整理"},
		{name: "other project marker is refused", chat: "oc_ZY", text: "@_user_1 #AN 帮我整理", wantReply: "本群只接项目 ZY zhiyun 的需求，其他项目请到对应的群"},
		{name: "disabled project is refused", chat: "oc_OLD", text: "@_user_1 帮我整理", wantReply: "项目 OLD 不可用：未启用"},
		{name: "unknown chat is ignored", chat: "oc_ELSE", text: "@_user_1 帮我整理"},
		{name: "chat claimed twice is ignored", chat: "oc_TWIN", text: "@_user_1 帮我整理"},
		{name: "shared chat marker still routes", chat: "oc_REDACTED", text: "@_user_1 #AN 帮我整理", wantPrefix: "AN", wantPrompt: "[需求人] 帮我整理", wantReply: "已整理"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []Turn
			agent := func(_ context.Context, turn Turn) (string, error) {
				calls = append(calls, turn)
				return "已整理", nil
			}
			var replies []string
			send := func(_ context.Context, _, text string) error { replies = append(replies, text); return nil }
			g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, []Project{zy, an, off, twinA, twinB, unset}, "", agent, send, noopReact, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			event := recordedEvent(t, "bot-event-message-at")
			event.Event.Message.MessageId = ptr("om_chat1")
			event.Event.Message.ChatId = ptr(tc.chat)
			event.Event.Message.Content = ptr(`{"text":"` + tc.text + `"}`)
			if err := g.Handle(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			if tc.wantPrefix == "" && len(calls) != 0 {
				t.Fatalf("agent ran: %+v", calls)
			}
			if tc.wantPrefix != "" && (len(calls) != 1 || calls[0].Project.Prefix != tc.wantPrefix || calls[0].Prompt != tc.wantPrompt) {
				t.Fatalf("calls=%+v", calls)
			}
			want := []string{}
			if tc.wantReply != "" {
				want = append(want, tc.wantReply)
			}
			if !slices.Equal(append([]string{}, replies...), want) {
				t.Fatalf("replies=%q want %q", replies, want)
			}
		})
	}
}

// TestIntakeProjectSelection covers OF-32's explicit marker: a brand-new
// thread's first message that starts with #<编号前缀> routes immediately with
// the marker stripped. A project that is not enabled, or has no local
// repository match, is rejected without starting the Agent. A first message
// without a marker gets the project picker card (TestIntakeProjectPicker).
func TestIntakeProjectSelection(t *testing.T) {
	of := recordedLedgerProject(t)
	of.Repo = "/repo/of"
	zii := Project{Name: "ziikoo", Remote: "github.com/vmxmy/ziikoo", Prefix: "ZII", Enabled: true, Repo: "/repo/zii"}
	disabled := Project{Name: "retired", Remote: "github.com/vmxmy/retired", Prefix: "OLD", Enabled: false, Repo: "/repo/old"}
	unmatched := Project{Name: "no-checkout", Remote: "github.com/vmxmy/elsewhere", Prefix: "ELS", Enabled: true}
	projects := []Project{of, zii, disabled, unmatched}

	newThread := func(id, text string) *im.P2MessageReceiveV1 {
		event := recordedEvent(t, "bot-event-message-at")
		event.Event.Message.MessageId = ptr(id)
		event.Event.Message.Content = ptr(`{"text":"` + text + `"}`)
		return event
	}

	t.Run("explicit marker strips it and routes immediately", func(t *testing.T) {
		var calls []Turn
		agent := func(_ context.Context, turn Turn) (string, error) {
			calls = append(calls, turn)
			return "已整理", nil
		}
		var replies []string
		send := func(_ context.Context, _, text string) error { replies = append(replies, text); return nil }
		g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, projects, "", agent, send, noopReact, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		event := newThread("om_marker1", "@_user_1 #OF 帮我整理这个需求")
		if err := g.Handle(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 || calls[0].Project.Prefix != "OF" || calls[0].Prompt != "[需求人] 帮我整理这个需求" || calls[0].Resume {
			t.Fatalf("calls=%+v", calls)
		}
		if len(replies) != 1 || replies[0] != "已整理" {
			t.Fatalf("replies=%q", replies)
		}
		if got := session(t, g.dir, "om_marker1"); got.Project != "OF" || got.PendingPrompt != "" {
			t.Fatalf("session=%+v", got)
		}
	})

	for _, tc := range []struct{ name, marker string }{{"disabled project", "#OLD"}, {"project with no local repo", "#ELS"}} {
		t.Run(tc.name+" is rejected", func(t *testing.T) {
			var calls []Turn
			agent := func(_ context.Context, turn Turn) (string, error) {
				calls = append(calls, turn)
				return "已整理", nil
			}
			var replies []string
			send := func(_ context.Context, _, text string) error { replies = append(replies, text); return nil }
			g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, projects, "", agent, send, noopReact, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			event := newThread("om_reject", "@_user_1 "+tc.marker+" 帮我整理这个需求")
			if err := g.Handle(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 0 {
				t.Fatalf("agent called for a rejected project: %+v", calls)
			}
			if len(replies) != 1 {
				t.Fatalf("replies=%q", replies)
			}
		})
	}
}

// TestIntakeProjectSelectionDiscoveryReason covers OF-42: a project the
// discovery package rejected (e.g. its pipeline check failed) carries that
// specific reason, and selecting it by marker replies with the reason
// instead of the generic "本机未配置仓库" message.
func TestIntakeProjectSelectionDiscoveryReason(t *testing.T) {
	rejected := Project{Name: "flaky", Remote: "github.com/vmxmy/flaky", Prefix: "FLK", Enabled: true, Reason: "check 失败"}
	g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, []Project{rejected}, "", func(context.Context, Turn) (string, error) {
		t.Fatal("agent called for a rejected project")
		return "", nil
	}, func(_ context.Context, _, text string) error {
		if !strings.Contains(text, "check 失败") {
			t.Errorf("reply=%q, want it to contain the discovery reason", text)
		}
		return nil
	}, noopReact, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	event := recordedEvent(t, "bot-event-message-at")
	event.Event.Message.MessageId = ptr("om_reject_reason")
	event.Event.Message.Content = ptr(`{"text":"@_user_1 #FLK 帮我整理这个需求"}`)
	if err := g.Handle(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

// TestSetProjects covers OF-42's merge semantics: a fresh discovery result
// replaces a same-prefix project but a stale one absent from it survives, so
// a thread that already committed to a project keeps working even if a
// later rediscovery (triggered by a different topic) no longer reports it.
func TestSetProjects(t *testing.T) {
	g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1,
		[]Project{{Prefix: "OF", Name: "orca-factory", Enabled: true, Repo: "/repo/of"}},
		"", noopAgent, noopReply, noopReact, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SetProjects([]Project{{Prefix: "ZII", Name: "ziikoo", Enabled: true, Repo: "/repo/zii"}}); err != nil {
		t.Fatal(err)
	}
	of, ok := g.findProject("OF", "")
	if !ok || of.Repo != "/repo/of" {
		t.Fatalf("stale project OF dropped: %+v", g.projectsSnapshot())
	}
	zii, ok := g.findProject("ZII", "")
	if !ok || zii.Repo != "/repo/zii" {
		t.Fatalf("fresh project ZII missing: %+v", g.projectsSnapshot())
	}
	if err := g.SetProjects([]Project{{Prefix: "OF", Name: "orca-factory", Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	of, ok = g.findProject("OF", "")
	if !ok || of.Enabled {
		t.Fatalf("fresh OF did not override stale: %+v", of)
	}
	if err := g.SetProjects([]Project{{Prefix: "DUP"}, {Prefix: "DUP"}}); err == nil {
		t.Fatal("duplicate prefix accepted")
	}
}

// TestSetOnboardable is OF-49: a fresh discovery round's onboardable set
// simply replaces the prior one, since no thread ever commits to one of
// these the way SetProjects preserves a stale project a thread already uses.
func TestSetOnboardable(t *testing.T) {
	g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, nil, "", noopAgent, noopReply, noopReact, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.onboardableSnapshot(); len(got) != 0 {
		t.Fatalf("onboardable=%+v, want none before SetOnboardable", got)
	}
	first := []Onboardable{{Name: "ziikoo-control-plane", Remote: "github.com/vmxmy/ziikoo-control-plane"}, {Name: "dujiao-next", Remote: "github.com/dujiao-next/dujiao-next"}}
	g.SetOnboardable(first)
	if got := g.onboardableSnapshot(); !reflect.DeepEqual(got, first) {
		t.Fatalf("onboardable=%+v, want %+v", got, first)
	}
	g.SetOnboardable(nil)
	if got := g.onboardableSnapshot(); len(got) != 0 {
		t.Fatalf("onboardable=%+v, want cleared", got)
	}
}

func noopAgent(context.Context, Turn) (string, error) { return "", nil }
func noopReply(context.Context, string, string) error { return nil }

func TestProjectMarkerOnlyAtMessageStart(t *testing.T) {
	for _, tc := range []struct{ in, prefix, name, rest string }{
		{"#OF 给 status 加一列", "OF", "", "给 status 加一列"},
		{"  项目:orca-factory 加一列", "", "orca-factory", "加一列"},
		{"和 #OF-17 有关的需求", "", "", "和 #OF-17 有关的需求"},
		{"#OF-17 卡住了", "", "", "#OF-17 卡住了"},
		{"参考 项目:旧系统 的做法", "", "", "参考 项目:旧系统 的做法"},
	} {
		prefix, name, rest := parseProjectMarker(tc.in)
		if prefix != tc.prefix || name != tc.name || rest != tc.rest {
			t.Errorf("parseProjectMarker(%q) = %q %q %q, want %q %q %q", tc.in, prefix, name, rest, tc.prefix, tc.name, tc.rest)
		}
	}
}
