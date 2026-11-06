package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/vmxmy/orca-factory/driver/internal/task"
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

// A project whose own env sets AGENT_INTAKE_PI gets the pi execution path
// even when the gateway-level AGENT_INTAKE_PI is empty: the tier selects the
// path per project, and the binary resolves independently of the gateway
// setting.
func TestResolveIntakeProjectPiTierWithoutGatewayTier(t *testing.T) {
	binDir := t.TempDir()
	piBin := filepath.Join(binDir, "pi")
	if err := os.WriteFile(piBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	runtime := newProjectRuntime("/usr/bin/true", t.TempDir(), config.Tuning{})
	runtime.setPi("", t.TempDir(), t.TempDir())
	project := projects.Project{Path: "/repo/of", Remote: "github.com/vmxmy/orca-factory", Prefix: "OF", Name: "orca-factory",
		Repository: config.Repository{Env: map[string]string{
			"AGENT_ISSUE_BACKEND": "github",
			"AGENT_GH_REPO":       "vmxmy/orca-factory",
			"AGENT_TIER_STRONG":   "claude:claude-opus-5-5:high",
			"AGENT_INTAKE_PI":     "pi:anthropic/claude-sonnet-5-5:high",
		}}}
	if _, err := runtime.resolve(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	runner := runtime.built["OF"]
	if runner == nil {
		t.Fatal("no runner built for OF")
	}
	if runner.PiBinary != piBin {
		t.Fatalf("PiBinary=%q, want resolved %q (project tier must select the pi path)", runner.PiBinary, piBin)
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

// draftTicketStore fakes the issues.Store parts the draft_ticket submission
// uses; every other Store method stays nil and panics if called.
type draftTicketStore struct {
	issues.Store
	tasks    []issues.Task
	created  []issues.CreateInput
	comments map[string][]string
}

func (f *draftTicketStore) List(context.Context) ([]issues.Task, error) { return f.tasks, nil }

func (f *draftTicketStore) Read(_ context.Context, id string) (issues.Task, error) {
	for _, t := range f.tasks {
		if t.ID == id || t.Metadata.ID == id || t.Metadata.Identifier == id {
			return t, nil
		}
	}
	return issues.Task{}, errors.New("task not found")
}

func (f *draftTicketStore) Create(_ context.Context, in issues.CreateInput) (issues.Task, error) {
	f.created = append(f.created, in)
	id := fmt.Sprintf("uuid-%d", len(f.created))
	identifier := fmt.Sprintf("OF-%d", 100+len(f.created))
	return issues.Task{Title: in.Title, URL: "https://ledger.example/" + id,
		Issue:    task.Issue{ID: id, Identifier: identifier},
		Metadata: issues.Metadata{ID: id, Identifier: identifier, State: in.State}}, nil
}

func (f *draftTicketStore) CreateComment(_ context.Context, id, body string) (task.Comment, error) {
	if f.comments == nil {
		f.comments = map[string][]string{}
	}
	f.comments[id] = append(f.comments[id], body)
	return task.Comment{ID: "c1", Body: body}, nil
}

func managedTask(id, identifier, title, state string, complete bool) issues.Task {
	return issues.Task{Title: title,
		Issue:    task.Issue{ID: id, Identifier: identifier},
		Metadata: issues.Metadata{ID: id, Identifier: identifier, State: state}, Complete: complete}
}

const draftTicketAnswer = "需求已明确，直接建票：\n\n" +
	"```draft_ticket\n" +
	"{\n  \"title\": \"网关增加部署重试\",\n" +
	"  \"todo\": [\"网关部署失败时按固定间隔重试\"],\n" +
	"  \"check\": \"cd driver && go test -count=1 -run TestDeployRetry ./internal/deploy\",\n" +
	"  \"scope\": \"driver/internal/deploy/retry.go\",\n" +
	"  \"blockers\": [],\n  \"related\": []\n}\n" +
	"```"

func TestDraftTicketParse(t *testing.T) {
	d, problems, found := parseDraftTicket(draftTicketAnswer)
	if !found || problems != nil {
		t.Fatalf("found=%v problems=%v, want a clean parse", found, problems)
	}
	if d.Title != "网关增加部署重试" || len(d.Todo) != 1 || d.Check == "" || d.Scope == "" {
		t.Fatalf("draft=%+v, want the fenced fields", d)
	}
	if d.Blockers == nil || d.Related == nil {
		t.Fatalf("empty arrays must parse as non-nil: %+v", d)
	}
	if _, problems, found := parseDraftTicket("这条回复没有草稿块"); found || problems != nil {
		t.Fatalf("found=%v problems=%v, want no draft", found, problems)
	}
	_, problems, found = parseDraftTicket("```draft_ticket\n{bad json}\n```")
	if !found || len(problems) == 0 {
		t.Fatalf("found=%v problems=%v, want a malformed-JSON problem", found, problems)
	}
}

func TestDraftTicketValidate(t *testing.T) {
	store := &draftTicketStore{tasks: []issues.Task{
		managedTask("uuid-1", "OF-1", "网关重试退避", "Todo", true),
		managedTask("uuid-2", "OF-2", "未纳管的票", "Todo", false),
	}}
	valid := draftTicketWire{Title: "新票", Todo: []string{"做一件事"}, Check: "go test ./...", Scope: "driver/",
		Blockers: []string{"OF-1"}, Related: []string{}}
	if problems := draftTicketFieldProblems(valid); len(problems) != 0 {
		t.Fatalf("field problems=%v, want none", problems)
	}
	blockers, related, problems := draftTicketDependencies(context.Background(), store, valid)
	if len(problems) != 0 {
		t.Fatalf("dep problems=%v, want none", problems)
	}
	if !slices.Equal(blockers, []string{"uuid-1"}) || !slices.Equal(related, []string{}) {
		t.Fatalf("blockers=%v related=%v, want canonical resolved IDs", blockers, related)
	}

	cases := []struct {
		name string
		d    draftTicketWire
		want string
	}{
		{"missing todo", draftTicketWire{Title: "新票", Check: "c", Scope: "s", Blockers: []string{}, Related: []string{}}, "todo"},
		{"missing check", draftTicketWire{Title: "新票", Todo: []string{"t"}, Scope: "s", Blockers: []string{}, Related: []string{}}, "check"},
		{"nil blockers", draftTicketWire{Title: "新票", Todo: []string{"t"}, Check: "c", Scope: "s", Related: []string{}}, "完整数组"},
		{"duplicate refs", draftTicketWire{Title: "新票", Todo: []string{"t"}, Check: "c", Scope: "s", Blockers: []string{"OF-1"}, Related: []string{"OF-1"}}, "重复"},
		{"missing dep", draftTicketWire{Title: "新票", Todo: []string{"t"}, Check: "c", Scope: "s", Blockers: []string{"OF-99"}, Related: []string{}}, "不存在"},
		{"incomplete dep", draftTicketWire{Title: "新票", Todo: []string{"t"}, Check: "c", Scope: "s", Blockers: []string{"OF-2"}, Related: []string{}}, "尚未纳管"},
	}
	for _, tc := range cases {
		if problems := draftTicketFieldProblems(tc.d); len(problems) > 0 {
			if !strings.Contains(strings.Join(problems, ";"), tc.want) {
				t.Errorf("%s: field problems=%v, want %q", tc.name, problems, tc.want)
			}
			continue
		}
		_, _, problems := draftTicketDependencies(context.Background(), store, tc.d)
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
			t.Errorf("%s: problems=%v, want one containing %q", tc.name, problems, tc.want)
		}
	}
}

func TestDraftTicketSimilarTitle(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"网关增加重试机制", "网关增加重试机制", true},
		{"网关增加重试机制", "增加网关的重试机制", true},
		{"网关增加重试机制", "修复部署屏障超时", false},
		{"整理 intake 草稿", "整理 intake 草稿的格式", true},
		{"网关增加重试机制", "增加网关的重试机制与熔断降级", true},
		{"", "非空标题", false},
	} {
		if got := similarTitle(tc.a, tc.b); got != tc.want {
			t.Errorf("similarTitle(%q, %q)=%v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDraftTicketSubmit(t *testing.T) {
	newRuntime := func() (*projectRuntime, *draftTicketStore) {
		store := &draftTicketStore{tasks: []issues.Task{
			managedTask("uuid-1", "OF-1", "网关重试退避", "Todo", true),
			managedTask("uuid-9", "OF-9", "已经完结的旧票", "Done", true),
		}}
		runtime := newProjectRuntime("/usr/bin/true", t.TempDir(), config.Tuning{})
		runtime.ledgers["OF"] = store
		return runtime, store
	}
	turn := intake.Turn{Session: "sess-417", Prompt: "给网关部署加重试，失败间隔翻倍", Project: intake.Project{Prefix: "OF"}}

	t.Run("creates at Todo with audit comment", func(t *testing.T) {
		runtime, store := newRuntime()
		answer := strings.Replace(draftTicketAnswer, `"related": []`, "\"related\": [],\n  \"labels\": [\"ready-for-agent\"]", 1)
		receipt, ok := runtime.submitDraftTicket(context.Background(), turn, answer)
		if !ok {
			t.Fatal("receipt missing for a draft answer")
		}
		if !strings.Contains(receipt, "已建票 OF-101") || !strings.Contains(receipt, "https://ledger.example/uuid-1") {
			t.Fatalf("receipt=%q, want ticket id and link", receipt)
		}
		if len(store.created) != 1 {
			t.Fatalf("created=%d, want 1", len(store.created))
		}
		created := store.created[0]
		if created.State != "Todo" || len(created.Labels) != 0 {
			t.Fatalf("state=%q labels=%v, want Todo with no pass-through labels", created.State, created.Labels)
		}
		if !strings.Contains(created.Description, "DEPENDENCIES:") || !strings.Contains(created.Description, "sess-417") {
			t.Fatalf("description=%q, want DEPENDENCIES line and session provenance", created.Description)
		}
		comments := store.comments["uuid-1"]
		if len(comments) != 1 || !strings.Contains(comments[0], "sess-417") || !strings.Contains(comments[0], turn.Prompt) {
			t.Fatalf("comments=%v, want one audit comment with session and request summary", comments)
		}
	})

	t.Run("dedup against open ticket", func(t *testing.T) {
		runtime, store := newRuntime()
		answer := strings.Replace(draftTicketAnswer, "网关增加部署重试", "网关的重试退避", 1)
		receipt, ok := runtime.submitDraftTicket(context.Background(), turn, answer)
		if !ok || !strings.Contains(receipt, "高度相似") || !strings.Contains(receipt, "OF-1") {
			t.Fatalf("receipt=%q, want a dedup explanation naming OF-1", receipt)
		}
		if len(store.created) != 0 {
			t.Fatalf("created=%v, want none on a dedup hit", store.created)
		}
	})

	t.Run("closed ticket does not block", func(t *testing.T) {
		runtime, store := newRuntime()
		answer := strings.Replace(draftTicketAnswer, "网关增加部署重试", "已经完结的旧票", 1)
		if _, ok := runtime.submitDraftTicket(context.Background(), turn, answer); !ok {
			t.Fatal("receipt missing")
		}
		if len(store.created) != 1 {
			t.Fatalf("created=%d, want 1; closed tickets must not dedup", len(store.created))
		}
	})

	t.Run("validation failure explains the missing field", func(t *testing.T) {
		runtime, store := newRuntime()
		answer := strings.Replace(draftTicketAnswer, `"check": "cd driver && go test -count=1 -run TestDeployRetry ./internal/deploy",`, "", 1)
		receipt, ok := runtime.submitDraftTicket(context.Background(), turn, answer)
		if !ok || !strings.Contains(receipt, "校验未通过") || !strings.Contains(receipt, "check") {
			t.Fatalf("receipt=%q, want the missing check field named", receipt)
		}
		if len(store.created) != 0 {
			t.Fatalf("created=%v, want none on validation failure", store.created)
		}
	})

	t.Run("answer without draft passes through", func(t *testing.T) {
		runtime, _ := newRuntime()
		if receipt, ok := runtime.submitDraftTicket(context.Background(), turn, "只是普通回复"); ok || receipt != "" {
			t.Fatalf("receipt=%q ok=%v, want no receipt", receipt, ok)
		}
	})
}
