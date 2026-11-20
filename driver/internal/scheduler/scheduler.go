// Package scheduler owns machine-wide admission and executor recovery. Durable
// ownership lives in ticket comments; merge work is reconstructed each snapshot.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/hostmem"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

type Ticket struct {
	ScopeWaits    []status.ScopeWait
	Dependencies  []status.Dependency
	stageTimeline []stageStart
	Resolution    *resolver.Record
	// Rebuilt from history so #142 waiting receipts cannot erase a nil policy
	// reserved by an earlier squash intent for this queue.
	deploymentReservedQueue             string
	Progress                            *merge.Progress
	Handoff                             *merge.Handoff
	MergeProblem                        string
	ID, Identifier, Repo, Terminal, Run string
	URL, Title                          string
	NotifyReason                        string
	Parked                              string // latest 搁置 reason while labeled parked
	Resumed                             bool   // parked ticket a person restored to ready-for-agent
	NotifyEvents                        map[string]bool
	CrossReviewed                       map[string]bool
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
	AutoFailed                          bool
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
	// probed is the configuration a rejected repository's in-flight probe
	// first handed the backend, which stays bound to it until the path is
	// tracked; tracking then starts from it instead of the latest discovery.
	probed       map[string]config.Repository
	Now          func() time.Time
	MergeQueue   []Ticket
	NextInterval time.Duration
	// NextTickAt is when Serve runs the next Tick; zero before the first Tick.
	NextTickAt    time.Time
	FullReconcile bool
	adaptive      Adaptive
	// rounds and maintenanceMode decide when a round's ledger reads skip the
	// table-rev reuse (fullSync).
	rounds           int
	maintenanceMode  string
	tokens           float64
	refill           time.Time
	initialized      bool
	lastRecoveryRepo string
	// tickStarted and tickEnded bound Serve's latest Tick; wakeReason names
	// why the next one runs.
	tickStarted, tickEnded time.Time
	wakeReason             string
	orcaRuntime            status.OrcaRuntime
	// orcaRestartsExhausted is set once the human notice after the last
	// automatic orca-serve restart is sent; recovery clears it.
	orcaRestartsExhausted bool
	// orcaPauseTicket selects the runtime-pause chat; unreachable notices use
	// the first schedulable repository of the latest successful inspection.
	orcaPauseTicket      Ticket
	orcaNotificationRepo string
	// orcaExhaustedLogged keeps the no-repository human-notice warning to one
	// line until a repository can receive the notice.
	orcaExhaustedLogged bool
	// HostMemory, when set, is read once per Tick; a hold stops every new
	// terminal launch until the reading recovers (OF-603).
	HostMemory func() (hostmem.Reading, error)
	hostMemory status.HostMemory
	// The hold decision and its start survive unreadable rounds for hysteresis.
	memoryHeld      bool
	memoryHeldSince time.Time
	// Each continuous pause can trigger one successful notification.
	memoryPausedSince time.Time
	memoryNotified    bool
	runtimePending    map[string]Ticket
	// relaunchedAt restarts the registration grace of a reservation whose
	// launch succeeded only after an Orca outage; its claim time is older.
	relaunchedAt map[string]time.Time
}

// fullSyncEvery is how many rounds pass between full ledger reads, besides the
// first round and the first round after maintenance returns to running.
const fullSyncEvery = 10

// OrcaRestartGrace is shared by runtime-pause and unreachable recovery and doctor.
const OrcaRestartGrace = 10 * time.Minute

const (
	orcaRuntimeFailureThreshold = 3
	orcaRestartInterval         = 30 * time.Minute
	orcaRestartLimit            = 3
)

// fullSync is true when this round reads the ledger in full. An unreadable
// maintenance state also reads in full, which is never wrong.
// resumesResolution reports a restored parked ticket whose frozen merge evidence
// forbids a fresh claim; Agent处置 continues from that evidence instead.
func (t Ticket) resumesResolution() bool {
	return t.Candidate && t.Resumed && (t.Handoff != nil || t.MergeProblem != "")
}

