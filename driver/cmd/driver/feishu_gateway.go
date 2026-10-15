package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
)

var gatewayKeys = []string{"AGENT_FEISHU_APP_ID", "AGENT_FEISHU_APP_SECRET", "AGENT_FEISHU_INTAKE_CHAT", "AGENT_FEISHU_INTAKE_USERS", "AGENT_FEISHU_BOT_OPEN_ID", "AGENT_FEISHU_ENDPOINT"}

func loadGatewaySettings() (map[string]string, error) {
	settings := map[string]string{}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(home, ".config/orca-factory/driver.env"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("driver.env could not be read")
	}
	if err == nil {
		settings, err = config.ParseEnv(f)
		err = errors.Join(err, f.Close())
		if err != nil {
			return nil, errors.New("driver.env is invalid")
		}
	}
	var missing []string
	for _, key := range gatewayKeys {
		if v := os.Getenv(key); v != "" {
			settings[key] = v
		}
		settings[key] = strings.TrimSpace(settings[key])
		if key != "AGENT_FEISHU_ENDPOINT" && settings[key] == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing configuration: %s", strings.Join(missing, ", "))
	}
	if len(gatewayUsers(settings["AGENT_FEISHU_INTAKE_USERS"])) == 0 {
		return nil, errors.New("AGENT_FEISHU_INTAKE_USERS must contain open_ids")
	}
	return settings, nil
}

func gatewayUsers(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

func init() { Register("feishu-gateway", feishuGateway) }

func feishuGateway(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: orca-factory-driver feishu-gateway")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := runFeishuGateway(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runFeishuGateway(ctx context.Context) error {
	settings, err := loadGatewaySettings()
	if err != nil {
		return err
	}
	dir := filepath.Join(logStateRoot(), "intake")
	lock, err := flock.TryLock(filepath.Join(dir, "gateway.lock"))
	if err != nil {
		return errors.New("cannot acquire intake gateway lock")
	}
	defer func() { _ = lock.Close() }()
	client, err := feishu.New(feishu.Options{AppID: settings["AGENT_FEISHU_APP_ID"], AppSecret: settings["AGENT_FEISHU_APP_SECRET"], Endpoint: settings["AGENT_FEISHU_ENDPOINT"]})
	if err != nil {
		return err
	}
	gateway, err := intake.New(dir, settings["AGENT_FEISHU_INTAKE_CHAT"], settings["AGENT_FEISHU_BOT_OPEN_ID"], gatewayUsers(settings["AGENT_FEISHU_INTAKE_USERS"]), client.ReplyInThread)
	if err != nil {
		return err
	}
	handler := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(gateway.Handle)
	handler.Config.Logger = gatewaySDKLogger{}
	options := []ws.ClientOption{ws.WithEventHandler(handler), ws.WithAutoReconnect(true), ws.WithLogger(gatewaySDKLogger{})}
	if endpoint := settings["AGENT_FEISHU_ENDPOINT"]; endpoint != "" {
		options = append(options, ws.WithDomain(endpoint))
	}
	connection := ws.NewClient(settings["AGENT_FEISHU_APP_ID"], settings["AGENT_FEISHU_APP_SECRET"], options...)
	err = connection.Start(ctx)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return errors.New("feishu gateway connection failed")
	}
	return nil
}

// SDK diagnostics can contain payloads and credential-bearing connection URLs.
// Keep only the severity at this boundary; never format SDK arguments.
type gatewaySDKLogger struct{}

func (gatewaySDKLogger) Debug(context.Context, ...interface{}) {}
func (gatewaySDKLogger) Info(context.Context, ...interface{})  {}
func (gatewaySDKLogger) Warn(context.Context, ...interface{}) {
	fmt.Fprintln(os.Stderr, "feishu gateway SDK warning")
}
func (gatewaySDKLogger) Error(context.Context, ...interface{}) {
	fmt.Fprintln(os.Stderr, "feishu gateway SDK error")
}
