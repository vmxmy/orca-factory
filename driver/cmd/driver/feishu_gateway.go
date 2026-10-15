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
	"sync"
	"syscall"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
)

var gatewayKeys = []string{"AGENT_FEISHU_APP_ID", "AGENT_FEISHU_APP_SECRET", "AGENT_FEISHU_INTAKE_CHAT", "AGENT_FEISHU_INTAKE_USERS", "AGENT_FEISHU_BOT_OPEN_ID", "AGENT_FEISHU_ENDPOINT", "AGENT_INTAKE_CLAUDE", "AGENT_FEISHU_INTAKE_PROJECT", "AGENT_FEISHU_APPROVERS"}

var optionalGatewayKeys = []string{"AGENT_FEISHU_ENDPOINT", "AGENT_INTAKE_CLAUDE", "AGENT_FEISHU_INTAKE_PROJECT", "AGENT_FEISHU_APPROVERS"}

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

// intakeLedger opens the ledger Store scoped to this project's own
// repository env (its AGENT_FEISHU_PROJECT remote): the gateway submits
// confirmed drafts through it (OF-15) and reads it for the ticket context.
func intakeLedger(ctx context.Context, env map[string]string, tuning config.Tuning) (issues.Store, error) {
	client, err := issues.New(ctx, env, tuning)
	if err != nil {
		return nil, err
	}
	store, ok := client.(issues.Store)
	if !ok {
		return nil, errors.New("intake gateway requires a feishu repository")
	}
	return store, nil
}

// intakeTickets builds the ledger reader a Runner writes into
// .intake/tickets.md before every Agent turn (OF-33).
func intakeTickets(store issues.Store) func(context.Context) ([]intake.TicketContext, error) {
	return func(ctx context.Context) ([]intake.TicketContext, error) {
		tasks, err := store.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]intake.TicketContext, 0, len(tasks))
		for _, task := range tasks {
			blockers := make([]string, 0, len(task.BlockedBy))
			for _, b := range task.BlockedBy {
				blockers = append(blockers, b.Issue.Identifier)
			}
			out = append(out, intake.TicketContext{Identifier: task.Identifier, State: task.Metadata.State, Title: task.Title, Blockers: blockers})
		}
		return out, nil
	}
}

// projectRuntime lazily builds and caches each eligible project's heavy
// wiring (Feishu ledger Store validation, model/effort tier, Runner) by
// prefix, across repeated discovery runs (OF-42): a new Feishu topic
// rediscovers the live eligible set, but only the first topic that actually
// dispatches to a given prefix pays for building its Store and Runner.
type projectRuntime struct {
	mu       sync.Mutex
	claude   string
	cacheDir string
	tuning   config.Tuning
	built    map[string]*intake.Runner
	projects map[string]intake.Project
}

func newProjectRuntime(claude, cacheDir string, tuning config.Tuning) *projectRuntime {
	return &projectRuntime{claude: claude, cacheDir: cacheDir, tuning: tuning, built: map[string]*intake.Runner{}, projects: map[string]intake.Project{}}
}

// resolve returns the fully wired intake.Project for an eligible discovery
// result, building and caching its Ledger Store and Runner the first time
// this prefix is seen; later discovery rounds reuse the cached wiring.
func (r *projectRuntime) resolve(ctx context.Context, p projects.Project) (intake.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cached, ok := r.projects[p.Prefix]; ok {
		return cached, nil
	}
	model, effort, err := tierParts(p.Repository.Env)
	if err != nil {
		return intake.Project{}, err
	}
	store, err := intakeLedger(ctx, p.Repository.Env, r.tuning)
	if err != nil {
		return intake.Project{}, err
	}
	checkout := filepath.Join(r.cacheDir, "orca-factory", "intake", p.Prefix, "checkout")
	project := intake.Project{Prefix: p.Prefix, Name: p.Name, Remote: p.Remote, Enabled: true, Repo: p.Path, Checkout: checkout, Ledger: store}
	// core/gate.sh always treats scripts/agent-gates/* as lane 2.
	project.Lane2Globs, project.Lane3Globs = "scripts/agent-gates/* "+p.Repository.Env["AGENT_LANE2_GLOBS"], p.Repository.Env["AGENT_LANE3_GLOBS"]
	r.built[p.Prefix] = &intake.Runner{Claude: r.claude, Repo: p.Path, Checkout: checkout, Model: model, Effort: effort, Timeout: 5 * time.Minute, Tickets: intakeTickets(store)}
	r.projects[p.Prefix] = project
	return project, nil
}

