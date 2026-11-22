package executor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// RunTicket binds a Run to the calling Orca terminal. The repository profile is
// trusted local configuration; ticket bodies are fetched separately as data.
// launch describes the scheduler's launch; verifyMode reads the AGENT_MODE of a Feishu repository path and its bound ledger.
func RunTicket(ctx context.Context, identifier string, merging bool, launch Launch, verifyMode func(context.Context, string, map[string]string) (ScopeMode, error)) (result Result, err error) {
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
	e.Launch = launch
	e.bindMode(repo, storeEnv, verifyMode)
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

// bindMode fixes where an executor bound to ledger reads its AGENT_MODE. Only the
// Feishu ledger proves who wrote an event, and the scheduler tracks no other
// backend, whose mode therefore comes from the profile alone.
func (e *Executor) bindMode(repo string, ledger map[string]string, verify func(context.Context, string, map[string]string) (ScopeMode, error)) {
	e.CurrentLane3 = func(ctx context.Context) ([]string, string, error) {
		current, _, err := repositoryExecutor(ctx, repo)
		if err != nil {
			return nil, "", err
		}
		version, err := ProfileVersion(repo)
		return current.Config.Lane3Globs, version, err
	}
	if ledger["AGENT_ISSUE_BACKEND"] == "feishu" {
		e.LaunchReceipts = true
		e.VerifyMode = func(ctx context.Context) (ScopeMode, error) { return verify(ctx, repo, ledger) }
		return
	}
	e.VerifyMode = func(ctx context.Context) (ScopeMode, error) {
		current, bound, err := repositoryExecutor(ctx, repo)
		if err != nil {
			return ScopeMode{}, err
		}
		for _, key := range []string{"AGENT_ISSUE_BACKEND", "AGENT_GH_REPO"} {
			if bound[key] != ledger[key] {
				return ScopeMode{}, fmt.Errorf("%s no longer names the bound ledger", key)
			}
		}
		if githubAPI(bound) != githubAPI(ledger) {
			return ScopeMode{}, errors.New("AGENT_GH_API_URL no longer names the bound ledger")
		}
		if mode := current.Config.Mode; mode != "manual" && mode != "auto" {
			return ScopeMode{}, errors.New("AGENT_MODE must be manual or auto")
		}
		return ScopeMode{Mode: current.Config.Mode, Lane3Globs: current.Config.Lane3Globs}, nil
	}
}

// githubAPI is the endpoint github.New derives from a ledger's configuration.
func githubAPI(ledger map[string]string) string {
	return strings.TrimRight(cmp.Or(ledger["AGENT_GH_API_URL"], "https://api.github.com"), "/")
}

// Launch is how the scheduler started this executor; the zero value is an
// operator's start, which no launch receipt covers.
type Launch struct {
	Dispatched bool
	// CallTimeout bounds the scheduler's Orca call that creates the terminal.
	CallTimeout time.Duration
}

// ScopeMode is a repository's AGENT_MODE and the lane 3 globs read with it, and
// the profile version they hold for; "" where the scheduler tracks nothing.
type ScopeMode struct {
	Mode       string
	Lane3Globs []string
	Profile    string
}

// ProfileVersion uses inode change time so that moving the original profile
// back into place invalidates a receipt even when its contents and mtime match.
func ProfileVersion(path string) (string, error) {
	info, err := os.Stat(filepath.Join(path, "scripts", "agent-gates", "agent-graph.env"))
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("repository profile stat unavailable")
	}
	fields := reflect.ValueOf(stat).Elem()
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := fields.FieldByName(name)
		if !field.IsValid() {
			continue
		}
		if changed, ok := field.Interface().(syscall.Timespec); ok {
			return strconv.FormatInt(changed.Nano(), 10), nil
		}
	}
	return "", errors.New("repository profile change time unavailable")
}

