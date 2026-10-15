package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
)

var gatewayKeys = []string{"AGENT_FEISHU_APP_ID", "AGENT_FEISHU_APP_SECRET", "AGENT_FEISHU_INTAKE_CHAT", "AGENT_FEISHU_INTAKE_USERS", "AGENT_FEISHU_BOT_OPEN_ID", "AGENT_FEISHU_ENDPOINT", "AGENT_INTAKE_CLAUDE", "AGENT_FEISHU_INTAKE_PROJECT"}

var optionalGatewayKeys = []string{"AGENT_FEISHU_ENDPOINT", "AGENT_INTAKE_CLAUDE", "AGENT_FEISHU_INTAKE_PROJECT"}

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
		if !slices.Contains(optionalGatewayKeys, key) && settings[key] == "" {
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

// tierParts splits a repository's AGENT_ROLE_INTAKE (or AGENT_TIER_STRONG
// fallback) "claude:model:effort" tier into its model and effort.
func tierParts(env map[string]string) (model, effort string, err error) {
	tier := env["AGENT_ROLE_INTAKE"]
	if tier == "" {
		tier = env["AGENT_TIER_STRONG"]
	}
	parts := strings.Split(tier, ":")
	if parts[0] != "claude" || len(parts) > 3 {
		return "", "", errors.New("AGENT_ROLE_INTAKE must be a claude tier")
	}
	parts = append(parts, "", "")
	return parts[1], parts[2], nil
}

func intakeConcurrency(env map[string]string) (int, error) {
	raw := env["AGENT_INTAKE_CONCURRENCY"]
	if raw == "" {
		return 2, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, errors.New("AGENT_INTAKE_CONCURRENCY must be a positive integer")
	}
	return n, nil
}

// legacyIntakeRunner serves a feishu repository that has no AGENT_FEISHU_BASE
// and so no ledger to resolve projects against: it is its own sole implicit
// project, exactly as before OF-32, including the pre-OF-32 checkout path.
func legacyIntakeRunner(claude string, repo config.Repository, cache string) (*intake.Runner, int, error) {
	model, effort, err := tierParts(repo.Env)
	if err != nil {
		return nil, 0, err
	}
	concurrency, err := intakeConcurrency(repo.Env)
	if err != nil {
		return nil, 0, err
	}
	return &intake.Runner{Claude: claude, Repo: repo.Path, Checkout: filepath.Join(cache, "orca-factory", "intake-checkout"), Model: model, Effort: effort, Timeout: 5 * time.Minute}, concurrency, nil
}

// intakeRunners matches each ledger project (rows, possibly empty) to the
// local repository that serves it, by driver.toml's AGENT_FEISHU_PROJECT
// remote, and builds one Runner per matched project, each in its own
// checkout so two projects never share origin/main state (OF-32). An empty
// rows falls back to legacyIntakeRunner: a single implicit project.
func intakeRunners(claude string, repos []config.Repository, rows []issues.LedgerProject) ([]intake.Project, func(context.Context, intake.Turn) (string, error), int, error) {
	if claude == "" {
		claude, _ = exec.LookPath("claude")
	}
	if !filepath.IsAbs(claude) {
		return nil, nil, 0, errors.New("AGENT_INTAKE_CLAUDE must be an absolute path to claude")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, nil, 0, err
	}
	if len(rows) == 0 {
		runner, concurrency, err := legacyIntakeRunner(claude, repos[0], cache)
		if err != nil {
			return nil, nil, 0, err
		}
		return nil, runner.Turn, concurrency, nil
	}
	byRemote := map[string]config.Repository{}
	for _, repo := range repos {
		remote := repo.Env["AGENT_FEISHU_PROJECT"]
		if remote == "" {
			continue
		}
		if _, dup := byRemote[remote]; dup {
			return nil, nil, 0, errors.New("duplicate AGENT_FEISHU_PROJECT across repositories")
		}
		byRemote[remote] = repo
	}
	runners := map[string]*intake.Runner{}
	projects := make([]intake.Project, 0, len(rows))
	concurrency := 0
	for _, row := range rows {
		p := intake.Project{Prefix: row.Prefix, Name: row.Name, Remote: row.Remote, Enabled: row.Enabled}
		if repo, matched := byRemote[row.Remote]; matched {
			model, effort, err := tierParts(repo.Env)
			if err != nil {
				return nil, nil, 0, err
			}
			projectConcurrency, err := intakeConcurrency(repo.Env)
			if err != nil {
				return nil, nil, 0, err
			}
			concurrency += projectConcurrency
			p.Repo = repo.Path
			runners[row.Prefix] = &intake.Runner{Claude: claude, Repo: repo.Path, Checkout: filepath.Join(cache, "orca-factory", "intake", row.Prefix, "checkout"), Model: model, Effort: effort, Timeout: 5 * time.Minute}
		}
		projects = append(projects, p)
	}
	if concurrency == 0 {
		concurrency = 2
	}
	agent := func(ctx context.Context, t intake.Turn) (string, error) {
		r, ok := runners[t.Project.Prefix]
		if !ok {
			return "", fmt.Errorf("intake gateway: no local repository for project %s", t.Project.Prefix)
		}
		return r.Turn(ctx, t)
	}
	return projects, agent, concurrency, nil
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
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	var repos []config.Repository
	for _, repo := range cfg.Repositories {
		if repo.Env["AGENT_ISSUE_BACKEND"] == "feishu" {
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		return errors.New("intake gateway requires at least one feishu repository in driver.toml")
	}
	// A repository with no AGENT_FEISHU_BASE has no ledger to resolve
	// projects against; intakeRunners then falls back to the pre-OF-32
	// single-project behavior.
	var rows []issues.LedgerProject
	if repos[0].Env["AGENT_FEISHU_BASE"] != "" {
		store, err := issues.New(ctx, repos[0].Env, cfg.Tuning)
		if err != nil {
			return err
		}
		ledger, ok := store.(*issues.Feishu)
		if !ok {
			return errors.New("intake gateway requires a feishu repository")
		}
		if rows, err = ledger.LedgerProjects(ctx); err != nil {
			return err
		}
	}
	projects, agent, concurrency, err := intakeRunners(settings["AGENT_INTAKE_CLAUDE"], repos, rows)
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
	gateway, err := intake.New(dir, settings["AGENT_FEISHU_INTAKE_CHAT"], settings["AGENT_FEISHU_BOT_OPEN_ID"], gatewayUsers(settings["AGENT_FEISHU_INTAKE_USERS"]), concurrency, projects, settings["AGENT_FEISHU_INTAKE_PROJECT"], agent, client.ReplyInThread)
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
