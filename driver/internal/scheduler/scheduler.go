// Package scheduler owns machine-wide admission and executor recovery. Durable
// ownership lives in Linear comments; merge work is reconstructed each snapshot.
package scheduler

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

type Ticket struct {
	ID, Identifier, Repo, Terminal, Run string
	Priority                            int
	CreatedAt, UpdatedAt                time.Time
	Owner                               machine.Owner
	Phase                               machine.Phase
	Attempt                             int
	Candidate, Legacy                   bool
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
	Config      config.Config
	Backend     Backend
	Now         func() time.Time
	MergeQueue  []Ticket
	tokens      float64
	refill      time.Time
	initialized bool
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
func (s *Scheduler) Tick(ctx context.Context) error {
	if s.Backend == nil || s.Config.MaxInFlight < 1 || s.Config.TokenBucket.Capacity < 1 || s.Config.TokenBucket.RefillPerSecond <= 0 {
		return errors.New("invalid scheduler configuration")
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	repos := map[string]config.Repository{}
	allowed := map[string]bool{}
	var problems []error
	for _, r := range s.Config.Repositories {
		repos[r.Path] = r
		ok, err := s.Backend.AutomationDisabled(ctx, r)
		allowed[r.Path] = err == nil && ok
		if !allowed[r.Path] {
			problems = append(problems, errors.New("repository refused: automation enabled or unverifiable"))
		}
	}
	tickets, err := s.Backend.Snapshot(ctx, s.Config.Repositories)
	if err != nil {
		return errors.New("scheduler snapshot failed")
	}
	s.MergeQueue = nil
	count := map[string]int{}
	total := 0
	for _, t := range tickets {
		if owned(t) {
			total++
			count[t.Repo]++
		}
	}
	for _, t := range tickets {
		if !allowed[t.Repo] {
			continue
		}
		r := repos[t.Repo]
		action := Action{}
		if owned(t) && !t.UpdatedAt.IsZero() && now.Sub(t.UpdatedAt) > time.Duration(r.StaleHours)*time.Hour {
			action = Action{Kind: Human, Attempt: t.Attempt, Reason: "in-flight ticket exceeded stale limit"}
		} else if t.Owner == machine.OwnerMerge {
			s.MergeQueue = append(s.MergeQueue, t)
			continue
		} else if t.Owner == machine.OwnerExecutor {
			absent, e1 := s.Backend.TerminalAbsent(ctx, t)
			inactive, e2 := s.Backend.NoActiveDispatch(ctx, t)
			if e1 != nil || e2 != nil || !absent || !inactive {
				continue
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
			total++
			count[t.Repo]++
		case Resume:
			if !owned(t) {
				total++
				count[t.Repo]++
			}
			t.Attempt = action.Attempt
			t.Phase = action.Phase
			t.Owner = machine.OwnerExecutor
			if err := s.Backend.Launch(ctx, t); err != nil {
				problems = append(problems, errors.New("executor launch uncertain; ownership retained"))
			}
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
		if !allowed[t.Repo] || !t.Candidate || t.Legacy || owned(t) || t.Owner == machine.OwnerHuman {
			continue
		}
		if total >= s.Config.MaxInFlight {
			break
		}
		if count[t.Repo] >= repos[t.Repo].MaxInFlight {
			continue
		}
		if !s.token(now) {
			break
		}
		state, _, err := machine.Step(machine.Initial(), machine.EvClaim, machine.DefaultLimits)
		if err != nil {
			return err
		}
		if err = s.Backend.Apply(ctx, t, Action{Kind: Claim, Attempt: state.Attempt, Phase: state.Phase}); err != nil {
			problems = append(problems, errors.New("ticket claim failed"))
			continue
		}
		total++
		count[t.Repo]++
		t.Attempt = state.Attempt
		t.Phase = state.Phase
		t.Owner = state.Owner
		if err = s.Backend.Launch(ctx, t); err != nil {
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
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err = s.Tick(ctx); err != nil && report != nil {
			report(err)
		}
		if err = wait(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}