// agent is the Turn dispatcher passed to intake.New: every prefix a Turn can
// name was already resolved (discoverIntakeProjects only ever hands the
// gateway prefixes it resolved), so a miss here means no local repository
// ever matched that project.
func (r *projectRuntime) agent(ctx context.Context, t intake.Turn) (string, error) {
	r.mu.Lock()
	runner, ok := r.built[t.Project.Prefix]
	r.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("intake gateway: no local repository for project %s", t.Project.Prefix)
	}
	return runner.Turn(ctx, t)
}

// discoverIntakeProjects re-runs project discovery (OF-42: live Orca
// repository inventory, the Feishu ledger's project table, each repository's
// own pipeline check) and resolves every eligible repository's wiring via
// runtime. Every other ledger row is still returned, carrying its rejection
// reason, so the gateway can answer a thread that names it specifically.
func discoverIntakeProjects(ctx context.Context, cfg config.Config, client *orca.Client, runtime *projectRuntime) ([]intake.Project, error) {
	orcaRepos, err := client.RepoList(ctx)
	if err != nil {
		return nil, err
	}
	var rows []issues.LedgerProject
	var checker projects.Checker
	if cfg.FactoryRoot != "" {
		checker = factoryChecker(cfg.FactoryRoot)
		if rows, err = ledgerProjectRows(ctx, orcaRepos, cfg.Tuning); err != nil {
			return nil, err
		}
	}
	result := projects.Discover(ctx, orcaRepos, rows, checker)
	reasons := make(map[string]projects.Reason, len(result.Rejected))
	for _, rejected := range result.Rejected {
		reasons[rejected.Remote] = rejected.Reason
	}
	eligible := make(map[string]projects.Project, len(result.Eligible))
	for _, p := range result.Eligible {
		eligible[p.Remote] = p
	}
	out := make([]intake.Project, 0, len(rows))
	for _, row := range rows {
		if p, ok := eligible[row.Remote]; ok {
			project, err := runtime.resolve(ctx, p)
			if err != nil {
				return nil, err
			}
			out = append(out, project)
			continue
		}
		out = append(out, intake.Project{Prefix: row.Prefix, Name: row.Name, Remote: row.Remote, Enabled: row.Enabled, Reason: string(reasons[row.Remote])})
	}
	return out, nil
}

// refreshIntakeProjects rediscovers and installs the gateway's current
// routable project set (OF-42). A discovery failure is logged and leaves the
// gateway's existing projects untouched: a transient Orca or ledger error
// must not drop an already-working project out of the picker.
func refreshIntakeProjects(ctx context.Context, gateway *intake.Gateway, cfg config.Config, client *orca.Client, runtime *projectRuntime) {
	fresh, err := discoverIntakeProjects(ctx, cfg, client, runtime)
	if err != nil {
		fmt.Fprintln(os.Stderr, "intake gateway: project discovery failed:", err)
		return
	}
	if err := gateway.SetProjects(fresh); err != nil {
		fmt.Fprintln(os.Stderr, "intake gateway: invalid discovered projects:", err)
	}
}

// isNewIntakeTopic reports whether event opens a new thread rather than
// replying inside one Feishu already assigned a thread_id: discovery only
// needs to run again when a topic is starting (OF-42).
func isNewIntakeTopic(event *im.P2MessageReceiveV1) bool {
	return event != nil && event.Event != nil && event.Event.Message != nil && value(event.Event.Message.ThreadId) == ""
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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
	claude := settings["AGENT_INTAKE_CLAUDE"]
	if claude == "" {
		claude, _ = exec.LookPath("claude")
	}
	if !filepath.IsAbs(claude) {
		return errors.New("AGENT_INTAKE_CLAUDE must be an absolute path to claude")
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	concurrency, err := intakeConcurrency(map[string]string{"AGENT_INTAKE_CONCURRENCY": os.Getenv("AGENT_INTAKE_CONCURRENCY")})
	if err != nil {
		return err
	}
	runtime := newProjectRuntime(claude, cacheDir, cfg.Tuning)
	orcaClient := &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout}
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
	// Projects are discovered on demand (OF-42): the gateway starts with none
	// and rediscovers the live Orca/ledger state when each new topic begins.
	gateway, err := intake.New(dir, settings["AGENT_FEISHU_INTAKE_CHAT"], settings["AGENT_FEISHU_BOT_OPEN_ID"], gatewayUsers(settings["AGENT_FEISHU_INTAKE_USERS"]), gatewayUsers(settings["AGENT_FEISHU_APPROVERS"]), concurrency, nil, settings["AGENT_FEISHU_INTAKE_PROJECT"], runtime.agent, client.ReplyInThread, client.AddReaction)
	if err != nil {
		return err
	}
	handler := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(func(ctx context.Context, event *im.P2MessageReceiveV1) error {
		if isNewIntakeTopic(event) {
			refreshIntakeProjects(ctx, gateway, cfg, orcaClient, runtime)
		}
		return gateway.Handle(ctx, event)
	})
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
