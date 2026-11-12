package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func init() { Register("serve", serve) }
func serve(args []string) int {
	if len(args) == 1 && args[0] == "--dry-run" {
		code, err := serveLogged(args, slog.Default())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return code
	}
	return loggedCommand("scheduler", filepath.Join(logStateRoot(), "scheduler.log"), os.Stdout, args, func(logger *slog.Logger) (int, error) {
		return serveLogged(args, logger)
	})
}
func serveLogged(args []string, logger *slog.Logger) (int, error) {
	dry := len(args) == 1 && args[0] == "--dry-run"
	if len(args) != 0 && !dry {
		return 2, fmt.Errorf("usage: driver serve [--dry-run]")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 1, fmt.Errorf("cannot resolve home directory")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if dry {
		if err := serveDryRun(ctx, home, os.Stdout); err != nil {
			return 1, err
		}
		return 0, nil
	}
	ctx = runx.WithLogger(ctx, logger)
	var served *scheduler.Runtime
	var nextReclaim time.Time
	var wait func(context.Context, time.Time) error
	err = serveWithStatus(ctx, filepath.Join(home, ".local", "state", "orca-factory", "driver-serve.lock"), func(ctx context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
		cfg, err := config.Load(home)
		if err != nil {
			return nil, nil, err
		}
		runtime, err := scheduler.NewRuntime(ctx, cfg, nil)
		if err != nil {
			return nil, nil, err
		}
		runtime.Version = version
		s := &scheduler.Scheduler{Config: cfg, Backend: runtime, Discover: projectSource(cfg), DiscoverEvery: projectDiscoveryInterval}
		if cfg.FactoryRoot != "" {
			cacheDir, err := os.UserCacheDir()
			if err != nil {
				return nil, nil, err
			}
			s.SyncCore = (&coreSyncer{factoryRoot: cfg.FactoryRoot, cacheDir: cacheDir}).sync
		}
		if err := s.AwaitProjects(ctx, sleepContext); err != nil {
			return nil, nil, fmt.Errorf("initial project discovery failed: %w", err)
		}
		served = runtime
		wait = newServeWait(s, runtime.StateRoot)
		return s, runtime, nil
	}, func(ctx context.Context) error {
		if now := time.Now(); served != nil && !now.Before(nextReclaim) {
			nextReclaim = now.Add(workspaceReclaimInterval)
			if _, err := served.ReclaimWorkspaces(ctx, false, logger); err != nil {
				logger.Warn("workspace reclaim incomplete", "err", err)
			}
		}
		return wait(ctx, nextReclaim)
	}, func(err error) { logger.Error("scheduler error", "err", err); fmt.Fprintln(os.Stderr, err) })
	if served != nil {
		if closeErr := served.Close(mergeGateDrain); closeErr != nil {
			logger.Warn("merge gates not stopped", "err", closeErr)
		}
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// mergeGateDrain bounds the wait for killed merged-result gates to remove
// their worktrees; systemd stops the unit after 90 seconds.
const mergeGateDrain = 75 * time.Second

// projectDiscoveryInterval spaces serve's project rediscovery: each run reads
// Orca, the Feishu project table and every candidate's pipeline check.
const projectDiscoveryInterval = 10 * time.Minute

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// workspaceReclaimInterval spaces serve's workspace reclaim (OF-96): each run
// lists the Orca workspaces of every tracked repository.
const workspaceReclaimInterval = 10 * time.Minute

func newServeWait(s *scheduler.Scheduler, stateRoot string) func(context.Context, time.Time) error {
	wakePath := filepath.Join(stateRoot, "scheduler-wake.json")
	lastWake, _ := os.ReadFile(wakePath)
	return func(ctx context.Context, nextReclaim time.Time) error {
		deadline := s.NextTickAt
		if nextReclaim.Before(deadline) {
			deadline = nextReclaim
		}
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		poll := time.NewTicker(time.Second)
		defer poll.Stop()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Scheduler owns parsing and read errors; this only detects new requests.
			if raw, err := os.ReadFile(wakePath); err == nil && !bytes.Equal(raw, lastWake) {
				lastWake = raw
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			case <-poll.C:
			}
		}
	}
}

func projectSource(cfg config.Config) func(context.Context) (projects.Result, error) {
	client := &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout}
	warned := map[string]string{}
	return func(ctx context.Context) (projects.Result, error) {
		result, _, err := discoverProjects(ctx, cfg, client)
		if err == nil {
			warnRemoteMismatch(ctx, result.Eligible, warned)
			for _, p := range result.Eligible {
				if ffErr := fastForwardCheckout(ctx, p.Path); ffErr != nil {
					runx.LoggerFrom(ctx).Warn("checkout not fast-forwarded", "path", p.Path, "err", ffErr)
				}
			}
		}
		return result, err
	}
}

// warnRemoteMismatch logs once per distinct live-remote drift an eligible
// project carries (OF-56): projects.Discover already marks RemoteMismatch on
// the Project itself for `projects --json`; this only adds a log line, and
// only the first time a given drift is seen for a path, so a long-running
// serve does not repeat the same warning every discovery interval. warned
// persists in projectSource's closure across calls; it never changes which
// projects are eligible.
func warnRemoteMismatch(ctx context.Context, eligible []projects.Project, warned map[string]string) {
	logger := runx.LoggerFrom(ctx)
	for _, p := range eligible {
		if p.RemoteMismatch == "" {
			delete(warned, p.Path)
			continue
		}
		if warned[p.Path] == p.RemoteMismatch {
			continue
		}
		warned[p.Path] = p.RemoteMismatch
		logger.Warn("live remote drifted from Orca identity", "path", p.Path, "canonicalKey", p.Remote, "liveRemote", p.RemoteMismatch)
	}
}

// serveDryRun discovers projects once, prints them, and runs one write-free tick.
func serveDryRun(ctx context.Context, home string, out io.Writer) error {
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	discovered, err := projectSource(cfg)(ctx)
	if err != nil {
		return err
	}
	for _, p := range discovered.Eligible {
		if _, err := fmt.Fprintf(out, "project eligible %s\n", p.Path); err != nil {
			return err
		}
	}
	for _, r := range discovered.Rejected {
		if _, err := fmt.Fprintf(out, "project rejected %s reason=%q\n", r.Path, r.Reason); err != nil {
			return err
		}
	}
	repos := eligibleRepositories(discovered)
	runtime, err := scheduler.NewRuntime(ctx, cfg, repos)
	if err != nil {
		return err
	}
	return dryRunTick(ctx, cfg, repos, runtime, out, time.Now)
}

// statusBackend observes existing reads without issuing additional requests.
// Preserve optional scheduler capabilities as well as the required Backend.
type statusBackend struct {
	scheduler.Backend
	tickets   []scheduler.Ticket
	inspected bool
}

// Snapshot marks the inspection even when it fails, so every Tick past the
// configuration check writes one status snapshot and wait-only iterations none.
func (b *statusBackend) Snapshot(ctx context.Context, repos []config.Repository) ([]scheduler.Ticket, error) {
	b.inspected = true
	tickets, err := b.Backend.Snapshot(ctx, repos)
	if err == nil {
		b.tickets = append([]scheduler.Ticket(nil), tickets...)
	}
	return tickets, err
}

func (b *statusBackend) TodayOutput(now time.Time) *status.TodayOutput {
	if source, ok := b.Backend.(interface {
		TodayOutput(time.Time) *status.TodayOutput
	}); ok {
		return source.TodayOutput(now)
	}
	return nil
}

func (b *statusBackend) ActualChanges(ctx context.Context, ticket scheduler.Ticket) ([]string, bool, error) {
	if source, ok := b.Backend.(interface {
		ActualChanges(context.Context, scheduler.Ticket) ([]string, bool, error)
	}); ok {
		return source.ActualChanges(ctx, ticket)
	}
	return nil, false, nil
}

func (b *statusBackend) NotifyFeishu(ctx context.Context, ticket scheduler.Ticket) error {
	if source, ok := b.Backend.(interface {
		NotifyFeishu(context.Context, scheduler.Ticket) error
	}); ok {
		return source.NotifyFeishu(ctx, ticket)
	}
	return nil
}

// Liveness and ReapDead keep the Runtime's heartbeat evidence visible through
// the wrapper; a backend without it leaves terminal and dispatch evidence.
func (b *statusBackend) Liveness(ctx context.Context, ticket scheduler.Ticket) (orca.Liveness, error) {
	if source, ok := b.Backend.(scheduler.LivenessBackend); ok {
		return source.Liveness(ctx, ticket)
	}
	return orca.LivenessNone, nil
}

func (b *statusBackend) ReapDead(ctx context.Context, ticket scheduler.Ticket) error {
	if source, ok := b.Backend.(scheduler.LivenessBackend); ok {
		return source.ReapDead(ctx, ticket)
	}
	return errors.New("backend has no heartbeat evidence")
}

func serveWithStatus(ctx context.Context, lockPath string, initialize func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error), wait func(context.Context) error, report func(error)) error {
	var s *scheduler.Scheduler
	var runtime *scheduler.Runtime
	var backend *statusBackend
	return scheduler.Serve(ctx, lockPath, func(ctx context.Context) (*scheduler.Scheduler, error) {
		var err error
		s, runtime, err = initialize(ctx)
		if err != nil {
			return nil, err
		}
		backend = &statusBackend{Backend: s.Backend}
		s.Backend = backend
		return s, nil
	}, func(ctx context.Context) error {
		if backend.inspected {
			backend.inspected = false
			now := time.Now()
			if s.Now != nil {
				now = s.Now()
			}
			snapshot := collectStatus(s, backend, runtime, now)
			snapshot.NextScanAt = s.NextTickAt
			if err := status.Write(snapshot); err != nil {
				fmt.Fprintln(os.Stderr, "cannot write driver status snapshot")
			}
			status.Push(now, func(view *status.View) {
				view.GitHub = snapshot.GitHub
				view.NextScanAt = snapshot.NextScanAt
			})
		}
		return wait(ctx)
	}, report)
}

