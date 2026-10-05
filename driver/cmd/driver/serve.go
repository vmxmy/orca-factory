package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func init() { Register("serve", serve) }
func serve(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "Usage: driver serve")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot resolve home directory")
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err = serveWithStatus(ctx, filepath.Join(home, ".local", "state", "orca-factory", "driver-serve.lock"), func(ctx context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
		cfg, err := config.Load(home)
		if err != nil {
			return nil, nil, err
		}
		runtime, err := scheduler.NewRuntime(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		return &scheduler.Scheduler{Config: cfg, Backend: runtime}, runtime, nil
	}, func(ctx context.Context) error {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}, func(err error) { fmt.Fprintln(os.Stderr, err) })
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
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

func (b *statusBackend) LinearLimits() []linear.RateLimit {
	// Tick checks quota even when a global pause prevents Snapshot. Wait-only
	// iterations never call this method, so they must not refresh generatedAt.
	b.inspected = true
	if source, ok := b.Backend.(interface{ LinearLimits() []linear.RateLimit }); ok {
		return source.LinearLimits()
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
	snapshot := status.Snapshot{GeneratedAt: now, Tickets: []status.Ticket{}, Pauses: []status.Pause{}, Linear: status.Usage{Hour: now.UTC().Truncate(time.Hour), Limits: []status.ClientLimits{}}}
	for _, ticket := range backend.tickets {
		if ticket.Owner == machine.OwnerExecutor || ticket.Owner == machine.OwnerMerge {
			snapshot.Tickets = append(snapshot.Tickets, status.Ticket{Repo: ticket.Repo, Identifier: ticket.Identifier, Owner: string(ticket.Owner), Phase: string(ticket.Phase)})
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
		// Runtime shares a client between repositories in the same Linear team.
		seen := map[scheduler.LinearClient]bool{}
		for _, repo := range repos {
			client := runtime.Clients[repo]
			source, ok := client.(interface{ Usage() linear.Usage })
			if !ok {
				continue
			}
			usage := source.Usage()
			if !seen[client] {
				snapshot.Linear.Requests += usage.Requests
				snapshot.Linear.Complexity += usage.Complexity
				seen[client] = true
			}
			snapshot.Linear.Limits = append(snapshot.Linear.Limits, status.ClientLimits{Repo: repo, Requests: statusLimit(usage.RequestLimit), Complexity: statusLimit(usage.ComplexityLimit)})
		}
	}
	return snapshot
}

func statusLimit(limit linear.RateLimit) status.RateLimit {
	return status.RateLimit{Limit: limit.Limit, Remaining: limit.Remaining, Reset: limit.Reset}
}
