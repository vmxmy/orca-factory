package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

func init() { Register("usage", runUsage) }

var usageTicket = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)

// Ticket discovery must enumerate linked worktrees, including those without a terminal.
// The existing Orca client only exposes worker and terminal observations.
var discoverUsageWorktrees = func(context.Context, string) ([]usage.Worktree, error) {
	return nil, errors.New("usage: ticket worktree discovery requires a worktree-list API in driver/internal/orca/orca.go; the existing worker and terminal lists do not enumerate ticket-linked worktrees")
}

func runUsage(args []string) int {
	usageError := func() int {
		fmt.Fprintln(os.Stderr, "Usage: driver usage <ticket> --json [--reviewer-worktree <path>] [--run-worktree <path> --coordinator-session <id> --claimed-ticket-count <count>]")
		return 2
	}
	if len(args) == 0 || !usageTicket.MatchString(args[0]) {
		return usageError()
	}
	options := usage.Options{Ticket: args[0]}
	flags := flag.NewFlagSet("usage", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	asJSON := flags.Bool("json", false, "emit JSON")
	flags.StringVar(&options.ReviewerWorktree, "reviewer-worktree", "", "directory explicitly associated with this ticket's reviewer")
	flags.StringVar(&options.RunWorktree, "run-worktree", "", "coordinator worktree for this run")
	flags.StringVar(&options.CoordinatorSession, "coordinator-session", "", "coordinator provider session ID for this run")
	flags.IntVar(&options.ClaimedTicketCount, "claimed-ticket-count", 0, "number of tickets claimed in this run")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return usageError()
	}
	if options.ReviewerWorktree != "" && !filepath.IsAbs(options.ReviewerWorktree) {
		return usageError()
	}
	if options.RunWorktree != "" || options.CoordinatorSession != "" || options.ClaimedTicketCount != 0 {
		if !filepath.IsAbs(options.RunWorktree) || options.CoordinatorSession == "" || options.ClaimedTicketCount < 1 {
			return usageError()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	worktrees, err := discoverUsageWorktrees(ctx, options.Ticket)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	options.Worktrees = worktrees
	paths := make([]string, 0, len(worktrees))
	for _, tree := range worktrees {
		paths = append(paths, tree.Path)
	}
	result, err := usage.Collect(ctx, options.Ticket, paths, options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fmt.Fprintln(os.Stderr, "usage: encode result:", err)
			return 1
		}
	} else {
		fmt.Printf("%s · API estimate USD · prices %s\n", result.Ticket, result.PriceVersion)
		for _, item := range result.Items {
			fmt.Printf("%s %s input=%g output=%g cache_read=%g cache_write=%g cache_write_hour=%g USD=%.8f unknown_model=%t apportioned=%t\n", item.Role, item.Model, item.InputTokens, item.OutputTokens, item.CacheReadTokens, item.CacheWriteTokens, item.CacheWriteHourTokens, item.CostUSD, item.UnknownModel, item.Apportioned)
		}
		for _, skipped := range result.Skipped {
			fmt.Printf("skipped %s: %s\n", skipped.Path, skipped.Error)
		}
	}
	return 0
}
