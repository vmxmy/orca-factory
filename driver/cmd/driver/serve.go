package main

import (
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
	"github.com/vmxmy/orca-factory/driver/internal/machine"
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
	err = serveWithStatus(ctx, filepath.Join(home, ".local", "state", "orca-factory", "driver-serve.lock"), func(ctx context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
		cfg, err := config.Load(home)
		if err != nil {
			return nil, nil, err
		}
		runtime, err := scheduler.NewRuntime(ctx, cfg, nil)
		if err != nil {
			return nil, nil, err
		}
		s := &scheduler.Scheduler{Config: cfg, Backend: runtime, Discover: projectSource(cfg), DiscoverEvery: projectDiscoveryInterval}
		if cfg.FactoryRoot != "" {
			cacheDir, err := os.UserCacheDir()
			if err != nil {
				return nil, nil, err
			}
			s.SyncCore = coreSyncer{factoryRoot: cfg.FactoryRoot, cacheDir: cacheDir}.sync
		}
		// Without a first project set serve has nothing to schedule; exit so
		// the service manager restarts it instead of idling on an empty set.
		if err := s.RefreshProjects(ctx); err != nil {
			return nil, nil, fmt.Errorf("initial project discovery failed: %w", err)
		}
		served = runtime
		return s, runtime, nil
	}, func(ctx context.Context) error {
		if now := time.Now(); served != nil && !now.Before(nextReclaim) {
			nextReclaim = now.Add(workspaceReclaimInterval)
			if _, err := served.ReclaimWorkspaces(ctx, false, logger); err != nil {
				logger.Warn("workspace reclaim incomplete", "err", err)
			}
		}
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}, func(err error) { logger.Error("scheduler error", "err", err); fmt.Fprintln(os.Stderr, err) })
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// projectDiscoveryInterval spaces serve's project rediscovery: each run reads
// Orca, the Feishu project table and every candidate's pipeline check.
const projectDiscoveryInterval = 10 * time.Minute

// workspaceReclaimInterval spaces serve's workspace reclaim (OF-96): each run
// lists the Orca workspaces of every tracked repository.
const workspaceReclaimInterval = 10 * time.Minute

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

func (b *statusBackend) Snapshot(ctx context.Context, repos []config.Repository) ([]scheduler.Ticket, error) {
	tickets, err := b.Backend.Snapshot(ctx, repos)
	if err == nil {
		b.tickets = append([]scheduler.Ticket(nil), tickets...)
	}
	return tickets, err
}

func (b *statusBackend) GitHubLimits() []github.Usage {
	// Tick checks quota even when a global pause prevents Snapshot. Wait-only
	// iterations never call this method, so they must not refresh generatedAt.
	b.inspected = true
	if source, ok := b.Backend.(interface{ GitHubLimits() []github.Usage }); ok {
		return source.GitHubLimits()
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
			if err := status.Write(collectStatus(s, backend, runtime, now)); err != nil {
				fmt.Fprintln(os.Stderr, "cannot write driver status snapshot")
			}
		}
		return wait(ctx)
	}, report)
}

func collectStatus(s *scheduler.Scheduler, backend *statusBackend, runtime *scheduler.Runtime, now time.Time) status.Snapshot {
	snapshot := status.Snapshot{GeneratedAt: now, Tickets: []status.Ticket{}, Pauses: []status.Pause{}}
	for _, ticket := range backend.tickets {
		if ticket.Owner == machine.OwnerExecutor || ticket.Owner == machine.OwnerMerge || scheduler.DeploymentBarrier(ticket) {
			row := scheduler.ObservationRow(ticket, s.Repositories)
			if p := ticket.Progress; p != nil && p.Deployment != nil {
				row.Deployment = &status.Deployment{QueueID: p.QueueID, SHA: p.MergeSHA, Workflow: p.Deployment.Workflow, State: p.Deployment.State, Reason: p.Deployment.Reason}
			}
			snapshot.Tickets = append(snapshot.Tickets, row)
		}
	}
	sort.Slice(snapshot.Tickets, func(i, j int) bool {
		a, b := snapshot.Tickets[i], snapshot.Tickets[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Identifier < b.Identifier
	})
	if s.Pauses != nil {
		for _, episode := range s.Pauses.Active() {
			snapshot.Pauses = append(snapshot.Pauses, status.Pause{Dependency: string(episode.Key.Dependency), Repo: episode.Key.Repo, Kind: string(episode.Key.Kind), Start: episode.Start, Duration: max(0, now.Sub(episode.Start))})
		}
	}
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