// scopeMode is the AGENT_MODE that governs a worker's scope request, empty when
// it cannot be verified and the request belongs to the resolver. A dispatched
// executor takes the scheduler's launch receipt and nothing else; any other
// start verifies the repository's current mode.
func (e *Executor) scopeMode(ctx context.Context, t Ticket, terminal string) (ScopeMode, error) {
	if e.Launch.Dispatched {
		return e.awaitLaunchedMode(ctx, t, terminal)
	}
	if e.VerifyMode == nil {
		return ScopeMode{}, nil
	}
	err := e.pollLedger(ctx, t, launchReceiptMargin, func(comments []task.Comment) (bool, error) {
		return operatorOwned(comments, terminal)
	})
	if errors.Is(err, errLedgerPending) {
		e.warn("%s registration of terminal %s not visible", t.Identifier, terminal)
		return ScopeMode{}, nil
	}
	if err != nil {
		return ScopeMode{}, err
	}
	current, err := e.VerifyMode(ctx)
	if err != nil {
		e.warn("%s repository mode unverified: %s", t.Identifier, logErrorSummary(err))
		return ScopeMode{}, nil
	}
	return current, nil
}

// The scheduler can only write the receipt after Orca returns the terminal, so a
// recovered report can ask for scope first. The wait is the scheduler's bound on
// that Orca call plus launchReceiptMargin for the ledger write.
const (
	launchReceiptMargin   = 10 * time.Minute
	launchReceiptInterval = 10 * time.Second
)

func (e *Executor) awaitLaunchedMode(ctx context.Context, t Ticket, terminal string) (mode ScopeMode, err error) {
	if !e.LaunchReceipts {
		return ScopeMode{}, nil
	}
	err = e.pollLedger(ctx, t, e.Launch.CallTimeout+launchReceiptMargin, func(comments []task.Comment) (bool, error) {
		var pending bool
		mode, pending, err = launchedMode(comments, terminal)
		return !pending, err
	})
	if errors.Is(err, errLedgerPending) {
		e.warn("%s launch mode receipt missing for terminal %s", t.Identifier, terminal)
		return ScopeMode{}, nil
	}
	return mode, err
}

var errLedgerPending = errors.New("ledger event not visible")

// pollLedger rereads the ticket's events until settled or limit has passed.
func (e *Executor) pollLedger(ctx context.Context, t Ticket, limit time.Duration, settled func([]task.Comment) (bool, error)) error {
	wait := e.Sleep
	if wait == nil {
		wait = pause
	}
	for waited := time.Duration(0); ; waited += launchReceiptInterval {
		comments, err := e.Issues.ListComments(ctx, t.ID)
		if err != nil {
			return safeDependencyError(err)
		}
		if done, err := settled(comments); err != nil || done {
			return err
		}
		if waited >= limit {
			return errLedgerPending
		}
		if err := wait(ctx, launchReceiptInterval); err != nil {
			return err
		}
	}
}

// launchedMode reads the mode the scheduler recorded for the terminal it created;
// pending means the receipt or the registration is not visible yet. Only the first executor registered in that
// terminal is the dispatched one. Once the ticket changes hands or launches again
// this executor no longer owns it and may not write it.
func launchedMode(comments []task.Comment, terminal string) (mode ScopeMode, pending bool, err error) {
	comments = slices.Clone(comments)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	receipts, registrations, own := 0, 0, "term="+terminal
	for _, comment := range comments {
		if receipt, ok := strings.CutPrefix(comment.Body, LaunchModePrefix); ok && comment.Source == task.SourceDriver {
			head, lane3, _ := strings.Cut(receipt, "\n"+LaunchLane3Prefix)
			words := strings.Fields(head)
			switch {
			case !slices.Contains(words, own):
				if receipts+registrations > 0 {
					return ScopeMode{}, false, errLaunchSuperseded
				}
			case slices.Contains(words, "mode=auto"):
				receipts, mode = receipts+1, ScopeMode{Mode: "auto", Lane3Globs: strings.Fields(lane3), Profile: receiptField(words, "profile")}
			case slices.Contains(words, "mode=manual"):
				receipts, mode = receipts+1, ScopeMode{Mode: "manual", Lane3Globs: strings.Fields(lane3), Profile: receiptField(words, "profile")}
			default:
				receipts += 2
			}
		}
		registered, foreign := ownershipEvent(comment, own)
		if registered {
			registrations++
		}
		if foreign && receipts+registrations > 0 {
			return ScopeMode{}, false, errLaunchSuperseded
		}
	}
	if registrations > 1 || receipts > 1 {
		return ScopeMode{}, false, nil
	}
	return mode, receipts == 0 || registrations == 0, nil
}

