package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	e, storeEnv, err := repositoryExecutor(ctx, repo)
	if err != nil {
		return Result{}, err
	}
	e.Config.Terminal = terminal
	cfg, err := config.Load(home())
	if err != nil {
		return Result{}, errors.New("driver configuration could not be loaded")
	}
	store, err := issues.New(ctx, storeEnv, cfg.Tuning)
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

// repositoryExecutor sources agent-graph.env with bash, as exec always has, and
// applies the machine profile through the same function LoadRepository uses.
// It sources once with the inherited environment, so values are those exec used
// before profiles. Keys count as explicit only when the file assigns them, as in
// LoadRepository; only keys the profile then fills replace the sourced values.
func repositoryExecutor(ctx context.Context, repo string) (Executor, map[string]string, error) {
	profile := filepath.Join(repo, "scripts", "agent-gates", "agent-graph.env")
	data, err := os.ReadFile(profile)
	if err != nil {
		return Executor{}, nil, errors.New("repository profile could not be loaded")
	}
	// Only selected, noncredential fields leave this subprocess.
	b, err := runx.Run(ctx, runx.Spec{Path: "/bin/bash", Args: []string{"-c", `source "$1" >/dev/null || exit; printf '%s\0' "$AGENT_GH_REPO" "$AGENT_ROLE_REVIEWER_CMD" "$AGENT_ROLE_REVIEWER" "$AGENT_ROLE_REVIEWER_CMD_SMALL" "$AGENT_ROLE_REVIEWER_SMALL" "$AGENT_ROLE_SECOND_REVIEWER_CMD" "$AGENT_ROLE_SECOND_REVIEWER" "${AGENT_TIER_DEFAULT:-}" "${AGENT_TIER_STRONG:-}" "${AGENT_TIER_ESCALATE:-0}" "${AGENT_REVIEWER_SMALL_MAX_ESTIMATE:-2}" "${AGENT_LANE3_GLOBS:-}" "${AGENT_ISSUE_BACKEND:-}" "${AGENT_GH_API_URL:-}" "${AGENT_FEISHU_BASE:-}" "${AGENT_FEISHU_PROJECT:-}" "${AGENT_INSTALL_CMD:-true}" "${AGENT_PROFILE-}" "${AGENT_WORKER_SKILLS:-}"; for k in $(compgen -v AGENT_TIER_); do printf '%s\0' "$k" "${!k}"; done`, "driver-profile", profile}, Dir: repo, Timeout: 10 * time.Second})
	if err != nil {
		return Executor{}, nil, errors.New("repository profile could not be loaded")
	}
	values := strings.Split(string(b), "\x00")
	if len(values) < 20 || len(values)%2 != 0 {
		return Executor{}, nil, errors.New("invalid repository profile")
	}
	owner, name, ok := strings.Cut(values[0], "/")
	if !ok || !component.MatchString(owner) || !component.MatchString(name) {
		return Executor{}, nil, errors.New("repository profile requires GitHub repository and reviewer")
	}
	maximum, err := strconv.ParseFloat(values[10], 64)
	if err != nil {
		return Executor{}, nil, errors.New("invalid reviewer estimate threshold")
	}
	assigned := config.AssignedKeys(data)
	sourced := map[string]string{
		"AGENT_GH_REPO": values[0], "AGENT_ROLE_REVIEWER_CMD": values[1], "AGENT_ROLE_REVIEWER": values[2],
		"AGENT_ROLE_REVIEWER_CMD_SMALL": values[3], "AGENT_ROLE_REVIEWER_SMALL": values[4],
		"AGENT_ROLE_SECOND_REVIEWER_CMD": values[5], "AGENT_ROLE_SECOND_REVIEWER": values[6],
		"AGENT_TIER_DEFAULT": values[7], "AGENT_TIER_STRONG": values[8], "AGENT_TIER_ESCALATE": values[9],
		"AGENT_REVIEWER_SMALL_MAX_ESTIMATE": values[10], "AGENT_LANE3_GLOBS": values[11], "AGENT_ISSUE_BACKEND": values[12],
		"AGENT_GH_API_URL": values[13], "AGENT_FEISHU_BASE": values[14], "AGENT_FEISHU_PROJECT": values[15],
		"AGENT_INSTALL_CMD": values[16], "AGENT_PROFILE": values[17], "AGENT_WORKER_SKILLS": values[18],
	}
	for i := 19; i+1 < len(values); i += 2 {
		if _, fixed := sourced[values[i]]; !fixed {
			sourced[values[i]] = values[i+1]
		}
	}
	explicit := map[string]string{}
	for key, value := range sourced {
		if assigned[key] {
			explicit[key] = value
		}
	}
	_, sources, err := config.ApplyProfile(explicit)
	if err != nil {
		return Executor{}, nil, err
	}
	commands, err := config.ResolveReviewerCommands(explicit, sources)
	if err != nil {
		return Executor{}, nil, err
	}
	effective := func(key string) string {
		if strings.HasPrefix(sources[key], "profile:") {
			return explicit[key]
		}
		return sourced[key]
	}
	parts := strings.Split(effective("AGENT_TIER_DEFAULT"), ":")
	if len(parts) != 3 || parts[0] == "" {
		return Executor{}, nil, errors.New("repository profile requires default worker tier")
	}
	chain, err := config.TierChain(effective("AGENT_TIER_CHAIN"))
	if err != nil {
		return Executor{}, nil, err
	}
	settings := map[string]string{"default": effective("AGENT_TIER_DEFAULT"), "strong": effective("AGENT_TIER_STRONG")}
	escalate := effective("AGENT_TIER_ESCALATE")
	for _, tier := range chain {
		key := "AGENT_TIER_" + strings.ToUpper(tier)
		settings[tier] = effective(key)
		if escalate != "1" {
			continue
		}
		if tierParts := strings.Split(settings[tier], ":"); len(tierParts) != 3 || tierParts[0] == "" {
			return Executor{}, nil, fmt.Errorf("AGENT_TIER_CHAIN tier %s requires %s as agent:model:effort", tier, key)
		}
	}
	if escalate == "1" && !slices.Contains(chain, "default") {
		return Executor{}, nil, errors.New("AGENT_TIER_CHAIN requires the default tier")
	}
	var workerSkills []string
	if value := effective("AGENT_WORKER_SKILLS"); value != "" {
		workerSkills = strings.Split(value, ",")
		for _, skill := range workerSkills {
			if !workerSkillName.MatchString(skill) {
				return Executor{}, nil, fmt.Errorf("AGENT_WORKER_SKILLS invalid value %q: skill names must match [A-Za-z0-9_:-]+", value)
			}
		}
	}
	cfg := Config{Repo: repo, Owner: owner, Repository: name, Unit: "executor", Tier: "default",
		Agent: parts[0], Model: parts[1], Effort: parts[2], Escalate: escalate,
		TierSettings: settings, TierChain: chain, WorkerSkills: workerSkills, Lane3Globs: strings.Fields(values[11])}
	if cfg.Model == "" {
		cfg.Effort = ""
	}
	proxy, err := config.ModelProxyURL(home())
	if err != nil {
		return Executor{}, nil, err
	}
	if commands.Main == "" {
		return Executor{}, nil, errors.New("repository profile requires GitHub repository and reviewer")
	}
	tools := &Commands{ReviewerCommand: commands.Main, ReviewerSmallCommand: commands.Small, ReviewerSmallMaxEstimate: &maximum, InstallCommand: values[16], ModelProxyURL: proxy}
	store := map[string]string{"AGENT_ISSUE_BACKEND": values[12], "AGENT_GH_REPO": values[0], "AGENT_GH_API_URL": values[13], "AGENT_FEISHU_BASE": values[14], "AGENT_FEISHU_PROJECT": values[15]}
	return Executor{Config: cfg, Tools: tools}, store, nil
}

var workerSkillName = regexp.MustCompile(`^[A-Za-z0-9_:-]+$`)

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
