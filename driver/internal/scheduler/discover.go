package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
)

// projectTracker is the optional Backend capability that prepares a newly
// discovered repository (its ledger client) before the scheduler reads it.
type projectTracker interface {
	Track(context.Context, config.Repository) error
}

// repositoryUpdater is the optional Backend capability that keeps its own
// copy of an already-tracked repository's configuration in sync once
// RefreshProjects confirms a hot reload is safe (OF-45): the ledger identity
// keys are unchanged, so no bound GitHub/Feishu client needs rebinding.
type repositoryUpdater interface {
	UpdateRepository(config.Repository)
}

// RefreshProjects re-runs Discover when it is due and replaces the claimable
// set (OF-43). A repository that leaves the eligible set is retired: Tick keeps
// reading it so its in-flight tickets, merges, deployment barriers and
// resolvers finish, but claims none of its new tickets. A failed discovery
// keeps the previous set and is returned.
func (s *Scheduler) RefreshProjects(ctx context.Context) error {
	err := s.refreshProjects(ctx)
	if err != nil {
		s.logger().Warn("project discovery failed; keeping previous projects", "err", err)
	}
	return err
}

// The driver can boot before Orca's runtime is ready (OF-137), so serve's
// first discovery waits out dependency outages instead of exiting into a
// systemd restart loop. A long wait repeats its WARN only every
// outageWarnEvery so the log stays bounded.
const (
	initialDiscoveryBackoff    = 5 * time.Second
	maxInitialDiscoveryBackoff = time.Minute
	outageWarnEvery            = 10 * time.Minute
)

type outage struct {
	dep    infra.Dep
	reason infra.Reason
}

