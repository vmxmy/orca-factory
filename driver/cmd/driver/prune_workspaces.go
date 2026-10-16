package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

func init() { Register("prune-workspaces", runPruneWorkspaces) }

// runPruneWorkspaces applies serve's workspace reclaim rule (OF-96) once to
// every discovered project; --dry-run prints the decisions without removing.
func runPruneWorkspaces(args []string) int {
	dry := len(args) == 1 && args[0] == "--dry-run"
	if len(args) != 0 && !dry {
		fmt.Fprintln(os.Stderr, "usage: driver prune-workspaces [--dry-run]")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := pruneDiscoveredWorkspaces(ctx, dry, logger); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func pruneDiscoveredWorkspaces(ctx context.Context, dry bool, logger *slog.Logger) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return errors.New("cannot resolve home directory")
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	discovered, err := projectSource(cfg)(ctx)
	if err != nil {
		return err
	}
	repos := eligibleRepositories(discovered)
	runtime, err := scheduler.NewRuntime(ctx, cfg, repos)
	if err != nil {
		return err
	}
	return pruneWorkspaces(ctx, runtime, repos, dry, os.Stdout, logger)
}

// pruneWorkspaces reads ticket states once, then prints one line per
// reclaimable workspace: removed (or would-remove) and kept with its blocks.
func pruneWorkspaces(ctx context.Context, runtime *scheduler.Runtime, repos []config.Repository, dry bool, out io.Writer, logger *slog.Logger) error {
	if _, err := runtime.Snapshot(ctx, repos); err != nil {
		var evidence *scheduler.SnapshotEvidenceError
		if !errors.As(err, &evidence) {
			return err
		}
	}
	verdicts, reclaimErr := runtime.ReclaimWorkspaces(ctx, dry, logger)
	for _, v := range verdicts {
		action, blocks := "removed", ""
		if dry {
			action = "would-remove"
		}
		if len(v.Blocks) != 0 {
			action, blocks = "keep", " blocks="+strings.Join(v.Blocks, ",")
		}
		if _, err := fmt.Fprintf(out, "%s %s ticket=%s reason=%q%s\n", action, v.Path, v.Ticket, v.Reason, blocks); err != nil {
			return errors.Join(reclaimErr, err)
		}
	}
	return reclaimErr
}
