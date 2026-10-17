// Package scheduler owns machine-wide admission and executor recovery. Durable
// ownership lives in ticket comments; merge work is reconstructed each snapshot.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/pause"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

type Ticket struct {
	Resolution *resolver.Record
	// Rebuilt from history so #142 waiting receipts cannot erase a nil policy
	// reserved by an earlier squash intent for this queue.
	deploymentReservedQueue             string
	Progress                            *merge.Progress
	Handoff                             *merge.Handoff
	MergeProblem                        string
	ID, Identifier, Repo, Terminal, Run string
	URL, Title                          string
	NotifyReason                        string
	NotifyEvents                        map[string]bool
	Number                              int
	Scope                               []string
	Blockers                            []string
	BlockersIncomplete                  bool
	Priority                            int
	CreatedAt, UpdatedAt                time.Time
	Owner                               machine.Owner
	Phase                               machine.Phase
	Attempt                             int
	Candidate, Legacy, Blocked, Held    bool
	// MaintenanceRefused marks an executor that exited on a maintenance
	// refusal after its latest ownership line; it resumes without a failure.
	MaintenanceRefused bool
	// GateSpecRefresh marks an executor that exited on a refresh receipt after
	// its latest ownership line; the stage resumes without a failure.
	GateSpecRefresh bool
}
type Legacy struct{ HasPR, Complete bool }
type ActionKind string

const (
	Claim   ActionKind = "claim"
	Resume  ActionKind = "resume"
	Merge   ActionKind = "merge"
	Release ActionKind = "release"
	Resolve ActionKind = "resolve"
)

type Action struct {
	Kind    ActionKind
	Attempt int
	Phase   machine.Phase
	Reason  string
}

// Backend returns errors for every incomplete or ambiguous read. Apply must
// durably reserve ownership before Launch: an uncertain launch is never replayed.
type Backend interface {
	Snapshot(context.Context, []config.Repository) ([]Ticket, error)
	AutomationDisabled(context.Context, config.Repository) (bool, error)
	TerminalAbsent(context.Context, Ticket) (bool, error)
	NoActiveDispatch(context.Context, Ticket) (bool, error)
	InspectLegacy(context.Context, Ticket) (Legacy, error)
	Apply(context.Context, Ticket, Action) error
	Launch(context.Context, Ticket) error
}

type Scheduler struct {
	Logger  *slog.Logger
	Config  config.Config
	Backend Backend
	// Repositories is every repository Tick reads: the eligible projects plus
	// retired ones whose in-flight work has not finished yet.
	Repositories []config.Repository
	// Discover, when set, yields the projects this driver may take over;
	// RefreshProjects re-runs it every DiscoverEvery (OF-43).
	Discover      func(context.Context) (projects.Result, error)
	DiscoverEvery time.Duration
	// SyncCore, when set, opens the core sync PR for a taken-over project
	// whose core copy lags the factory (OF-53); it is idempotent per factory
	// version, so RefreshProjects calls it on every discovery.
	SyncCore     func(context.Context, projects.Project) error
	retired      map[string]bool
	nextDiscover time.Time
	rejections   string
	coreBehind   string
	// identityDrift remembers the last-logged ledger-identity change per
	// repository path so RefreshProjects (OF-45) warns once per distinct drift.
	identityDrift map[string]string
	Now           func() time.Time
	MergeQueue    []Ticket
	Pauses        *pause.Manager
	NextInterval  time.Duration
	FullReconcile bool
	adaptive      Adaptive
	// quotaRetry caps the next inspection while any pause is active.
	quotaRetry       time.Time
	githubQuotaRetry time.Time
	tokens           float64
	refill           time.Time
	initialized      bool
}

func Acquire(path string) (*flock.Lock, error) { return flock.TryLock(path) }
func (s *Scheduler) token(now time.Time) bool {
	if !s.initialized {
		s.tokens = float64(s.Config.TokenBucket.Capacity)
		s.refill = now
		s.initialized = true
	}
	if now.After(s.refill) {
		s.tokens += now.Sub(s.refill).Seconds() * s.Config.TokenBucket.RefillPerSecond
		s.refill = now
	}
	if s.tokens > float64(s.Config.TokenBucket.Capacity) {
		s.tokens = float64(s.Config.TokenBucket.Capacity)
	}
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}
func owned(t Ticket) bool {
	return t.Owner == machine.OwnerExecutor || t.Owner == machine.OwnerMerge || t.Owner == machine.OwnerResolver
}
func priority(n int) int {
	if n == 0 {
		return 5
	}
	return n
}

