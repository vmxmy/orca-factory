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
	profile := filepath.Join(repo, "scripts", "agent-gates", "agent-graph.env")
	// Only selected, noncredential fields leave this subprocess.
	b, err = runx.Run(ctx, runx.Spec{Path: "/bin/bash", Args: []string{"-c", `source "$1" >/dev/null || exit; printf '%s\0' "$AGENT_GH_REPO" "$AGENT_ROLE_REVIEWER_CMD" "${AGENT_TIER_FAST:-}" "${AGENT_TIER_DEFAULT:-}" "${AGENT_TIER_STRONG:-}" "${AGENT_TIER_ESCALATE:-0}" "${AGENT_ROLE_REVIEWER_CMD_SMALL:-}" "${AGENT_REVIEWER_SMALL_MAX_ESTIMATE:-2}" "${AGENT_LANE3_GLOBS:-}" "${AGENT_ISSUE_BACKEND:-}" "${AGENT_GH_API_URL:-}" "${AGENT_FEISHU_BASE:-}" "${AGENT_FEISHU_PROJECT:-}" "${AGENT_INSTALL_CMD:-true}"`, "driver-profile", profile}, Dir: repo, Timeout: 10 * time.Second})
	if err != nil {
		return Result{}, errors.New("repository profile could not be loaded")
	}
	values := strings.Split(string(b), "\x00")
	if len(values) != 15 {
		return Result{}, errors.New("invalid repository profile")
	}
	owner, name, ok := strings.Cut(values[0], "/")
	if !ok || !component.MatchString(owner) || !component.MatchString(name) || strings.TrimSpace(values[1]) == "" {
		return Result{}, errors.New("repository profile requires GitHub repository and reviewer")
	}
	maximum, err := strconv.ParseFloat(values[7], 64)
	if err != nil {
		return Result{}, errors.New("invalid reviewer estimate threshold")
	}
	cfg, err := config.Load(home())
	if err != nil {
		return Result{}, errors.New("driver configuration could not be loaded")
	}
	proxy, err := config.ModelProxyURL(home())
	if err != nil {
		return Result{}, err
	}
	store, err := issues.New(ctx, map[string]string{"AGENT_ISSUE_BACKEND": values[9], "AGENT_GH_REPO": values[0], "AGENT_GH_API_URL": values[10], "AGENT_FEISHU_BASE": values[11], "AGENT_FEISHU_PROJECT": values[12]}, cfg.Tuning)
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
	e := Executor{Orca: oc, Issues: store, GitHub: gc, Tools: &Commands{ReviewerCommand: values[1], ReviewerSmallCommand: values[6], ReviewerSmallMaxEstimate: &maximum, InstallCommand: values[13], ModelProxyURL: proxy}, Config: Config{Repo: repo, Owner: owner, Repository: name, Terminal: terminal, Unit: "executor"}}
	e.Config.Root, err = executorStateRoot()
	if err != nil {
		return Result{}, err
	}
	e.HeartbeatPath = orca.HeartbeatPath(e.Config.Root, name, ticket.Identifier, "executor")
	e.Config.WorkerIdleTimeout = workerIdleTimeout(os.Getenv("AGENT_WORKER_IDLE_MINUTES"))
	e.Config.Tier = "default"
	e.Config.Escalate = values[5]
	e.Config.TierSettings = map[string]string{"fast": values[2], "default": values[3], "strong": values[4]}
	parts := strings.Split(values[3], ":")
	if len(parts) != 3 || parts[0] == "" {
		return Result{}, errors.New("repository profile requires default worker tier")
	}
	e.Config.Agent, e.Config.Model, e.Config.Effort = parts[0], parts[1], parts[2]
	if e.Config.Model == "" {
		e.Config.Effort = ""
	}
	delegated = true
	e.Config.Merging = merging
	e.Config.Lane3Globs = strings.Fields(values[8])
	return e.Start(ctx, ticket)
}

func workerIdleTimeout(value string) time.Duration {
	minutes, err := strconv.ParseInt(value, 10, 64)
	if err != nil || minutes <= 0 || minutes > int64((1<<63-1)/time.Minute) {
		return 15 * time.Minute
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
