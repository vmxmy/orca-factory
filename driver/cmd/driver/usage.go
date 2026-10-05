package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

func init() { Register("usage", runUsage) }

var usageTicket = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)

func runUsage(args []string) int {
	if len(args) == 0 || !usageTicket.MatchString(args[0]) {
		fmt.Fprintln(os.Stderr, "usage: driver usage <ticket> [--json] [--worktree /path] [--reviewer-worktree /path] [--run-worktree /path --claimed-tickets N]")
		return 2
	}
	options := usage.Options{Ticket: args[0]}
	flags := flag.NewFlagSet("usage", flag.ContinueOnError)
	asJSON := flags.Bool("json", false, "emit JSON")
	flags.StringVar(&options.ClaudeDir, "claude-dir", "", "Claude projects directory")
	flags.StringVar(&options.CodexDir, "codex-dir", "", "Codex sessions directory")
	flags.StringVar(&options.RunWorktree, "run-worktree", "", "coordinator worktree for this run")
	flags.IntVar(&options.ClaimedTickets, "claimed-tickets", 0, "distinct tickets claimed in this run")
	flags.Func("worktree", "ticket unit worktree (repeatable)", func(path string) error {
		options.Worktrees = append(options.Worktrees, usage.Worktree{Path: path})
		return nil
	})
	flags.Func("reviewer-worktree", "reviewer worktree (repeatable)", func(path string) error {
		options.Worktrees = append(options.Worktrees, usage.Worktree{Path: path, Role: "reviewer"})
		return nil
	})
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return 2
	}
	if len(options.Worktrees) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		worktrees, err := usageWorktrees(ctx, options.Ticket)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		options.Worktrees = worktrees
	}
	result, err := usage.Collect(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		if json.NewEncoder(os.Stdout).Encode(result) != nil {
			fmt.Fprintln(os.Stderr, "usage: cannot encode result")
			return 1
		}
	} else {
		fmt.Printf("%s · API estimate USD · prices %s\n", result.Ticket, result.PriceVersion)
		for _, item := range result.Items {
			fmt.Printf("%s %s input=%g output=%g cache_read=%g cache_write=%g cache_write_hour=%g USD=%.8f unknown_model=%t apportioned=%t\n", item.Role, item.Model, item.InputTokens, item.OutputTokens, item.CacheReadTokens, item.CacheWriteTokens, item.CacheWriteHourTokens, item.CostUSD, item.UnknownModel, item.Apportioned)
		}
		for _, failure := range result.Errors {
			fmt.Fprintf(os.Stderr, "usage: %s: %s\n", failure.Path, failure.Error)
		}
	}
	return 0
}

// Discovery uses an explicit ticket link only. Role and run allocation cannot
// be inferred safely from a directory name; callers can provide those flags.
func usageWorktrees(ctx context.Context, ticket string) ([]usage.Worktree, error) {
	path, err := runx.OrcaPath()
	if err != nil {
		return nil, err
	}
	output, err := runx.Run(ctx, runx.Spec{Path: path, Args: []string{"worktree", "list", "--json"}, Timeout: 30 * time.Second})
	if err != nil {
		return nil, errors.New("usage: cannot list Orca worktrees")
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Truncated bool `json:"truncated"`
			Worktrees []struct {
				Path   string `json:"path"`
				Ticket string `json:"linkedLinearIssue"`
			} `json:"worktrees"`
		} `json:"result"`
	}
	if json.Unmarshal(output, &envelope) != nil || !envelope.OK || envelope.Result.Truncated || envelope.Result.Worktrees == nil {
		return nil, errors.New("usage: invalid or incomplete Orca worktree list")
	}
	var worktrees []usage.Worktree
	for _, w := range envelope.Result.Worktrees {
		if w.Ticket == ticket {
			worktrees = append(worktrees, usage.Worktree{Path: w.Path})
		}
	}
	if len(worktrees) == 0 {
		return nil, errors.New("usage: no linked worktrees; provide --worktree explicitly")
	}
	return worktrees, nil
}