// Tick checks automation exclusion before inspecting every repository in one
// snapshot. Refused repositories still count toward the machine admission limit.
func (s *Scheduler) Tick(ctx context.Context) (tickErr error) {
	started := time.Now()
	var tickets []Ticket
	snapshot := false
	defer func() { s.summary(started, tickets, snapshot, tickErr) }()
	if s.Backend == nil || s.Config.MaxInFlight < 1 || s.Config.TokenBucket.Capacity < 1 || s.Config.TokenBucket.RefillPerSecond <= 0 {
		return errors.New("invalid scheduler configuration")
	}
	defer s.pushStatus()
	now := s.clock()
	remaining, pauseErr := s.checkPauses(ctx, now)
	s.MergeQueue = nil
	if s.Pauses.Paused("") {
		if s.quotaRetry.IsZero() {
			// Wake within five minutes of a new pause even when the idle backoff is longer.
			s.quotaRetry = now.Add(5 * time.Minute)
		}
		s.plan(now, false, remaining)
		return pauseErr
	}
	// A failed rediscovery is already logged and keeps the previous projects.
	_ = s.RefreshProjects(ctx)
	repos := map[string]config.Repository{}
	allowed := map[string]bool{}
	var problems []error
	if pauseErr != nil {
		problems = append(problems, pauseErr)
	}
	for _, r := range s.Repositories {
		repos[r.Path] = r
		ok, err := s.Backend.AutomationDisabled(ctx, r)
		allowed[r.Path] = err == nil && ok
		if !allowed[r.Path] {
			problems = append(problems, errors.New("repository refused: automation enabled or unverifiable"))
		}
	}
	notify := func(t Ticket) {
		if category, _ := feishuNotification(t); category == "" {
			return
		}
		notifier, ok := s.Backend.(interface {
			NotifyFeishu(context.Context, Ticket) error
		})
		if !ok {
			return
		}
		if err := notifier.NotifyFeishu(ctx, t); err != nil {
			logger := s.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("Feishu notification failed", "ticket", t.Identifier, "error", err)
		}
	}
	var err error
	tickets, err = s.Backend.Snapshot(ctx, s.Repositories)
	if err != nil {
		category := "other"
		s.checkGitHubQuota(now)
		var evidence *SnapshotEvidenceError
		if errors.As(err, &evidence) && category == "other" {
			category = "evidence"
		}
		if !s.Pauses.Paused("") && errors.As(err, &evidence) {
			for _, ticket := range evidence.Tickets {
				if allowed[ticket.Repo] {
					notify(ticket)
				}
			}
		}
		reason := snapshotReason(err)
		s.logger().Warn("scheduler snapshot failed", "category", category, "reason", reason)
		return &snapshotError{reason: reason, err: err}
	}
	snapshot = true
	s.dropIdleRetired(tickets)
	status.PushObservation(s.clock(), func(view *status.View) {
		view.Tickets = nil
		for _, t := range tickets {
			row := ObservationRow(t, s.Repositories)
			switch {
			case owned(t):
			case t.Candidate:
				row.Stage = "candidate"
			case t.Owner == machine.OwnerHuman:
				row.Stage = "human"
			default:
				continue
			}
			view.Tickets = append(view.Tickets, row)
		}
	})
	// A successful snapshot replaces the stale quota observation. If the
	// refreshed budget is still critical, start its new recovery window before
	// allowing dispatch, even when no dependency probe is installed.
	s.quotaRetry = time.Time{}
	s.githubQuotaRetry = time.Time{}
	remaining = s.checkGitHubQuota(now)
	if s.Pauses.Paused("") {
		s.plan(now, false, remaining)
		for _, t := range tickets {
			if t.Candidate {
				s.decision(t, "skip", "paused")
			}
		}
		return errors.Join(problems...)
	}
	for _, t := range tickets {
		if allowed[t.Repo] {
			notify(t)
		}
	}
	busy := false
	for _, t := range tickets {
		if owned(t) || t.Candidate {
			busy = true
		}
	}
	s.plan(now, busy, remaining)
	s.MergeQueue = nil
	count := map[string]int{}
	total := 0
	for _, t := range tickets {
		if owned(t) {
			total++
			count[t.Repo]++
		}
	}
	conflicts := s.scopeConflicts(ctx, tickets, repos)
	for _, t := range tickets {
		if !allowed[t.Repo] || s.Pauses.Paused(t.Repo) {
			continue
		}
		r := repos[t.Repo]
		action := Action{}
		if t.Owner == machine.OwnerResolver || t.Owner == machine.OwnerHuman {
			if DeploymentBarrier(t) {
				s.MergeQueue = append(s.MergeQueue, t)
			}
			if t.Resolution != nil && t.Resolution.Phase == "human" {
				continue
			}
			if t.Resolution != nil && t.Resolution.Phase == "waiting" && now.Before(t.Resolution.RetryAt) {
				continue
			}
			if !owned(t) && (total >= s.Config.MaxInFlight || count[t.Repo] >= r.MaxInFlight) {
				continue
			}
			beat := beatLegacy
			if (t.Resolution == nil || (t.Resolution.Phase != "result" && t.Resolution.Phase != "waiting")) && t.Terminal != "" {
				beat = s.heartbeat(ctx, t, &problems)
				if beat == beatSkip {
					continue
				}
				if beat == beatLegacy && !unregistered(t) {
					absent, readErr := s.Backend.TerminalAbsent(ctx, t)
					if readErr != nil || !absent {
						continue
					}
				}
			}
			if beat == beatLegacy && (t.Resolution == nil || t.Resolution.Phase != "result") && unregistered(t) && now.Sub(t.UpdatedAt) < s.unregisteredGrace() {
				continue
			}
			action = Action{Kind: Resolve, Attempt: t.Attempt, Phase: machine.PhaseResolver, Reason: "continue agent resolution"}
		} else if t.Owner == machine.OwnerMerge || DeploymentBarrier(t) {
			s.MergeQueue = append(s.MergeQueue, t)
			continue
		} else if t.Owner == machine.OwnerExecutor {
			if unregistered(t) {
				// An executor beats before it registers its terminal, so a heartbeat
				// decides first. Without one, after the grace period no executor exists
				// to provide evidence, so treat the claim as a failed executor.
				switch s.heartbeat(ctx, t, &problems) {
				case beatSkip:
					continue
				case beatLegacy:
					result := "dead"
					if now.Sub(t.UpdatedAt) < s.unregisteredGrace() {
						result = "uncertain"
					}
					s.decision(t, "ownership", "unregistered", "terminal", t.Terminal, "run", t.Run, "terminal_exists", false, "active_dispatch", nil, "result", result)
					if now.Sub(t.UpdatedAt) < s.unregisteredGrace() {
						continue
					}
				}
			} else {
				switch s.heartbeat(ctx, t, &problems) {
				case beatSkip:
					continue
				case beatLegacy:
					absent, e1 := s.Backend.TerminalAbsent(ctx, t)
					inactive, e2 := s.Backend.NoActiveDispatch(ctx, t)
					s.ownership(t, absent, inactive, e1, e2)
					if e1 != nil || e2 != nil || !absent || !inactive {
						continue
					}
				}
			}
			if !t.UpdatedAt.IsZero() && s.Pauses.EffectiveElapsed(t.Repo, t.UpdatedAt) > time.Duration(r.StaleHours)*time.Hour {
				action = Action{Kind: Resolve, Attempt: t.Attempt, Phase: machine.PhaseResolver, Reason: "in-flight ticket exceeded stale limit"}
			} else if t.MaintenanceRefused || t.GateSpecRefresh {
				action = Action{Kind: Resume, Attempt: t.Attempt, Phase: t.Phase}
			} else {
				state := machine.State{Owner: machine.OwnerExecutor, Phase: t.Phase, Attempt: t.Attempt, ExecFailures: max(0, t.Attempt-1)}
				next, _, stepErr := machine.Step(state, machine.EvTick, machine.DefaultLimits)
				if stepErr != nil {
					return stepErr
				}
				if next.Owner == machine.OwnerResolver {
					action = Action{Kind: Resolve, Attempt: t.Attempt, Phase: machine.PhaseResolver, Reason: "executor failed twice"}
				} else {
					action = Action{Kind: Resume, Attempt: next.Attempt, Phase: next.Phase}
				}
			}
		} else if t.Legacy {
			legacy, readErr := s.Backend.InspectLegacy(ctx, t)
			if readErr != nil {
				problems = append(problems, errors.New("legacy inspection failed"))
				continue
			}
			switch {
			case legacy.HasPR:
				action = Action{Kind: Merge, Attempt: t.Attempt, Phase: machine.PhaseMergeQueue}
			case legacy.Complete:
				if total >= s.Config.MaxInFlight || count[t.Repo] >= r.MaxInFlight {
					continue
				}
				action = Action{Kind: Resume, Attempt: max(1, t.Attempt), Phase: machine.PhaseGate}
			default:
				action = Action{Kind: Release}
			}
		} else {
			continue
		}
		if (action.Kind == Resume || action.Kind == Resolve) && !s.token(now) {
			continue
		}
		// Reserve before an external write: a failed response may already have
		// persisted ownership. Do not oversubscribe on an uncertain result.
		if !owned(t) && (action.Kind == Resume || action.Kind == Merge || action.Kind == Resolve) {
			total++
			count[t.Repo]++
		}
		if err := s.Backend.Apply(ctx, t, action); err != nil {
			result := outcome(err)
			s.decision(t, "apply", "", "action", action.Kind, "result", result, "err", err)
			if result != "deferred" {
				problems = append(problems, fmt.Errorf("ownership update failed: %s %s: %s", t.Identifier, action.Kind, applyReason(err)))
			}
			continue
		}
		s.pushTicket(t, action)
		switch action.Kind {
		case Merge:
			t.Owner = machine.OwnerMerge
			s.MergeQueue = append(s.MergeQueue, t)
		case Resolve:
			t.Owner, t.Phase = machine.OwnerResolver, machine.PhaseResolver
			if err := s.launch(ctx, t); err != nil {
				problems = append(problems, fmt.Errorf("resolver launch uncertain; ownership retained: %s: %s", t.Identifier, applyReason(err)))
			}
		case Resume:
			t.Attempt = action.Attempt
			t.Phase = action.Phase
			t.Owner = machine.OwnerExecutor
			if err := s.launch(ctx, t); err != nil {
				problems = append(problems, fmt.Errorf("executor launch uncertain; ownership retained: %s: %s", t.Identifier, applyReason(err)))
			}
		}
	}
	if merger, ok := s.Backend.(interface {
		AdvanceMerges(context.Context, []Ticket) error
	}); ok {
		if err := merger.AdvanceMerges(ctx, s.MergeQueue); err != nil {
			problems = append(problems, err)
		}
	}
	if runners, ok := s.Backend.(interface{ CheckRunners(context.Context) error }); ok {
		// A runner alert never blocks ticket flow; its failure is only logged.
		if err := runners.CheckRunners(ctx); err != nil {
			s.logger().Warn("runner health check failed", "err", err)
		}
	}
	sort.SliceStable(tickets, func(i, j int) bool {
		a, b := tickets[i], tickets[j]
		if priority(a.Priority) != priority(b.Priority) {
			return priority(a.Priority) < priority(b.Priority)
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	for _, t := range tickets {
		if !t.Candidate {
			continue
		}
		reason := ""
		switch {
		case !allowed[t.Repo] || s.Pauses.Paused(t.Repo):
			reason = "paused"
		case s.retired[t.Repo]:
			reason = "retired"
		case t.Blocked:
			reason = "blocked"
		case t.Legacy:
			reason = "legacy"
		case owned(t) || t.Owner == machine.OwnerHuman:
			reason = "owned"
		case total >= s.Config.MaxInFlight || count[t.Repo] >= repos[t.Repo].MaxInFlight:
			reason = "capacity"
		case repos[t.Repo].Env["AGENT_SCOPE_AVOID"] != "0" && scopesOverlap(t.Scope, conflicts[t.Repo]):
			reason = "scope"
		case !s.token(now):
			reason = "token"
		}
		if reason != "" {
			if reason == "blocked" {
				s.decision(t, "skip", reason, "blockers", t.Blockers, "blockers_incomplete", t.BlockersIncomplete)
			} else {
				s.decision(t, "skip", reason)
			}
			continue
		}
		state, _, err := machine.Step(machine.Initial(), machine.EvClaim, machine.DefaultLimits)
		if err != nil {
			return err
		}
		total++
		count[t.Repo]++
		// An uncertain claim still reserves its scope for this tick.
		conflicts[t.Repo] = append(conflicts[t.Repo], t.Scope...)
		err = s.Backend.Apply(ctx, t, Action{Kind: Claim, Attempt: state.Attempt, Phase: state.Phase})
		result := outcome(err)
		s.decision(t, "claim", "", "result", result, "err", err)
		if err != nil {
			if result != "deferred" {
				problems = append(problems, fmt.Errorf("ticket claim failed: %s %s: %s", t.Identifier, Claim, applyReason(err)))
			}
			continue
		}
		s.pushTicket(t, Action{Kind: Claim, Phase: state.Phase})
		t.Attempt = state.Attempt
		t.Phase = state.Phase
		t.Owner = state.Owner
		if err = s.launch(ctx, t); err != nil {
			problems = append(problems, fmt.Errorf("executor launch uncertain; ownership retained: %s: %s", t.Identifier, applyReason(err)))
		}
	}
	return errors.Join(problems...)
}

// Serve keeps the singleton lock across initialization, every inspection and
// context-aware wait. Report receives sanitized inspection errors.
func Serve(ctx context.Context, lockPath string, initialize func(context.Context) (*Scheduler, error), wait func(context.Context) error, report func(error)) error {
	lock, err := Acquire(lockPath)
	if err != nil {
		return errors.New("driver serve singleton lock unavailable")
	}
	defer func() { _ = lock.Close() }() // Cleanup releases the descriptor even if explicit unlock fails.
	s, err := initialize(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	var next time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}
		if !s.clock().Before(next) {
			if err = s.Tick(ctx); err != nil && report != nil {
				report(err)
			}
			delay := s.NextInterval
			if delay <= 0 {
				delay = time.Minute
			}
			next = s.clock().Add(delay)
		}
		if err = wait(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// unregisteredGrace bounds how long a claim may wait for its executor to record a terminal.
func (s *Scheduler) unregisteredGrace() time.Duration {
	return s.Config.Tuning.Scheduler.UnregisteredGrace
}

func unregistered(t Ticket) bool { return t.Terminal == "-" }

// applyReason is the safety-error handling OF-99 introduced, reused for an
// Apply failure (OF-100): a single line, truncated, with the same redaction
// snapshotReason applies, so a Backend implementation's raw error text can
// never carry a credential or a response body into the problems list.
const applyReasonLimit = 200

func applyReason(err error) string {
	reason := logx.RedactText(strings.Join(strings.Fields(err.Error()), " "))
	if runes := []rune(reason); len(runes) > applyReasonLimit {
		reason = string(runes[:applyReasonLimit])
	}
	return reason
}

// Adapters return fixed-text errors, except decoding imported comment history
// into task.Comment: its encoding/json and time errors quote the rejected input.
func snapshotReason(err error) string {
	text := err.Error()
	root := err
	for next := errors.Unwrap(root); next != nil; next = errors.Unwrap(root) {
		root = next
	}
	var syntax *json.SyntaxError
	var mismatch *json.UnmarshalTypeError
	var timestamp *time.ParseError
	decoding := errors.As(root, &syntax) || errors.As(root, &mismatch) || errors.As(root, &timestamp) || strings.HasPrefix(root.Error(), "json: ")
	if decoding && strings.HasSuffix(text, root.Error()) {
		text = strings.TrimSuffix(text, root.Error()) + "invalid JSON"
	}
	return logx.RedactText(text)
}

// snapshotError text carries only the redacted reason; Unwrap keeps the cause.
type snapshotError struct {
	reason string
	err    error
}

func (e *snapshotError) Error() string { return "scheduler snapshot failed: " + e.reason }
func (e *snapshotError) Unwrap() error { return e.err }
