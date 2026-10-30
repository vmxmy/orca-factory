package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
)

func strPtr(s string) *string { return &s }

func TestIsNewIntakeTopic(t *testing.T) {
	newTopic := &im.P2MessageReceiveV1{Event: &im.P2MessageReceiveV1Data{Message: &im.EventMessage{ThreadId: strPtr("")}}}
	reply := &im.P2MessageReceiveV1{Event: &im.P2MessageReceiveV1Data{Message: &im.EventMessage{ThreadId: strPtr("omt_123")}}}
	for _, tc := range []struct {
		name string
		in   *im.P2MessageReceiveV1
		want bool
	}{
		{"nil event", nil, false},
		{"root message has no thread_id", newTopic, true},
		{"reply carries thread_id", reply, false},
	} {
		if got := isNewIntakeTopic(tc.in); got != tc.want {
			t.Errorf("%s: isNewIntakeTopic=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// discoverIntakeProjects with no factory_root configured joins a live Orca
// repository list into zero ledger rows (there is no table to read) and so
// returns no intake.Project at all (OF-42): nothing is eligible, and nothing
// in the (empty) ledger table names an unavailable project either.
func TestDiscoverIntakeProjectsFactoryRootUnconfigured(t *testing.T) {
	t.Setenv("AGENT_ORCA_BIN", fakeOrcaRepoList(t, "/repo/orca-factory", "github.com/vmxmy/orca-factory"))
	runtime := newProjectRuntime("/usr/bin/true", t.TempDir(), config.Tuning{})
	out, onboardable, pendingEnable, err := discoverIntakeProjects(context.Background(), config.Config{}, &orca.Client{}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("out=%+v, want none", out)
	}
	// ReasonFactoryRootUnconfigured, not ReasonOnboardable or
	// ReasonProjectDisabled: without a factory_root, discovery cannot tell
	// onboardable or pending-enable repos apart from any other repo, so
	// OF-49's and OF-54's sets both stay empty too.
	if len(onboardable) != 0 {
		t.Fatalf("onboardable=%+v, want none", onboardable)
	}
	if len(pendingEnable) != 0 {
		t.Fatalf("pendingEnable=%+v, want none", pendingEnable)
	}
}

// TestResolvePendingEnable is OF-54: resolvePendingEnable keeps only the
// ReasonProjectDisabled rejections joined with their ledger row (Prefix and
// Name, unlike resolveOnboardable's local directory name), and drops
// repositories rejected for any other reason, or one without a matching row.
func TestResolvePendingEnable(t *testing.T) {
	result := projects.Result{Rejected: []projects.Rejection{
		{Path: "/repo/dujiao-next", Remote: "github.com/dujiao-next/dujiao-next", Reason: projects.ReasonProjectDisabled},
		{Path: "/repo/orca-factory", Remote: "github.com/vmxmy/orca-factory", Reason: projects.ReasonCheckFailed},
		{Path: "/repo/no-row", Remote: "github.com/vmxmy/no-row", Reason: projects.ReasonProjectDisabled},
	}}
	rows := []issues.LedgerProject{
		{Prefix: "DJ", Name: "dujiao-next", Remote: "github.com/dujiao-next/dujiao-next", Enabled: false},
	}
	got := resolvePendingEnable(result, rows)
	want := []intake.PendingEnable{{Prefix: "DJ", Name: "dujiao-next", Remote: "github.com/dujiao-next/dujiao-next", Path: "/repo/dujiao-next"}}
	if !slices.Equal(got, want) {
		t.Fatalf("resolvePendingEnable=%+v, want %+v", got, want)
	}
}

// TestResolveOnboardable is OF-49: resolveOnboardable keeps only the
// ReasonOnboardable rejections, naming each by its local checkout directory,
// and drops repositories rejected for any other reason (e.g. "check 失败"
// below), which are not onboardable even though they are also not eligible.
func TestResolveOnboardable(t *testing.T) {
	result := projects.Result{Rejected: []projects.Rejection{
		{Path: "/repo/ziikoo-control-plane", Remote: "github.com/vmxmy/ziikoo-control-plane", Reason: projects.ReasonOnboardable},
		{Path: "/repo/dujiao-next", Remote: "github.com/dujiao-next/dujiao-next", Reason: projects.ReasonOnboardable},
		{Path: "/repo/orca-factory", Remote: "github.com/vmxmy/orca-factory", Reason: projects.ReasonCheckFailed},
	}}
	got := resolveOnboardable(result)
	want := []intake.Onboardable{
		{Name: "ziikoo-control-plane", Remote: "github.com/vmxmy/ziikoo-control-plane", Path: "/repo/ziikoo-control-plane"},
		{Name: "dujiao-next", Remote: "github.com/dujiao-next/dujiao-next", Path: "/repo/dujiao-next"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("resolveOnboardable=%+v, want %+v", got, want)
	}
}

// TestResolveIntakeProjectsPartialTierFailure covers OF-48: among three
// eligible projects, two carry a non-claude AGENT_TIER_STRONG tier (no
// AGENT_ROLE_INTAKE set). Each of those two must fail to resolve on its own
// and be listed unavailable with the tier reason, while the one claude-tier
// project still resolves — a single bad tier must never take down the whole
// discovery round (the production defect: it made every other project
// disappear from the picker, and the gateway could not route at all).
func TestResolveIntakeProjectsPartialTierFailure(t *testing.T) {
	runtime := newProjectRuntime("/usr/bin/true", t.TempDir(), config.Tuning{})
	eligible := func(prefix, remote, tier string) projects.Project {
		return projects.Project{Path: "/repo/" + prefix, Remote: remote, Prefix: prefix, Name: prefix + "-project",
			Repository: config.Repository{Env: map[string]string{
				"AGENT_ISSUE_BACKEND": "github",
				"AGENT_GH_REPO":       "vmxmy/" + prefix,
				"AGENT_TIER_STRONG":   tier,
			}}}
	}
	result := projects.Result{Eligible: []projects.Project{
		eligible("OF", "github.com/vmxmy/orca-factory", "claude:claude-opus-5-5:high"),
		eligible("ZY", "github.com/vmxmy/zhiyun-ai-dashboard", "codex:gpt-6:high"),
		eligible("AN", "github.com/vmxmy/ansible", "codex:gpt-6:high"),
	}}
	rows := []issues.LedgerProject{
		{Prefix: "OF", Name: "orca-factory", Remote: "github.com/vmxmy/orca-factory", Enabled: true},
		{Prefix: "ZY", Name: "zhiyun-ai-dashboard", Remote: "github.com/vmxmy/zhiyun-ai-dashboard", Enabled: true},
		{Prefix: "AN", Name: "ansible", Remote: "github.com/vmxmy/ansible", Enabled: true},
	}
	out := resolveIntakeProjects(context.Background(), result, rows, runtime)
	if len(out) != 3 {
		t.Fatalf("out=%+v, want 3 projects", out)
	}
	var usable []string
	unavailable := map[string]string{}
	for _, p := range out {
		if p.Repo != "" {
			usable = append(usable, p.Prefix)
			continue
		}
		unavailable[p.Prefix] = p.Reason
	}
	if !slices.Equal(usable, []string{"OF"}) {
		t.Fatalf("usable=%v, want only OF", usable)
	}
	const wantReason = "需求助手档位不是 claude（AGENT_ROLE_INTAKE）"
	if unavailable["ZY"] != wantReason || unavailable["AN"] != wantReason {
		t.Fatalf("unavailable=%+v, want %q for ZY and AN", unavailable, wantReason)
	}
}

// A project's chat comes from its current config on every discovery round: the
// runtime caches the rest of the project, but a chat set later must take effect
// without restarting the gateway.
func TestResolveIntakeProjectFollowsChatConfig(t *testing.T) {
	runtime := newProjectRuntime("/usr/bin/true", t.TempDir(), config.Tuning{})
	project := func(chat string) projects.Project {
		return projects.Project{Path: "/repo/of", Remote: "github.com/vmxmy/orca-factory", Prefix: "OF", Name: "orca-factory",
			Repository: config.Repository{Env: map[string]string{
				"AGENT_ISSUE_BACKEND":      "github",
				"AGENT_GH_REPO":            "vmxmy/orca-factory",
				"AGENT_TIER_STRONG":        "claude:claude-opus-5-5:high",
				"AGENT_FEISHU_NOTIFY_CHAT": chat,
			}}}
	}
	for _, chat := range []string{"", "oc_OF"} {
		got, err := runtime.resolve(context.Background(), project(chat))
		if err != nil {
			t.Fatal(err)
		}
		if got.Chat != chat {
			t.Fatalf("Chat=%q, want %q", got.Chat, chat)
		}
	}
}

// A chat an unusable project declares is still claimed. This runs real profiles
// through Discover and resolveIntakeProjects into the gateway: an unusable
// project dropping out on the way would hand the other project a chat two
// repositories declared, and leave the unusable project's own chat silent.
func TestProjectChatClaimsSurviveDiscovery(t *testing.T) {
	const of, an = "github.com/vmxmy/of", "github.com/vmxmy/an"
	ledger := "AGENT_ISSUE_BACKEND=feishu\nAGENT_FEISHU_BASE=https://example.feishu.cn/base/b\n"
	for _, tc := range []struct {
		name, otherProfile, chat, wantReply string
		otherEnabled, wantAgent             bool
	}{
		{name: "sole owner binds", otherProfile: ledger + "AGENT_TIER_STRONG=claude:claude-opus-5-5:high\n", chat: "oc_OF", wantAgent: true},
		{name: "disabled project with no base shares the chat", otherProfile: "AGENT_ISSUE_BACKEND=feishu\nAGENT_FEISHU_NOTIFY_CHAT=oc_OF\n", chat: "oc_OF"},
		{name: "project on another backend shares the chat", otherProfile: "AGENT_ISSUE_BACKEND=github\nAGENT_FEISHU_NOTIFY_CHAT=oc_OF\n", otherEnabled: true, chat: "oc_OF"},
		{name: "project with an invalid limit shares the chat", otherProfile: ledger + "AGENT_MAX_IN_FLIGHT=0\nAGENT_FEISHU_NOTIFY_CHAT=oc_OF\n", otherEnabled: true, chat: "oc_OF"},
		{name: "project failing to resolve shares the chat", otherProfile: ledger + "AGENT_TIER_STRONG=codex:gpt-6:high\nAGENT_FEISHU_NOTIFY_CHAT=oc_OF\n", otherEnabled: true, chat: "oc_OF"},
		{name: "unusable project's own chat explains itself", otherProfile: "AGENT_ISSUE_BACKEND=feishu\nAGENT_FEISHU_NOTIFY_CHAT=oc_AN\n", chat: "oc_AN", wantReply: "项目 AN 不可用：" + string(projects.ReasonProjectDisabled)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, profile string) string {
				dir := filepath.Join(root, name, "scripts", "agent-gates")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "agent-graph.env"), []byte(profile), 0o600); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(root, name)
			}
			ofPath := write("of", ledger+"AGENT_FEISHU_NOTIFY_CHAT=oc_OF\n")
			anPath := write("an", tc.otherProfile)
			rows := []issues.LedgerProject{{Prefix: "OF", Name: "of", Remote: of, Enabled: true}, {Prefix: "AN", Name: "an", Remote: an, Enabled: tc.otherEnabled}}
			check := func(context.Context, string) ([]string, error) { return nil, nil }
			result := projects.Discover(context.Background(), []orca.Repo{{Path: ofPath, CanonicalKey: of}, {Path: anPath, CanonicalKey: an}}, rows, check, nil)
			runtime := newProjectRuntime("/usr/bin/true", t.TempDir(), config.Tuning{})
			runtime.projects["OF"] = intake.Project{Prefix: "OF", Name: "of", Remote: of, Enabled: true, Repo: ofPath}
			agentRuns := 0
			agent := func(context.Context, intake.Turn) (string, error) { agentRuns++; return "已整理", nil }
			var replies []string
			send := func(_ context.Context, _, text string) error { replies = append(replies, text); return nil }
			react := func(context.Context, string) error { return nil }
			gateway, err := intake.New(t.TempDir(), "oc_SHARED", "ou_BOT_REDACTED", []string{"ou_REDACTED"}, nil, 1, resolveIntakeProjects(context.Background(), result, rows, runtime), "", agent, send, react, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile("../../testdata/recorded/feishu/bot-event-message-at.json")
			if err != nil {
				t.Fatal(err)
			}
			var recording struct {
				Event *im.P2MessageReceiveV1 `json:"event"`
			}
			if err := json.Unmarshal(raw, &recording); err != nil {
				t.Fatal(err)
			}
			recording.Event.Event.Message.ChatId = strPtr(tc.chat)
			recording.Event.Event.Message.Content = strPtr(`{"text":"@_user_1 帮我整理"}`)
			if err := gateway.Handle(context.Background(), recording.Event); err != nil {
				t.Fatal(err)
			}
			if (agentRuns == 1) != tc.wantAgent {
				t.Fatalf("agent runs=%d, want agent=%v, replies=%q", agentRuns, tc.wantAgent, replies)
			}
			if !tc.wantAgent && (tc.wantReply == "") != (len(replies) == 0) {
				t.Fatalf("replies=%q, want %q", replies, tc.wantReply)
			}
			if tc.wantReply != "" && (len(replies) != 1 || replies[0] != tc.wantReply) {
				t.Fatalf("replies=%q, want %q", replies, tc.wantReply)
			}
		})
	}
}

func TestFeishuGatewayConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range gatewayKeys {
		t.Setenv(key, "")
	}
	_, err := loadGatewaySettings()
	if err == nil {
		t.Fatal("missing config accepted")
	}
	for _, key := range gatewayKeys[:5] {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("missing %s: %v", key, err)
		}
	}
	dir := filepath.Join(os.Getenv("HOME"), ".config/orca-factory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	content := ""
	for _, key := range gatewayKeys[:5] {
		content += key + "=file-value\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "driver.env"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_FEISHU_APP_SECRET", "environment-secret")
	settings, err := loadGatewaySettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings["AGENT_FEISHU_APP_SECRET"] != "environment-secret" || settings["AGENT_FEISHU_APP_ID"] != "file-value" {
		t.Fatal("wrong environment precedence")
	}
	if commands["feishu-gateway"] == nil {
		t.Fatal("command not registered")
	}
}

func TestFeishuGatewayInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config/systemd/user")
	if err := os.MkdirAll(filepath.Join(dir, "orca-factory-driver.service.d"), 0755); err != nil {
		t.Fatal(err)
	}
	proxy := "[Service]\nEnvironment=https_proxy=http://127.0.0.1:7890\n"
	if err := os.WriteFile(filepath.Join(dir, "orca-factory-driver.service.d/proxy.conf"), []byte(proxy), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := installWith([]string{"--dry-run"}, &out, &installFakeRunner{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "orca-factory-feishu-gateway.service") {
		t.Fatal("dry-run omits gateway")
	}
	if err := installWith(nil, &out, &installFakeRunner{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "orca-factory-feishu-gateway.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "orca-factory-driver feishu-gateway") || !strings.Contains(string(b), "EnvironmentFile=-%h/.config/orca-factory/driver.env") {
		t.Fatalf("wrong unit %s", b)
	}
	b, err = os.ReadFile(filepath.Join(dir, "orca-factory-feishu-gateway.service.d/proxy.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != proxy {
		t.Fatal("proxy drop-in differs")
	}
}

// Bootstrap and WebSocket transport are synthetic; message contracts are
// exercised separately against PR #420 recordings.
func TestFeishuGatewayReconnectAndShutdown(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_DRIVER_STATE_ROOT", t.TempDir())
	t.Setenv("AGENT_INTAKE_CLAUDE", "/bin/true")
	for _, key := range gatewayKeys[:5] {
		t.Setenv(key, "test")
	}
	for path, content := range map[string]string{
		filepath.Join(home, ".config/orca-factory/driver.toml"):    "repositories = [\"" + repo + "\"]\n",
		filepath.Join(repo, "scripts/agent-gates/agent-graph.env"): "AGENT_ISSUE_BACKEND=feishu\nAGENT_TIER_STRONG='claude:claude-opus-5-5:high'\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	accepted := make(chan *websocket.Conn, 4)
	upgrader := websocket.Upgrader{}
	socket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		accepted <- conn
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer socket.Close()
	bootstrap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&ws.EndpointResp{Code: 0, Data: &ws.Endpoint{Url: "ws" + strings.TrimPrefix(socket.URL, "http") + "?device_id=test&service_id=1", ClientConfig: &ws.ClientConfig{ReconnectCount: -1, ReconnectInterval: 0, ReconnectNonce: 0, PingInterval: 3600}}}); err != nil {
			t.Error(err)
		}
	}))
	defer bootstrap.Close()
	t.Setenv("AGENT_FEISHU_ENDPOINT", bootstrap.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runFeishuGateway(ctx) }()
	next := func() *websocket.Conn {
		t.Helper()
		select {
		case conn := <-accepted:
			return conn
		case err := <-done:
			t.Fatalf("gateway exited: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("gateway did not connect")
		}
		return nil
	}
	first := next()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := next()
	defer func() { _ = second.Close() }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	lock, err := flock.TryLock(filepath.Join(logStateRoot(), "intake/gateway.lock"))
	if err != nil {
		t.Fatalf("shutdown retained lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
