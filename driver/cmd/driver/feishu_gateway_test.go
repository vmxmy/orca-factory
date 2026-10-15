package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
)

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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_DRIVER_STATE_ROOT", t.TempDir())
	for _, key := range gatewayKeys[:5] {
		t.Setenv(key, "test")
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
