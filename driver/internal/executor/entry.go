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
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// RunTicket binds a Run to the calling Orca terminal. The repository profile is
// trusted local configuration; ticket bodies are fetched separately as data.
func RunTicket(ctx context.Context, identifier string, merging bool) (result Result, err error) {
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
	b, err = runx.Run(ctx, runx.Spec{Path: "/bin/bash", Args: []string{"-c", `source "$1" >/dev/null || exit; printf '%s\0' "$AGENT_GH_REPO" "$AGENT_LINEAR_TEAM" "$AGENT_LINEAR_PROJECT" "$AGENT_ROLE_REVIEWER_CMD" "${AGENT_TIER_FAST:-}" "${AGENT_TIER_DEFAULT:-}" "${AGENT_TIER_STRONG:-}" "${AGENT_TIER_ESCALATE:-0}" "${AGENT_ROLE_REVIEWER_CMD_SMALL:-}" "${AGENT_REVIEWER_SMALL_MAX_ESTIMATE:-2}" "${AGENT_LANE3_GLOBS:-}"`, "driver-profile", profile}, Dir: repo, Timeout: 10 * time.Second})
	if err != nil {
		return Result{}, errors.New("repository profile could not be loaded")
	}
	values := strings.Split(string(b), "\x00")
	if len(values) != 12 {
		return Result{}, errors.New("invalid repository profile")
	}
	owner, name, ok := strings.Cut(values[0], "/")
	if !ok || !component.MatchString(owner) || !component.MatchString(name) || values[1] == "" || values[2] == "" || strings.TrimSpace(values[3]) == "" {
		return Result{}, errors.New("repository profile requires GitHub repository, Linear team/project and reviewer")
	}
	maximum, err := strconv.ParseFloat(values[9], 64)
	if err != nil {
		return Result{}, errors.New("invalid reviewer estimate threshold")
	}
	lc, err := linear.New(ctx, linear.Options{TeamID: values[1]})
	if err != nil {
		return Result{}, errors.New("Linear initialization failed")
	}
	ticket, err := readTicketWithClient(ctx, lc, identifier, values[1], values[2])
	if err != nil {
		return Result{}, err
	}
	cfg, err := config.Load(home())
	if err != nil {
		return Result{}, errors.New("driver configuration could not be loaded")
	}
	gc, err := github.New(github.Options{HTTPTimeout: cfg.Tuning.GitHub.HTTPTimeout})
	if err != nil {
		return Result{}, errors.New("GitHub initialization failed")
	}
	oc := &orca.Client{WorkerStartWait: cfg.Tuning.Orca.WorkerStartWait, Timeout: cfg.Tuning.Orca.CallTimeout}
	e := Executor{Orca: oc, Linear: lc, GitHub: gc, Tools: &Commands{ReviewerCommand: values[3], ReviewerSmallCommand: values[8], ReviewerSmallMaxEstimate: &maximum}, Config: Config{Repo: repo, Owner: owner, Repository: name, Terminal: terminal, Unit: "executor"}}
	e.Config.Root, err = executorStateRoot()
	if err != nil {
		return Result{}, err
	}
	e.Config.WorkerIdleTimeout = workerIdleTimeout(os.Getenv("AGENT_WORKER_IDLE_MINUTES"))
	e.Config.Tier = "default"
	e.Config.Escalate = values[7]
	e.Config.TierSettings = map[string]string{"fast": values[4], "default": values[5], "strong": values[6]}
	parts := strings.Split(values[5], ":")
	if len(parts) != 3 || parts[0] == "" {
		return Result{}, errors.New("repository profile requires default worker tier")
	}
	e.Config.Agent, e.Config.Model, e.Config.Effort = parts[0], parts[1], parts[2]
	if e.Config.Model == "" {
		e.Config.Effort = ""
	}
	delegated = true
	e.Config.Merging = merging
	e.Config.Lane3Globs = strings.Fields(values[10])
	return e.Start(ctx, ticket)
}

// linear.Snapshot deliberately omits description. This executor-local read
// adapter asks only for the brief and fences it to the configured team/project.
func readTicket(ctx context.Context, identifier, team, project string) (Ticket, error) {
	lc, err := linear.New(ctx, linear.Options{TeamID: team})
	if err != nil {
		return Ticket{}, errors.New("Linear initialization failed")
	}
	return readTicketWithClient(ctx, lc, identifier, team, project)
}

func readTicketWithClient(ctx context.Context, lc *linear.Client, identifier, team, project string) (Ticket, error) {
	brief, err := lc.Brief(ctx, identifier, team, project)
	if err != nil {
		return Ticket{}, errors.New("Linear brief request failed")
	}
	ticket, err := ParseTicket(brief.ID, brief.Identifier, brief.Title, brief.Description)
	ticket.Estimate = brief.Estimate
	return ticket, err
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