func collectStatus(s *scheduler.Scheduler, backend *statusBackend, runtime *scheduler.Runtime, now time.Time) status.Snapshot {
	snapshot := status.Snapshot{GeneratedAt: now, Tickets: []status.Ticket{}}
	snapshot.Tickets = scheduler.SnapshotRows(backend.tickets, s.Repositories, now)
	snapshot.Today = backend.TodayOutput(now)
	if runtime != nil {
		repos := make([]string, 0, len(runtime.Clients))
		for repo := range runtime.Clients {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		seen := map[issues.Store]bool{}
		for _, repo := range repos {
			client := runtime.Clients[repo]
			if source, ok := client.(interface{ Usage() github.Usage }); ok {
				usage := source.Usage()
				if !seen[client] {
					snapshot.GitHub.Requests += usage.Requests
					seen[client] = true
				}
				snapshot.GitHub.Hour = usage.Hour
				snapshot.GitHub.Limits = append(snapshot.GitHub.Limits, status.ClientLimits{Repo: repo, Requests: status.RateLimit{Limit: usage.Limit, Remaining: usage.Remaining, Reset: usage.Reset}})
			}
		}
	}
	return snapshot
}

func (b *statusBackend) AdvanceMerges(ctx context.Context, tickets []scheduler.Ticket) error {
	if source, ok := b.Backend.(interface {
		AdvanceMerges(context.Context, []scheduler.Ticket) error
	}); ok {
		return source.AdvanceMerges(ctx, tickets)
	}
	return nil
}

func (b *statusBackend) AdvanceCrossReviews(ctx context.Context, tickets []scheduler.Ticket) error {
	if source, ok := b.Backend.(interface {
		AdvanceCrossReviews(context.Context, []scheduler.Ticket) error
	}); ok {
		return source.AdvanceCrossReviews(ctx, tickets)
	}
	return nil
}

func (b *statusBackend) UpdateRepository(repo config.Repository) {
	if source, ok := b.Backend.(interface{ UpdateRepository(config.Repository) }); ok {
		source.UpdateRepository(repo)
	}
}

func (b *statusBackend) Track(ctx context.Context, repo config.Repository) error {
	if source, ok := b.Backend.(interface {
		Track(context.Context, config.Repository) error
	}); ok {
		return source.Track(ctx, repo)
	}
	return nil
}

func (b *statusBackend) CheckRunners(ctx context.Context) error {
	if source, ok := b.Backend.(interface{ CheckRunners(context.Context) error }); ok {
		return source.CheckRunners(ctx)
	}
	return nil
}
