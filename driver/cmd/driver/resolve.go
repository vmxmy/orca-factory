package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

func init() { Register("resolve", resolveTicket) }
func resolveTicket(args []string) int {
	if len(args) != 1 || !logComponent.MatchString(args[0]) {
		fmt.Fprintln(os.Stderr, "usage: driver resolve <ticket>")
		return 2
	}
	return loggedCommand("resolver", filepath.Join(logStateRoot(), executorLogRepository(), args[0], "resolver.log"), os.Stderr, args, func(logger *slog.Logger) (int, error) {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		home, err := os.UserHomeDir()
		if err != nil {
			return 1, err
		}
		cfg, err := config.Load(home)
		if err != nil {
			return 1, err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return 1, err
		}
		var repos []config.Repository
		for _, repo := range cfg.Repositories {
			if repo.Path == cwd {
				repos = append(repos, repo)
			}
		}
		if len(repos) != 1 {
			return 1, errors.New("resolver must run at a configured repository root")
		}
		cfg.Repositories = repos
		// Local exclusion is only a controller lock. Ledger receipts own all state.
		lock, err := scheduler.Acquire(filepath.Join(logStateRoot(), executorLogRepository(), args[0], "resolver.lock"))
		if err != nil {
			return 1, err
		}
		defer func() {
			if err := lock.Close(); err != nil {
				logger.Error("resolver lock close", "err", err)
			}
		}()
		runtime, err := scheduler.NewRuntime(ctx, cfg)
		if err != nil {
			return 1, err
		}
		tickets, err := runtime.Snapshot(ctx, repos)
		if err != nil {
			return 1, err
		}
		for _, ticket := range tickets {
			if ticket.Identifier == args[0] {
				err = runtime.ResolveTicket(ctx, ticket, os.Getenv("ORCA_TERMINAL_HANDLE"))
				if err != nil {
					return 1, err
				}
				return 0, nil
			}
		}
		return 1, errors.New("resolver ticket not found")
	})
}