// ownershipEvent reports that an event registers this terminal's executor, or
// hands the ticket to anyone else.
func ownershipEvent(comment task.Comment, own string) (registered, foreign bool) {
	if !comment.CarriesProtocol() {
		return false, false
	}
	foreign = strings.HasPrefix(comment.Body, "转人: ")
	for _, line := range strings.Split(comment.Body, "\n") {
		ownership, ok := strings.CutPrefix(line, "归属: ")
		if !ok {
			continue
		}
		words := strings.Fields(ownership)
		if len(words) > 0 && words[0] == string(machine.OwnerExecutor) && slices.Contains(words, own) {
			registered = true
		} else {
			foreign = true
		}
	}
	return registered, foreign
}

func receiptField(words []string, key string) string {
	for _, word := range words {
		if value, ok := strings.CutPrefix(word, key+"="); ok {
			return value
		}
	}
	return ""
}

// autoApproval rechecks an approval on the snapshot it decides on and supplies the
// lane 3 globs it must avoid, read after that snapshot. A registration already seen
// and now missing proves no ownership.
func (e *Executor) autoApproval(ctx context.Context, t Ticket, terminal string, mode ScopeMode) func([]task.Comment) (string, bool, error) {
	return func(comments []task.Comment) (string, bool, error) {
		if err := e.autoOwned(comments, terminal); err != nil {
			return "", false, err
		}
		if e.Launch.Dispatched {
			// Registering twice in one terminal voids the receipt already read.
			if current, _, err := launchedMode(comments, terminal); err != nil || current.Mode != "auto" {
				return "", false, err
			}
		}
		var current []string
		if e.CurrentLane3 != nil {
			globs, version, err := e.CurrentLane3(ctx)
			if err == nil && e.Launch.Dispatched && version != mode.Profile {
				err = errors.New("repository profile changed since dispatch")
			}
			if err != nil {
				e.warn("%s lane 3 globs unverified: %s", t.Identifier, logErrorSummary(err))
				return "", false, nil
			}
			current = globs
		}
		return strings.Join(slices.Concat(e.Config.Lane3Globs, mode.Lane3Globs, current), " "), true, nil
	}
}

func (e *Executor) autoOwned(comments []task.Comment, terminal string) error {
	if e.Launch.Dispatched {
		_, pending, err := launchedMode(comments, terminal)
		if err == nil && pending {
			return errLaunchSuperseded
		}
		return err
	}
	held, err := operatorOwned(comments, terminal)
	if err == nil && !held {
		return errLaunchSuperseded
	}
	return err
}

// operatorOwned reports that this terminal's latest registration still holds the
// ticket, false while no registration is visible. An executor's line before it is
// the reservation it takes over; any other owner's means the ticket was lost.
func operatorOwned(comments []task.Comment, terminal string) (bool, error) {
	comments = slices.Clone(comments)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	held, lost := false, false
	for _, comment := range comments {
		registered, foreign := ownershipEvent(comment, "term="+terminal)
		switch {
		case registered:
			held, lost = true, foreign
		case foreign:
			lost = held || !strings.Contains("\n"+comment.Body, "\n归属: "+string(machine.OwnerExecutor)+" ")
		}
	}
	if lost {
		return false, errLaunchSuperseded
	}
	return held, nil
}

var errLaunchSuperseded = errors.New("ticket no longer owned by this executor terminal")