// AwaitProjects runs serve's first discovery. A temporary outage (Orca
// runtime_unavailable, or an infra.Transient timeout or network failure) is
// retried with doubling backoff until discovery succeeds or sleep reports ctx
// ended. Credential, authentication, permission and missing-executable
// failures, and any other error, are returned at once. It claims and launches
// nothing.
func (s *Scheduler) AwaitProjects(ctx context.Context, sleep func(context.Context, time.Duration) error) error {
	warned := map[outage]time.Time{}
	for delay := initialDiscoveryBackoff; ; delay = min(2*delay, maxInitialDiscoveryBackoff) {
		err := s.refreshProjects(ctx)
		if err == nil {
			return nil
		}
		cause, retry := classifyOutage(err)
		if !retry {
			if cause.dep != "" {
				return fmt.Errorf("%s %s: %w", cause.dep, cause.reason, err)
			}
			return err
		}
		s.nextDiscover = time.Time{}
		now := s.clock()
		level := slog.LevelDebug
		if last, seen := warned[cause]; !seen || now.Sub(last) >= outageWarnEvery {
			warned[cause] = now
			level = slog.LevelWarn
		}
		s.logger().Log(ctx, level, "project discovery waiting for dependency; scheduling not started",
			"dependency", cause.dep, "reason", cause.reason, "retryIn", delay)
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func classifyOutage(err error) (outage, bool) {
	var global *infra.Global
	if errors.As(err, &global) {
		return outage{dep: global.Dep, reason: global.Reason}, global.Reason == infra.RuntimeUnavailable
	}
	var transient *infra.Transient
	if errors.As(err, &transient) {
		return outage{dep: "unknown", reason: "transient"}, true
	}
	return outage{}, false
}

func (s *Scheduler) refreshProjects(ctx context.Context) error {
	now := s.clock()
	if s.Discover == nil || now.Before(s.nextDiscover) {
		return nil
	}
	s.nextDiscover = now.Add(s.DiscoverEvery)
	result, err := s.Discover(ctx)
	if err != nil {
		return err
	}
	rejected := map[string]string{}
	for _, r := range result.Rejected {
		rejected[r.Path] = string(r.Reason)
	}
	tracker, _ := s.Backend.(projectTracker)
	updater, _ := s.Backend.(repositoryUpdater)
	eligible := map[string]bool{}
	repositories := slices.Clone(s.Repositories)
	for _, p := range result.Eligible {
		if idx := slices.IndexFunc(repositories, func(r config.Repository) bool { return r.Path == p.Path }); idx >= 0 {
			eligible[p.Path] = true
			repositories[idx] = s.reconcileRepository(repositories[idx], p.Repository, updater)
			continue
		}
		if tracker != nil {
			if err := tracker.Track(ctx, p.Repository); err != nil {
				rejected[p.Path] = err.Error()
				continue
			}
		}
		eligible[p.Path] = true
		repositories = append(repositories, p.Repository)
	}
	s.syncCore(ctx, result.Eligible, eligible)
	for _, r := range result.Rejected {
		if r.Repository == nil || slices.ContainsFunc(repositories, func(known config.Repository) bool { return known.Path == r.Path }) {
			continue
		}
		held, err := s.heldTickets(ctx, *r.Repository, tracker)
		if err != nil {
			rejected[r.Path] = string(r.Reason) + "; in-flight probe failed: " + err.Error()
			continue
		}
		if len(held) > 0 {
			repositories = append(repositories, *r.Repository)
			s.logger().Warn("project not taken over but has in-flight tickets", "path", r.Path, "reason", string(r.Reason), "tickets", held)
		}
	}
	retired := map[string]bool{}
	for _, r := range repositories {
		if !eligible[r.Path] {
			retired[r.Path] = true
		}
	}
	s.Repositories, s.retired = repositories, retired
	s.logRejections(rejected)
	paths := make([]string, 0, len(eligible))
	for path := range eligible {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	s.logger().Info("projects discovered", "eligible", paths, "retired", s.retiredPaths())
	return nil
}

// heldTickets reads a not-takeable repository's ledger and returns the
// identifiers of the tickets the driver still owns there (OF-98): a
// repository that is already not takeable when serve starts never left the
// eligible set, so without this its in-flight work would have no reader.
func (s *Scheduler) heldTickets(ctx context.Context, repo config.Repository, tracker projectTracker) ([]string, error) {
	if tracker != nil {
		if err := tracker.Track(ctx, repo); err != nil {
			return nil, err
		}
	}
	tickets, err := s.Backend.Snapshot(ctx, []config.Repository{repo})
	if err != nil {
		return nil, err
	}
	var held []string
	for _, t := range tickets {
		if owned(t) || DeploymentBarrier(t) {
			held = append(held, t.Identifier)
		}
	}
	return held, nil
}

// reconcileRepository replaces current with fresh whenever fresh differs and
// keeps the ledger identity keys (config.LedgerIdentityKeys) unchanged: every
// reader (scheduler.go's MaxInFlight/StaleHours/AGENT_SCOPE_AVOID checks, and
// the Backend's own repositoryUpdater copy) sees the replacement on this same
// tick. A changed identity key instead keeps current entirely until serve
// restarts, so a bound GitHub/Feishu client is never rebound live.
func (s *Scheduler) reconcileRepository(current, fresh config.Repository, updater repositoryUpdater) config.Repository {
	changed := config.ChangedEnvKeys(current.Env, fresh.Env)
	if current.MaxInFlight != fresh.MaxInFlight {
		changed = append(changed, "MaxInFlight")
	}
	if current.StaleHours != fresh.StaleHours {
		changed = append(changed, "StaleHours")
	}
	if len(changed) == 0 {
		return current
	}
	sort.Strings(changed)
	if slices.ContainsFunc(changed, func(key string) bool { return slices.Contains(config.LedgerIdentityKeys, key) }) {
		s.warnIdentityDrift(current.Path, changed)
		return current
	}
	if updater != nil {
		updater.UpdateRepository(fresh)
	}
	s.logger().Info("repository configuration updated", "path", current.Path, "changed", changed)
	return fresh
}

// warnIdentityDrift logs once per distinct ledger-identity change a
// repository carries; an unchanged drift is not re-logged on every discovery.
func (s *Scheduler) warnIdentityDrift(path string, changed []string) {
	fingerprint := strings.Join(changed, ",")
	if s.identityDrift == nil {
		s.identityDrift = map[string]string{}
	}
	if s.identityDrift[path] == fingerprint {
		return
	}
	s.identityDrift[path] = fingerprint
	s.logger().Warn("ledger identity changed; restart required", "path", path, "changed", changed)
}

// syncCore warns once per distinct set of lagging core copies among the
// taken-over projects, and hands each lagging one to SyncCore. A failed sync
// only warns: the project stays taken over and the next discovery retries.
func (s *Scheduler) syncCore(ctx context.Context, found []projects.Project, eligible map[string]bool) {
	var lines []string
	var lagging []projects.Project
	for _, p := range found {
		if eligible[p.Path] && len(p.CoreBehind) > 0 {
			lines = append(lines, p.Path+"\x00"+strings.Join(p.CoreBehind, "\x00"))
			lagging = append(lagging, p)
		}
	}
	sort.Strings(lines)
	if fingerprint := strings.Join(lines, "\n"); fingerprint != s.coreBehind {
		s.coreBehind = fingerprint
		for _, p := range lagging {
			s.logger().Warn("project core behind", "path", p.Path, "files", p.CoreBehind)
		}
	}
	if s.SyncCore == nil {
		return
	}
	for _, p := range lagging {
		if err := s.SyncCore(ctx, p); err != nil {
			s.logger().Warn("core sync failed", "path", p.Path, "err", err)
		}
	}
}

// logRejections warns once per repository whenever the rejected set changes.
func (s *Scheduler) logRejections(rejected map[string]string) {
	lines := make([]string, 0, len(rejected))
	for path, reason := range rejected {
		lines = append(lines, path+"\x00"+reason)
	}
	sort.Strings(lines)
	fingerprint := strings.Join(lines, "\n")
	if fingerprint == s.rejections {
		return
	}
	s.rejections = fingerprint
	for _, line := range lines {
		path, reason, _ := strings.Cut(line, "\x00")
		s.logger().Warn("project not taken over", "path", path, "reason", reason)
	}
}

func (s *Scheduler) retiredPaths() []string {
	paths := make([]string, 0, len(s.retired))
	for path := range s.retired {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// dropIdleRetired stops reading a retired repository once the snapshot shows
// it holds no ticket the scheduler or a human still owns.
func (s *Scheduler) dropIdleRetired(tickets []Ticket) {
	if len(s.retired) == 0 {
		return
	}
	busy := map[string]bool{}
	for _, t := range tickets {
		if owned(t) || DeploymentBarrier(t) || t.Owner == machine.OwnerHuman {
			busy[t.Repo] = true
		}
	}
	repositories := make([]config.Repository, 0, len(s.Repositories))
	retired := map[string]bool{}
	for _, r := range s.Repositories {
		if s.retired[r.Path] && !busy[r.Path] {
			continue
		}
		repositories = append(repositories, r)
		if s.retired[r.Path] {
			retired[r.Path] = true
		}
	}
	s.Repositories, s.retired = repositories, retired
}