func (s *Scheduler) fullSync() bool {
	full := s.rounds%fullSyncEvery == 0
	s.rounds++
	state, err := maintenance.Read()
	if err != nil {
		s.maintenanceMode = ""
		return true
	}
	recovered := s.maintenanceMode != "" && s.maintenanceMode != "running" && state.Mode == "running"
	s.maintenanceMode = state.Mode
	return full || recovered
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
	timing := tickPhases{}
	ctx = withPhases(ctx, timing)
	full := s.fullSync()
	if full {
		ctx = issues.WithFullSync(ctx)
	}
	mark := started
	lap := func(phase string) {
		timing.add(phase, mark)
		mark = time.Now()
	}
	defer func() { s.summary(started, tickets, snapshot, full, timing, tickErr) }()
	if s.Backend == nil || s.Config.MaxInFlight < 1 || s.Config.TokenBucket.Capacity < 1 || s.Config.TokenBucket.RefillPerSecond <= 0 {
		return errors.New("invalid scheduler configuration")
	}
	now := s.clock()
	s.checkHostMemory(ctx, now)
	memoryHeld := s.hostMemory.Paused
	admitted := 0
	takeToken := func() bool {
		if admitted >= s.Config.TokenBucket.Capacity || !s.token(s.clock()) {
			return false
		}
		admitted++
		return true
	}
	tokenWait := func(t Ticket) {
		status.Push(s.clock(), func(view *status.View) {
			for i := range view.Tickets {
				row := &view.Tickets[i]
				if row.Repo == t.Repo && row.Identifier == t.Identifier {
					row.BlockedReason = "token"
					row.PushedAt = time.Time{}
					return
				}
			}
		})
	}
	s.MergeQueue = nil
	// A failed rediscovery is already logged and keeps the previous projects.
	_ = s.RefreshProjects(ctx)
	repos := map[string]config.Repository{}
	allowed := map[string]bool{}
	var problems []error
	answered, unreachable := false, false
	for _, r := range s.Repositories {
		repos[r.Path] = r
		ok, err := s.Backend.AutomationDisabled(ctx, r)
		allowed[r.Path] = err == nil && ok
		answered = answered || err == nil
		unreachable = unreachable || orcaUnreachable(ctx, err)
		if !allowed[r.Path] {
			problems = append(problems, errors.New("repository refused: automation enabled or unverifiable"))
		}
	}
	s.observeOrcaReads(ctx, answered, unreachable)
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
	lap("projects")
	var err error
	observedFrom := s.clock()
	tickets, err = s.Backend.Snapshot(ctx, s.Repositories)
	lap("snapshot")
	if err != nil {
		category := "other"
		var evidence *SnapshotEvidenceError
		if errors.As(err, &evidence) && category == "other" {
			category = "evidence"
		}
		if errors.As(err, &evidence) {
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
	// Without a tracked repository no automation read runs, so no outage is timed.
	if len(s.Repositories) == 0 {
		s.orcaRuntime.UnreachableSince = time.Time{}
	}
	if answered {
		s.orcaNotificationRepo = ""
		for _, r := range s.Repositories {
			if allowed[r.Path] && !s.retired[r.Path] {
				s.orcaNotificationRepo = r.Path
				break
			}
		}
	}
	conflicts := s.scopeConflicts(ctx, tickets, repos)
	observeScopeWaits(tickets, repos, conflicts)
	rows := ObservationRows(tickets, s.Repositories, s.clock())
	status.PushObservation(s.clock(), observedFrom, func(view *status.View) {
		view.Today = nil
		if source, ok := s.Backend.(interface {
			TodayOutput(time.Time) *status.TodayOutput
		}); ok {
			view.Today = source.TodayOutput(s.clock())
		}
		view.Tickets = nil
		for index, t := range tickets {
			row := rows[index]
			if !owned(t) && row.Stage == "" && t.Parked == "" && !DeploymentBarrier(t) {
				continue
			}
			view.Tickets = append(view.Tickets, row)
		}
	})
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
	s.plan(now, busy)
	if s.orcaRuntime.Paused {
		probe, ok := s.Backend.(interface {
			ProbeOrcaRuntime(context.Context, string) error
		})
		if !ok {
			return errOrcaRuntimePaused
		}
		repo := ""
		for _, ticket := range tickets {
			if allowed[ticket.Repo] {
				repo = ticket.Repo
				break
			}
		}
		if repo == "" {
			return errors.New("orca runtime fault: no repository available for recovery probe")
		}
		if err := probe.ProbeOrcaRuntime(ctx, repo); err != nil {
			var createErr *orcaProbeCreateError
			if errors.As(err, &createErr) {
				s.restartOrcaService(ctx)
			}
			return fmt.Errorf("orca runtime recovery probe: %w", err)
		}
		s.clearOrcaRuntime()
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
	start := 0
	for i, repo := range s.Repositories {
		if repo.Path == s.lastRecoveryRepo {
			start = i + 1
			break
		}
	}
	order := make(map[string]int, len(s.Repositories))
	for i := range s.Repositories {
		order[s.Repositories[(start+i)%len(s.Repositories)].Path] = i
	}
	recovery := append([]Ticket(nil), tickets...)
	sort.SliceStable(recovery, func(i, j int) bool { return order[recovery[i].Repo] < order[recovery[j].Repo] })
	for _, t := range recovery {
		if !allowed[t.Repo] {
			continue
		}
		if s.orcaRuntime.Paused {
			continue
		}
		if !unregistered(t) {
			delete(s.relaunchedAt, t.ID)
		}
		if pending, ok := s.runtimePending[t.ID]; ok {
			// Reuse only the reservation that failed before a terminal existed.
			if t.Owner == pending.Owner && t.Attempt == pending.Attempt && t.Phase == pending.Phase && unregistered(t) {
				if memoryHeld {
					s.decision(t, "skip", "memory", "action", "runtime-retry")
				} else if s.token(now) {
					if err := s.launchWithRuntimeCheck(ctx, t); err != nil {
						problems = append(problems, err)
					}
				}
				continue
			}
			delete(s.runtimePending, t.ID)
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
			if beat == beatLegacy && (t.Resolution == nil || t.Resolution.Phase != "result") && unregistered(t) && now.Sub(s.claimedAt(t)) < s.unregisteredGrace() {
				continue
			}
			action = Action{Kind: Resolve, Attempt: t.Attempt, Phase: machine.PhaseResolver, Reason: "continue agent resolution"}
		} else if t.resumesResolution() {
			// Before the merge lane: conflicting evidence stops that lane forever.
			// The barrier still holds the lane until the resolver clears it.
			if DeploymentBarrier(t) {
				s.MergeQueue = append(s.MergeQueue, t)
			}
			if total >= s.Config.MaxInFlight || count[t.Repo] >= r.MaxInFlight {
				continue
			}
			action = Action{Kind: Resolve, Attempt: t.Attempt, Phase: machine.PhaseResolver, Reason: "parked ticket resumed"}
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
					if now.Sub(s.claimedAt(t)) < s.unregisteredGrace() {
						result = "uncertain"
					}
					s.decision(t, "ownership", "unregistered", "terminal", t.Terminal, "run", t.Run, "terminal_exists", false, "active_dispatch", nil, "result", result)
					if now.Sub(s.claimedAt(t)) < s.unregisteredGrace() {
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
			if t.AutoFailed {
				action = Action{Kind: Resolve, Attempt: t.Attempt, Phase: machine.PhaseResolver, Reason: "failed executor requires agent resolution"}
			} else if !t.UpdatedAt.IsZero() && now.Sub(t.UpdatedAt) > time.Duration(r.StaleHours)*time.Hour {
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
		if action.Kind == Resume || action.Kind == Resolve {
			// A reported resolver result is applied without a new terminal.
			if memoryHeld && (action.Kind == Resume || t.Resolution == nil || t.Resolution.Phase != "result") {
				s.decision(t, "skip", "memory", "action", action.Kind)
				continue
			}
			if !takeToken() {
				s.decision(t, "skip", "token", "action", action.Kind)
				tokenWait(t)
				continue
			}
			s.lastRecoveryRepo = t.Repo
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
			if err := s.launchWithRuntimeCheck(ctx, t); err != nil {
				problems = append(problems, fmt.Errorf("resolver launch uncertain; ownership retained: %s: %s", t.Identifier, applyReason(err)))
			}
		case Resume:
			t.Attempt = action.Attempt
			t.Phase = action.Phase
			t.Owner = machine.OwnerExecutor
			t.Terminal, t.Run = "-", ""
			if err := s.launchWithRuntimeCheck(ctx, t); err != nil {
				problems = append(problems, fmt.Errorf("executor launch uncertain; ownership retained: %s: %s", t.Identifier, applyReason(err)))
			}
		}
	}
	lap("dispatch")
	// A merge correction launches an executor terminal, so a paused runtime
	// holds the merge lane too.
	if merger, ok := s.Backend.(interface {
		AdvanceMerges(context.Context, []Ticket) error
	}); ok && !s.orcaRuntime.Paused {
		counter, counted := s.Backend.(interface{ OrcaTerminalCreates() (int, int) })
		createdBefore := 0
		if counted {
			_, createdBefore = counter.OrcaTerminalCreates()
		}
		if err := merger.AdvanceMerges(ctx, s.MergeQueue); err != nil {
			problems = append(problems, err)
			for _, runtimeErr := range terminalCreateRuntimeErrors(err) {
				s.recordOrcaRuntimeFailure(ctx, runtimeErr)
			}
		}
		// A merge correction that succeeds returns no error; read the counts back.
		// A created terminal ends the earlier fault; a pause the same batch
		// reached afterwards is a new fault and keeps its own start.
		if counted {
			failures, created := counter.OrcaTerminalCreates()
			if created != createdBefore {
				paused, pausedAt := s.orcaRuntime.Paused, s.orcaRuntime.PausedAt
				s.clearOrcaRuntime()
				s.orcaRuntime.Paused, s.orcaRuntime.PausedAt = paused, pausedAt
			}
			s.orcaRuntime.Failures = failures
		}
	}
	lap("merge")
	permitted := make([]Ticket, 0, len(tickets))
	for _, t := range tickets {
		if allowed[t.Repo] {
			permitted = append(permitted, t)
		}
	}
	if reviews, ok := s.Backend.(interface {
		AdvanceCrossReviews(context.Context, []Ticket) error
	}); ok {
		// The shadow record never decides anything, so it never fails a tick.
		if err := reviews.AdvanceCrossReviews(ctx, permitted); err != nil {
			s.logger().Warn("cross review record failed", "err", err)
		}
	}
	if parks, ok := s.Backend.(interface {
		ResumeModelParks(context.Context, []Ticket) error
	}); ok {
		// A failed resume leaves the ticket parked until the next tick.
		if err := parks.ResumeModelParks(ctx, permitted); err != nil {
			s.logger().Warn("model park resume failed", "err", err)
		}
	}
	if runners, ok := s.Backend.(interface{ CheckRunners(context.Context) error }); ok {
		// A runner alert never blocks ticket flow; its failure is only logged.
		if err := runners.CheckRunners(ctx); err != nil {
			s.logger().Warn("runner health check failed", "err", err)
		}
	}
	lap("runners")
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
		if s.orcaRuntime.Paused {
			break
		}
		if !t.Candidate || t.resumesResolution() {
			continue
		}
		reason := ""
		scope := avoidedScope(t.Scope, repos[t.Repo])
		switch {
		case !allowed[t.Repo]:
			reason = "refused"
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
		case repos[t.Repo].Env["AGENT_SCOPE_AVOID"] != "0" && len(scopeMatches(scope, conflicts[t.Repo])) > 0:
			reason = "scope"
		case memoryHeld:
			reason = "memory"
		case !takeToken():
			reason = "token"
		}
		if reason != "" {
			if reason == "token" {
				tokenWait(t)
			}
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
		conflicts[t.Repo] = append(conflicts[t.Repo], scopeReservation{identifier: t.Identifier, scope: scope, source: "declared"})
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
		if err = s.launchWithRuntimeCheck(ctx, t); err != nil {
			problems = append(problems, fmt.Errorf("executor launch uncertain; ownership retained: %s: %s", t.Identifier, applyReason(err)))
		}
	}
	lap("claim")
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
	for {
		if ctx.Err() != nil {
			return nil
		}
		s.wake()
		if !s.clock().Before(s.NextTickAt) {
			if s.wakeReason == "" {
				s.wakeReason = "interval"
			}
			s.tickStarted = s.clock()
			if err = s.Tick(ctx); err != nil && report != nil && !transientSnapshotFailure(err) {
				report(err)
			}
			delay := s.NextInterval
			if delay <= 0 {
				delay = time.Minute
			}
			s.tickEnded = s.clock()
			s.NextTickAt, s.wakeReason = s.tickEnded.Add(delay), ""
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

// claimedAt is when the unregistered grace of t started.
func (s *Scheduler) claimedAt(t Ticket) time.Time {
	if at, ok := s.relaunchedAt[t.ID]; ok && at.After(t.UpdatedAt) {
		return at
	}
	return t.UpdatedAt
}

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
// infra classification wrappers hide their cause in Error(), so the reason
// appends each wrapped cause to keep the underlying failure visible.
func snapshotReason(err error) string {
	text := err.Error()
	root := err
	for next := errors.Unwrap(root); next != nil; next = errors.Unwrap(root) {
		if hidesCause(root) {
			text += ": " + next.Error()
		}
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

func hidesCause(err error) bool {
	switch err.(type) {
	case *infra.Ticket, *infra.Transient, *infra.Global:
		return true
	}
	return false
}

// A transient snapshot failure is already logged as WARN and clears on its own;
// reporting it would add an ERROR for a round the scheduler simply skips.
func transientSnapshotFailure(err error) bool {
	var snapshot *snapshotError
	var transient *infra.Transient
	return errors.As(err, &snapshot) && errors.As(snapshot.err, &transient)
}

// snapshotError text carries only the redacted reason; Unwrap keeps the cause.
type snapshotError struct {
	reason string
	err    error
}

func (e *snapshotError) Error() string { return "scheduler snapshot failed: " + e.reason }
func (e *snapshotError) Unwrap() error { return e.err }

// launchWithRuntimeCheck keeps a reservation whose launch never reached a
// terminal create answer, and clears the fault only on a real success.
func (s *Scheduler) launchWithRuntimeCheck(ctx context.Context, t Ticket) error {
	result, err := s.launchOutcome(ctx, t)
	var runtimeErr *terminalCreateRuntimeError
	var uncertain *terminalCreateUncertainError
	switch {
	case errors.As(err, &runtimeErr):
		s.recordOrcaRuntimeFailure(ctx, runtimeErr)
	case result == "success":
		if _, retried := s.runtimePending[t.ID]; retried {
			if s.relaunchedAt == nil {
				s.relaunchedAt = map[string]time.Time{}
			}
			s.relaunchedAt[t.ID] = s.clock()
		}
		delete(s.runtimePending, t.ID)
		s.clearOrcaRuntime()
	case errors.As(err, &uncertain):
		delete(s.runtimePending, t.ID)
	}
	return err
}

// recordOrcaRuntimeFailure keeps the reservation for retry and mirrors the
// Runtime's consecutive count, which also sees merge-correction launches.
func (s *Scheduler) recordOrcaRuntimeFailure(ctx context.Context, runtimeErr *terminalCreateRuntimeError) {
	t := runtimeErr.ticket
	if s.runtimePending == nil {
		s.runtimePending = map[string]Ticket{}
	}
	s.runtimePending[t.ID] = t
	s.orcaRuntime.Failures = runtimeErr.failures
	if s.orcaRuntime.Failures < orcaRuntimeFailureThreshold || s.orcaRuntime.Paused {
		return
	}
	s.orcaRuntime.Paused = true
	s.orcaRuntime.PausedAt = s.clock()
	s.orcaPauseTicket = t
	if notifier, ok := s.Backend.(interface {
		NotifyOrcaRuntime(context.Context, Ticket) error
	}); ok {
		if notifyErr := notifier.NotifyOrcaRuntime(ctx, t); notifyErr != nil {
			s.logger().Warn("Orca runtime notification failed", "error", notifyErr)
		}
	}
}

// clearOrcaRuntime ends every Orca fault once a terminal create succeeds.
func (s *Scheduler) clearOrcaRuntime() {
	s.orcaRuntime = status.OrcaRuntime{}
	s.orcaRestartsExhausted = false
	s.orcaExhaustedLogged = false
}

// observeOrcaReads records one round of Orca reads: an answered read clears
// the unreachable start, and a round Orca never answered starts it and
// restarts orca-serve after the grace.
func (s *Scheduler) observeOrcaReads(ctx context.Context, answered, unreachable bool) {
	if answered {
		s.orcaRuntime.UnreachableSince = time.Time{}
		return
	}
	if !unreachable {
		return
	}
	if s.orcaRuntime.UnreachableSince.IsZero() {
		s.orcaRuntime.UnreachableSince = s.clock()
		s.logger().Warn("Orca unreachable", "unreachable_since", s.orcaRuntime.UnreachableSince)
	}
	if s.clock().Sub(s.orcaRuntime.UnreachableSince) >= OrcaRestartGrace {
		s.restartOrcaService(ctx)
	}
}

// orcaUnreachable reports an automation read that Orca did not answer: a
// runtime_unavailable reply or a CLI timeout while the round itself was live.
func orcaUnreachable(ctx context.Context, err error) bool {
	var global *infra.Global
	if errors.As(err, &global) {
		return global.Dep == infra.Orca && global.Reason == infra.RuntimeUnavailable
	}
	return ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
}

// ErrOrcaRestartUnsupported is returned by a backend that cannot restart
// orca-serve; the scheduler then neither counts a restart nor notifies.
var ErrOrcaRestartUnsupported = errors.New("backend has no Orca service restart")

// restartOrcaService shares the grace, interval and attempt limit across
// terminal-create failures and unreachable automation reads.
func (s *Scheduler) restartOrcaService(ctx context.Context) {
	now := s.clock()
	ticket := s.orcaPauseTicket
	since := s.orcaRuntime.PausedAt
	if !s.orcaRuntime.UnreachableSince.IsZero() {
		ticket = Ticket{}
		// The repository may have retired or left since that inspection.
		if !s.retired[s.orcaNotificationRepo] && slices.ContainsFunc(s.Repositories, func(r config.Repository) bool { return r.Path == s.orcaNotificationRepo }) {
			ticket.Repo = s.orcaNotificationRepo
		}
		since = s.orcaRuntime.UnreachableSince
	}
	if s.orcaRuntime.Restarts >= orcaRestartLimit {
		if s.orcaRestartsExhausted {
			return
		}
		if !s.orcaRuntime.UnreachableSince.IsZero() && ticket.Repo == "" {
			if !s.orcaExhaustedLogged {
				s.orcaExhaustedLogged = true
				s.logger().Warn("Orca restart limit reached; manual intervention required, no repository to notify yet",
					"restarts", s.orcaRuntime.Restarts, "unreachable_since", s.orcaRuntime.UnreachableSince)
			}
			return
		}
		if notifier, ok := s.Backend.(interface {
			NotifyOrcaRestartExhausted(context.Context, Ticket) error
		}); ok {
			if err := notifier.NotifyOrcaRestartExhausted(ctx, ticket); err != nil {
				s.logger().Warn("Orca restart exhausted notification failed", "error", err)
				// Feishu has no idempotency key, so only a notice that never reached
				// it or that it rejected (a business code, or a refused token or
				// permission) is sent again; a lost or unreadable response may mean
				// it was delivered.
				var rejected *feishu.APIError
				var refused *infra.Global
				if errors.Is(err, errNoticeNotSent) || errors.As(err, &rejected) || errors.As(err, &refused) && refused.Dep == feishu.Dep {
					return
				}
			}
		}
		s.orcaRestartsExhausted = true
		return
	}
	if now.Sub(since) < OrcaRestartGrace {
		return
	}
	if !s.orcaRuntime.LastRestartAt.IsZero() && now.Sub(s.orcaRuntime.LastRestartAt) < orcaRestartInterval {
		return
	}
	restarter, ok := s.Backend.(interface {
		RestartOrcaService(context.Context) error
	})
	if !ok {
		return
	}
	restartErr := restarter.RestartOrcaService(ctx)
	if errors.Is(restartErr, ErrOrcaRestartUnsupported) {
		return
	}
	if errors.Is(restartErr, errOrcaRestartRefused) {
		s.logger().Warn("Orca service restart refused by maintenance", "error", restartErr)
		return
	}
	s.orcaRuntime.Restarts++
	s.orcaRuntime.LastRestartAt = now
	s.logger().Warn("Orca service restarted after runtime fault", "restart", s.orcaRuntime.Restarts,
		"limit", orcaRestartLimit, "exit_code", restartExitCode(restartErr), "paused_at", s.orcaRuntime.PausedAt, "unreachable_since", s.orcaRuntime.UnreachableSince, "error", restartErr)
	if !s.orcaRuntime.UnreachableSince.IsZero() && ticket.Repo == "" {
		return
	}
	if notifier, ok := s.Backend.(interface {
		NotifyOrcaRestart(context.Context, Ticket, int, error) error
	}); ok {
		if err := notifier.NotifyOrcaRestart(ctx, ticket, s.orcaRuntime.Restarts, restartErr); err != nil {
			s.logger().Warn("Orca restart notification failed", "error", err)
		}
	}
}

const hostMemoryNotifyAfter = 15 * time.Minute

// checkHostMemory applies the host and all readable cgroup limits each round.
func (s *Scheduler) checkHostMemory(ctx context.Context, now time.Time) {
	if s.HostMemory == nil {
		return
	}
	reading, err := s.HostMemory()
	if err != nil && reading.Source == "" {
		if s.hostMemory.Error == "" {
			s.logger().Warn("host memory unreadable; launches are not held", "err", err)
		}
		s.hostMemory = status.HostMemory{Reading: s.hostMemory.Reading, Error: err.Error()}
		return
	}
	if s.hostMemory.Error != "" {
		s.logger().Warn("host memory readable again", "available", reading.Available, "some_avg60", reading.SomeAvg60)
	}
	var cgroupError string
	if err != nil {
		cgroupError = err.Error()
		if s.hostMemory.CgroupError == "" {
			s.logger().Warn("cgroup memory read failed, using readable levels", "source", reading.Source, "err", err)
		}
	} else if s.hostMemory.CgroupError != "" {
		s.logger().Warn("cgroup memory readable again")
	}
	held := hostmem.Decide(s.memoryHeld, reading)
	switch {
	case !held:
		s.memoryHeldSince = time.Time{}
	case !s.memoryHeld:
		s.memoryHeldSince = now
	}
	s.memoryHeld = held
	if held && !s.hostMemory.Paused {
		s.memoryPausedSince = now
		s.memoryNotified = false
	}
	s.hostMemory = status.HostMemory{Paused: held, Since: s.memoryHeldSince, Reading: reading, CgroupError: cgroupError}
	if held && !s.memoryNotified && now.Sub(s.memoryPausedSince) >= hostMemoryNotifyAfter {
		if notifier, ok := s.Backend.(interface {
			NotifyHostMemory(context.Context, status.HostMemory) error
		}); ok {
			notice := s.hostMemory
			notice.Since = s.memoryPausedSince
			if err := notifier.NotifyHostMemory(ctx, notice); err != nil {
				s.logger().Warn("host memory notification failed", "err", err)
			} else {
				s.memoryNotified = true
			}
		}
	}
}

// HostMemoryStatus is the host memory hold serve publishes in its status.
func (s *Scheduler) HostMemoryStatus() status.HostMemory { return s.hostMemory }

// OrcaRuntime is the Orca fault state serve publishes in its status.
func (s *Scheduler) OrcaRuntime() status.OrcaRuntime { return s.orcaRuntime }

// terminalCreateRuntimeErrors finds every rejection in a joined merge-lane error.
func terminalCreateRuntimeErrors(err error) []*terminalCreateRuntimeError {
	switch e := err.(type) {
	case *terminalCreateRuntimeError:
		return []*terminalCreateRuntimeError{e}
	case interface{ Unwrap() []error }:
		var found []*terminalCreateRuntimeError
		for _, inner := range e.Unwrap() {
			found = append(found, terminalCreateRuntimeErrors(inner)...)
		}
		return found
	case interface{ Unwrap() error }:
		return terminalCreateRuntimeErrors(e.Unwrap())
	}
	return nil
}
