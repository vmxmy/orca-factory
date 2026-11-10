package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// RunTicket binds a Run to the calling Orca terminal. The repository profile is
// trusted local configuration; ticket bodies are fetched separately as data.
func RunTicket(ctx context.Context, identifier string, merging bool) (result Result, err error) {
	session, err := maintenance.Session()
	if err != nil {
		return Result{}, err
	}
	defer session()
	logger := (&Executor{}).withLog(ctx)

	delegated := false
	defer func() {
		if !delegated {
			logger.logError("initialization", err)
		}
	}()
	if !component.MatchString(identifier) {
		return Result{}, errors.New("invalid ticket identifier")
	}
	terminal := os.Getenv("ORCA_TERMINAL_HANDLE")
	if terminal == "" {
		return Result{}, errors.New("driver exec requires a current Orca terminal")
	}
	b, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"rev-parse", "--show-toplevel"}, Timeout: 10 * time.Second})
	if err != nil {
		return Result{}, errors.New("cannot locate repository")
	}
	repo := strings.TrimSpace(string(b))
	e, repository, err := repositoryExecutor(repo)
	if err != nil {
		return Result{}, err
	}
	e.Config.Terminal = terminal
	cfg, err := config.Load(home())
	if err != nil {
		return Result{}, errors.New("driver configuration could not be loaded")
	}
	store, err := issues.New(ctx, repository.Env, cfg.Tuning)
	if err != nil {
		return Result{}, err
	}
	task, err := store.Read(ctx, identifier)
	if err != nil {
		return Result{}, err
	}
	if !task.Metadata.Complete {
		return Result{}, errors.New("explicit adoption required before recovery")
	}
	ticket, err := ParseTicket(task.ID, task.Identifier, task.Title, task.Description)
	ticket.Estimate = task.Estimate
	ticket.Number = task.Number
	if err != nil {
		return Result{}, err
	}
	gc, err := github.New(github.Options{HTTPTimeout: cfg.Tuning.GitHub.HTTPTimeout})
	if err != nil {
		return Result{}, errors.New("GitHub initialization failed")
	}
	oc := &orca.Client{WorkerStartWait: cfg.Tuning.Orca.WorkerStartWait, Timeout: cfg.Tuning.Orca.CallTimeout}
	e.Orca, e.Issues, e.GitHub = oc, store, gc
	e.Config.Root, err = executorStateRoot()
	if err != nil {
		return Result{}, err
	}
	e.HeartbeatPath = orca.HeartbeatPath(e.Config.Root, e.Config.Repository, ticket.Identifier, "executor")
	e.Config.WorkerIdleTimeout = envMinutes(os.Getenv("AGENT_WORKER_IDLE_MINUTES"), 15*time.Minute)
	e.Config.WorkerMaxDuration = envMinutes(os.Getenv("AGENT_WORKER_MAX_MINUTES"), 120*time.Minute)
	delegated = true
	e.Config.Merging = merging
	return e.Start(ctx, ticket)
}

func repositoryExecutor(path string) (Executor, config.Repository, error) {
	repository, err := config.LoadRepository(path)
	if err != nil {
		return Executor{}, config.Repository{}, err
	}
	env := repository.Env
	owner, name, ok := strings.Cut(env["AGENT_GH_REPO"], "/")
	if !ok || !component.MatchString(owner) || !component.MatchString(name) || strings.TrimSpace(env["AGENT_ROLE_REVIEWER_CMD"]) == "" {
		return Executor{}, config.Repository{}, errors.New("repository profile requires GitHub repository and reviewer")
	}
	threshold := env["AGENT_REVIEWER_SMALL_MAX_ESTIMATE"]
	if threshold == "" {
		threshold = "2"
	}
	maximum, err := strconv.ParseFloat(threshold, 64)
	if err != nil {
		return Executor{}, config.Repository{}, errors.New("invalid reviewer estimate threshold")
	}
	parts := strings.Split(env["AGENT_TIER_DEFAULT"], ":")
	if len(parts) != 3 || parts[0] == "" {
		return Executor{}, config.Repository{}, errors.New("repository profile requires default worker tier")
	}
	cfg := Config{Repo: path, Owner: owner, Repository: name, Unit: "executor", Tier: "default",
		Agent: parts[0], Model: parts[1], Effort: parts[2],
		TierSettings: map[string]string{"default": env["AGENT_TIER_DEFAULT"], "strong": env["AGENT_TIER_STRONG"]},
		Escalate:     env["AGENT_TIER_ESCALATE"], Lane3Globs: strings.Fields(env["AGENT_LANE3_GLOBS"])}
	if cfg.Model == "" {
		cfg.Effort = ""
	}
	if cfg.Escalate == "" {
		cfg.Escalate = "0"
	}
	install := env["AGENT_INSTALL_CMD"]
	if install == "" {
		install = "true"
	}
	proxy, err := config.ModelProxyURL(home())
	if err != nil {
		return Executor{}, config.Repository{}, err
	}
	commands := &Commands{ModelProxyURL: proxy, ReviewerCommand: env["AGENT_ROLE_REVIEWER_CMD"], ReviewerSmallCommand: env["AGENT_ROLE_REVIEWER_CMD_SMALL"], ReviewerSmallMaxEstimate: &maximum, InstallCommand: install}
	return Executor{Config: cfg, Tools: commands}, repository, nil
}

func envMinutes(value string, fallback time.Duration) time.Duration {
	minutes, err := strconv.ParseInt(value, 10, 64)
	if err != nil || minutes <= 0 || minutes > int64((1<<63-1)/time.Minute) {
		return fallback
	}
	return time.Duration(minutes) * time.Minute
}

// executorStateRoot is shared by all entrypoint recovery and artifact writes.
func home() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func executorStateRoot() (string, error) {
	if root := os.Getenv("AGENT_DRIVER_STATE_ROOT"); root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot resolve state root")
	}
	return filepath.Join(home, ".local", "state", "orca-factory"), nil
}
