// Package scheduler owns machine-wide admission and executor recovery. Durable
// ownership lives in Linear comments; merge work is reconstructed each snapshot.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/pause"
)

type Ticket struct {
	// Rebuilt from history so #142 waiting receipts cannot erase a nil policy
	// reserved by an earlier squash intent for this queue.
	deploymentReservedQueue             string
	Progress                            *merge.Progress
	Handoff                             *merge.Handoff
	MergeProblem                        string
	ID, Identifier, Repo, Terminal, Run string
	Scope                               []string
	Blockers                            []string
	BlockersIncomplete                  bool
	Priority                            int
	CreatedAt, UpdatedAt                time.Time
	Owner                               machine.Owner
	Phase                               machine.Phase
	Attempt                             int
	Candidate, Legacy, Blocked          bool
}
type Legacy struct{ HasPR, Complete bool }
type ActionKind string

const (
	Claim   ActionKind = "claim"
	Resume  ActionKind = "resume"
	Merge   ActionKind = "merge"
	Release ActionKind = "release"
	Human   ActionKind = "human"
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
	Logger           *slog.Logger
	Config           config.Config
	Backend          Backend
	Now              func() time.Time
	MergeQueue       []Ticket
	Pauses           *pause.Manager
	NextInterval     time.Duration
	FullReconcile    bool
	adaptive         Adaptive
	linearQuotaRetry time.Time
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
func owned(t Ticket) bool { return t.Owner == machine.OwnerExecutor || t.Owner == machine.OwnerMerge }
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
	now := s.clock()
	remaining, pauseErr := s.checkPauses(ctx, now)
	s.MergeQueue = nil
	if s.Pauses.Paused("") {
		s.plan(now, false, remaining)
		return pauseErr
	}
	repos := map[string]config.Repository{}
	allowed := map[string]bool{}
	var problems []error
	if pauseErr != nil {
		problems = append(problems, pauseErr)
	}
	for _, r := range s.Config.Repositories {
		repos[r.Path] = r
		ok, err := s.Backend.AutomationDisabled(ctx, r)
		allowed[r.Path] = err == nil && ok
		if !allowed[r.Path] {
			problems = append(problems, errors.New("repository refused: automation enabled or unverifiable"))
		}
	}
	var err error
	tickets, err = s.Backend.Snapshot(ctx, s.Config.Repositories)
	if err != nil {
		return errors.New("scheduler snapshot failed")
	}
	snapshot = true
	// A successful snapshot replaces the stale quota observation. If the
	// refreshed budget is still critical, start its new recovery window before
	// allowing dispatch, even when no dependency probe is installed.
	s.linearQuotaRetry = time.Time{}
	remaining = s.checkLinearQuota(now)
	if s.Pauses.Paused("") {
		s.plan(now, false, remaining)
		for _, t := range tickets {
			if t.Candidate {
				s.decision(t, "skip", "paused")
			}
		}
		return errors.Join(problems...)
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
		if t.Owner == machine.OwnerExecutor && !t.UpdatedAt.IsZero() && s.Pauses.EffectiveElapsed(t.Repo, t.UpdatedAt) > time.Duration(r.StaleHours)*time.Hour {
			action = Action{Kind: Human, Attempt: t.Attempt, Reason: "in-flight ticket exceeded stale limit"}
		} else if t.Owner == machine.OwnerMerge || DeploymentBarrier(t) {
			s.MergeQueue = append(s.MergeQueue, t)
			continue
		} else if t.Owner == machine.OwnerExecutor {
			if unregistered(t) {
				// The claim never gained an executor terminal; after the grace period no
				// executor exists to provide evidence, so treat the claim as a failed executor.
				result := "dead"
				if now.Sub(t.UpdatedAt) < unregisteredGrace {
					result = "uncertain"
				}
				s.decision(t, "ownership", "unregistered", "terminal", t.Terminal, "run", t.Run, "terminal_exists", false, "active_dispatch", nil, "result", result)
				if now.Sub(t.UpdatedAt) < unregisteredGrace {
					continue
				}
			} else {
				absent, e1 := s.Backend.TerminalAbsent(ctx, t)
				inactive, e2 := s.Backend.NoActiveDispatch(ctx, t)
				s.ownership(t, absent, inactive, e1, e2)
				if e1 != nil || e2 != nil || !absent || !inactive {
					continue
				}
			}
			state := machine.State{Owner: machine.OwnerExecutor, Phase: t.Phase, Attempt: t.Attempt, ExecFailures: max(0, t.Attempt-1)}
			next, _, stepErr := machine.Step(state, machine.EvTick, machine.DefaultLimits)
			if stepErr != nil {
				return stepErr
			}
			if next.Owner == machine.OwnerHuman {
				action = Action{Kind: Human, Attempt: t.Attempt, Reason: "executor failed twice"}
			} else {
				action = Action{Kind: Resume, Attempt: next.Attempt, Phase: next.Phase}
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
		if action.Kind == Resume && !s.token(now) {
			continue
		}
		// Reserve before an external write: a failed response may already have
		// persisted ownership. Do not oversubscribe on an uncertain result.
		if !owned(t) && (action.Kind == Resume || action.Kind == Merge) {
			total++
			count[t.Repo]++
		}
		if err := s.Backend.Apply(ctx, t, action); err != nil {
			problems = append(problems, errors.New("ownership update failed"))
			continue
		}
		switch action.Kind {
		case Human:
			if owned(t) {
				total--
				count[t.Repo]--
			}
		case Merge:
			t.Owner = machine.OwnerMerge
			s.MergeQueue = append(s.MergeQueue, t)
		case Resume:
			t.Attempt = action.Attempt
			t.Phase = action.Phase
			t.Owner = machine.OwnerExecutor
			if err := s.launch(ctx, t); err != nil {
				problems = append(problems, errors.New("executor launch uncertain; ownership retained"))
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
		result := "success"
		if err != nil {
			result = "uncertain"
		}
		s.decision(t, "claim", "", "result", result, "err", err)
		if err != nil {
			problems = append(problems, errors.New("ticket claim failed"))
			continue
		}
		t.Attempt = state.Attempt
		t.Phase = state.Phase
		t.Owner = state.Owner
		if err = s.launch(ctx, t); err != nil {
			problems = append(problems, errors.New("executor launch uncertain; ownership retained"))
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
	defer lock.Close()
	s, err := initialize(ctx)
	if err != nil {
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
const unregisteredGrace = 3 * time.Minute

func unregistered(t Ticket) bool { return t.Terminal == "-" }
