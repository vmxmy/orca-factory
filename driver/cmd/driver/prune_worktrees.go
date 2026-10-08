package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/vmxmy/orca-factory/driver/internal/worktree"
)

func init() { Register("prune-worktrees", runPruneWorktrees) }

func runPruneWorktrees(args []string) int {
	observed := make(chan os.Signal, 1)
	signal.Notify(observed, syscall.SIGINT, syscall.SIGTERM, syscall.SIGALRM)
	defer signal.Stop(observed)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGALRM)
	defer cancel()
	code := worktree.RunPrune(ctx, args, os.Stdout, os.Stderr)
	select {
	case interrupted := <-observed:
		if number, ok := interrupted.(syscall.Signal); ok {
			return 128 + int(number)
		}
	default:
	}
	return code
}