// LaunchModePrefix starts the scheduler's receipt that binds the AGENT_MODE it
// tracked at launch to the executor terminal it created.
const (
	LaunchModePrefix = "派工档位: "
	// LaunchLane3Prefix starts its second line, the lane 3 globs tracked with it.
	LaunchLane3Prefix = "lane3: "
)

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
	b, err := runx.Run(ctx, runx.Spec{Path: "/bin/bash", Args: []string{"-c", `source "$1" >/dev/null || exit; printf '%s\0' "$AGENT_GH_REPO" "$AGENT_ROLE_REVIEWER_CMD" "$AGENT_ROLE_REVIEWER" "$AGENT_ROLE_REVIEWER_CMD_SMALL" "$AGENT_ROLE_REVIEWER_SMALL" "$AGENT_ROLE_SECOND_REVIEWER_CMD" "$AGENT_ROLE_SECOND_REVIEWER" "${AGENT_TIER_DEFAULT:-}" "${AGENT_TIER_STRONG:-}" "${AGENT_TIER_ESCALATE:-0}" "${AGENT_REVIEWER_SMALL_MAX_ESTIMATE:-2}" "${AGENT_LANE3_GLOBS:-}" "${AGENT_ISSUE_BACKEND:-}" "${AGENT_GH_API_URL:-}" "${AGENT_FEISHU_BASE:-}" "${AGENT_FEISHU_PROJECT:-}" "${AGENT_INSTALL_CMD:-true}" "${AGENT_PROFILE-}" "${AGENT_WORKER_SKILLS:-}"; [ -z "${AGENT_MODE+x}" ] || printf '%s\0' AGENT_MODE "$AGENT_MODE"; for k in $(compgen -v AGENT_TIER_) $(compgen -v AGENT_ROLE_REVIEWER); do printf '%s\0' "$k" "${!k}"; done`, "driver-profile", profile}, Dir: repo, Timeout: 10 * time.Second})
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
	entry, err := modelhealth.Parse(effective("AGENT_TIER_DEFAULT"))
	if err != nil {
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
		if _, err := modelhealth.Parse(settings[tier]); err != nil {
			return Executor{}, nil, fmt.Errorf("AGENT_TIER_CHAIN tier %s requires %s as agent:model:effort", tier, key)
		}
	}
	for _, tier := range slices.Collect(maps.Keys(settings)) {
		key := config.FallbackKey("AGENT_TIER_" + strings.ToUpper(tier))
		if _, err := config.ParseFallback(effective(key)); err != nil {
			return Executor{}, nil, fmt.Errorf("%s: %w", key, err)
		}
		settings[key] = effective(key)
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
	mode, configured := explicit["AGENT_MODE"]
	if !configured {
		mode = "manual"
	}
	cfg := Config{Repo: repo, Owner: owner, Repository: name, Unit: "executor", Tier: "default",
		Agent: entry.Agent, Model: entry.Model, Effort: entry.Effort, Escalate: escalate, Mode: mode,
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
	mainCandidates, err := config.ReviewerCandidates(explicit, sources, "AGENT_ROLE_REVIEWER", "AGENT_ROLE_REVIEWER_CMD")
	if err != nil {
		return Executor{}, nil, err
	}
	smallCandidates, err := config.ReviewerCandidates(explicit, sources, "AGENT_ROLE_REVIEWER_SMALL", "AGENT_ROLE_REVIEWER_CMD_SMALL")
	if err != nil {
		return Executor{}, nil, err
	}
	root, err := executorStateRoot()
	if err != nil {
		return Executor{}, nil, err
	}
	tools := &Commands{ReviewerCommand: commands.Main, ReviewerSmallCommand: commands.Small, ReviewerSmallMaxEstimate: &maximum, InstallCommand: values[16], ModelProxyURL: proxy,
		ReviewerCandidates: mainCandidates, ReviewerSmallCandidates: smallCandidates, StateRoot: root}
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
