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
	out, onboardable, err := discoverIntakeProjects(context.Background(), config.Config{}, &orca.Client{}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("out=%+v, want none", out)
	}
	// ReasonFactoryRootUnconfigured, not ReasonOnboardable: without a
	// factory_root, discovery cannot tell onboardable repos apart from any
	// other repo, so OF-49's set stays empty too.
	if len(onboardable) != 0 {
		t.Fatalf("onboardable=%+v, want none", onboardable)
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
