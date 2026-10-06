package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

func init() { Register("usage", runUsage) }

var usageTicket = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)

// Unit worktrees follow Orca's <ticket>-<unit> directory naming convention.
var discoverUsageWorktrees = func(ctx context.Context, ticket string) ([]usage.Worktree, error) {
	out, err := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain", "-z").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("usage: list git worktrees: %w: %s", err, strings.TrimSpace(string(out)))
	}
	worktrees := []usage.Worktree{}
	for _, line := range strings.Split(string(out), "\x00") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if ok && strings.HasPrefix(filepath.Base(path), ticket+"-") {
			worktrees = append(worktrees, usage.Worktree{Path: path})
		}
	}
	return worktrees, nil
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
