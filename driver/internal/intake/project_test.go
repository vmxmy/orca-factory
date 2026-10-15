package intake

import (
	"context"
	"encoding/json"
	"os"
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

// TestIntakeProjectSelection covers OF-32: a brand-new thread's first message
// picks a project by an explicit #<编号前缀> marker, by the configured group
// default (AGENT_FEISHU_INTAKE_PROJECT), or — failing both — the gateway
// lists the enabled, locally available projects and waits for the requester
// to pick one by index or prefix before any agent turn runs, using the
// original first message as the first round's input. A project that is not
// enabled, or has no local repository match, is rejected without starting
// the Agent.
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
	pick := func(id, root, text string) *im.P2MessageReceiveV1 {
		event := recordedEvent(t, "bot-event-thread-reply-no-at")
		event.Event.Message.MessageId = ptr(id)
		event.Event.Message.RootId = ptr(root)
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
		g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, projects, "", agent, send, noopReact)
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

	t.Run("group default is used when no marker", func(t *testing.T) {
		var calls []Turn
		agent := func(_ context.Context, turn Turn) (string, error) {
			calls = append(calls, turn)
			return "已整理", nil
		}
		send := func(context.Context, string, string) error { return nil }
		g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, projects, of.Remote, agent, send, noopReact)
		if err != nil {
			t.Fatal(err)
		}
		event := newThread("om_default1", "@_user_1 帮我整理这个需求")
		if err := g.Handle(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 || calls[0].Project.Prefix != "OF" || calls[0].Prompt != "[需求人] 帮我整理这个需求" {
			t.Fatalf("calls=%+v", calls)
		}
	})

	t.Run("no marker and no default lists projects, then routes on pick", func(t *testing.T) {
		var calls []Turn
		agent := func(_ context.Context, turn Turn) (string, error) {
			calls = append(calls, turn)
			return "已整理", nil
		}
		var replies []string
		send := func(_ context.Context, _, text string) error { replies = append(replies, text); return nil }
		g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, projects, "", agent, send, noopReact)
		if err != nil {
			t.Fatal(err)
		}
		first := newThread("om_list1", "@_user_1 帮我整理这个需求")
		if err := g.Handle(context.Background(), first); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 0 {
			t.Fatalf("agent called before project chosen: %+v", calls)
		}
		if len(replies) != 1 || !strings.Contains(replies[0], "OF "+of.Name) || !strings.Contains(replies[0], "ZII ziikoo") || strings.Contains(replies[0], "OLD") || strings.Contains(replies[0], "ELS") {
			t.Fatalf("list reply=%q", replies)
		}
		if got := session(t, g.dir, "om_list1"); got.Project != "" || got.PendingPrompt != "帮我整理这个需求" {
			t.Fatalf("pending session=%+v", got)
		}

		bad := pick("om_list_bad", "om_list1", "99")
		if err := g.Handle(context.Background(), bad); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 0 || len(replies) != 2 {
			t.Fatalf("invalid pick: calls=%+v replies=%q", calls, replies)
		}

		good := pick("om_list_good", "om_list1", "1")
		if err := g.Handle(context.Background(), good); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 || calls[0].Project.Prefix != "OF" || calls[0].Prompt != "[需求人] 帮我整理这个需求" || calls[0].Resume {
			t.Fatalf("calls after pick=%+v", calls)
		}
		if got := session(t, g.dir, "om_list1"); got.Project != "OF" || got.PendingPrompt != "" {
			t.Fatalf("resolved session=%+v", got)
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
			g, err := New(t.TempDir(), "oc_REDACTED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, projects, "", agent, send, noopReact)
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
	}, noopReact)
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
		"", noopAgent, noopReply, noopReact)
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
