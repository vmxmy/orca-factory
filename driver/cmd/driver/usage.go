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
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

func init() { Register("usage", runUsage) }

var usageTicket = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)

func runUsage(args []string) int {
	if len(args) == 0 || !usageTicket.MatchString(args[0]) {
		fmt.Fprintln(os.Stderr, "usage: driver usage <ticket> --json")
		return 2
	}
	options := usage.Options{Ticket: args[0]}
	flags := flag.NewFlagSet("usage", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	asJSON := flags.Bool("json", false, "emit JSON")
	flags.StringVar(&options.ClaudeDir, "claude-dir", "", "Claude session root")
	flags.StringVar(&options.CodexDir, "codex-dir", "", "Codex session root")
	flags.StringVar(&options.RunWorktree, "run-worktree", "", "verified coordinator worktree")
	flags.StringVar(&options.RunID, "run-id", "", "verified run identifier")
	flags.Func("coordinator-session", "provider session ID explicitly associated with this run (repeatable)", func(id string) error {
		options.CoordinatorSessions = append(options.CoordinatorSessions, id)
		return nil
	})
	flags.Func("claimed-ticket", "ticket actually claimed in this run (repeatable)", func(ticket string) error {
		if !usageTicket.MatchString(ticket) {
			return errors.New("invalid claimed ticket")
		}
		options.ClaimedTickets = append(options.ClaimedTickets, ticket)
		return nil
	})
	flags.Func("worktree", "associated ticket worktree (repeatable)", func(path string) error {
		options.Worktrees = append(options.Worktrees, usage.Worktree{Path: path})
		return nil
	})
	flags.Func("reviewer-worktree", "associated reviewer worktree (repeatable)", func(path string) error {
		options.Worktrees = append(options.Worktrees, usage.Worktree{Path: path, Role: "reviewer"})
		return nil
	})
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if len(options.Worktrees) == 0 {
		discovered, err := usageDiscovery(ctx, options.Ticket)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		options.Worktrees = discovered.Worktrees
		if options.RunID != "" && options.RunID != discovered.RunID {
			fmt.Fprintln(os.Stderr, "usage: explicit run ID conflicts with ticket metadata")
			return 1
		}
		options.RunID = discovered.RunID
	}
	if options.RunID == "" || options.RunWorktree == "" || len(options.CoordinatorSessions) == 0 || len(options.ClaimedTickets) == 0 {
		fmt.Fprintln(os.Stderr, "usage: read-only attribution metadata missing: verified run worktree, coordinator provider session IDs and current-run claimed ticket identifiers; executor.json and Orca Run/terminal metadata do not supply these associations")
		return 1
	}
	result, err := usage.Collect(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, failure := range result.Errors {
		fmt.Fprintf(os.Stderr, "usage: %s: %s\n", failure.Path, failure.Error)
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
	}
	return 0
}

// Existing executor checkpoints identify ticket runs and unit worktrees. They
// do not identify coordinator provider sessions or an actual run claim set.
// Discovery never substitutes a directory name or all historical tickets.
func usageDiscovery(ctx context.Context, ticket string) (usage.Options, error) {
	result := usage.Options{Ticket: ticket}
	home, err := os.UserHomeDir()
	if err != nil {
		return result, err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return result, err
	}
	root := os.Getenv("AGENT_DRIVER_STATE_ROOT")
	if root == "" {
		root = filepath.Join(home, ".local", "state", "orca-factory")
	}
	var found bool
	for _, repo := range cfg.Repositories {
		_, name, ok := strings.Cut(repo.Env["AGENT_GH_REPO"], "/")
		if !ok || name == "" || filepath.Base(name) != name || name == "." || name == ".." {
			return result, errors.New("usage: invalid configured repository name")
		}
		path := filepath.Join(root, name, ticket, "executor.json")
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, fmt.Errorf("usage: open ticket checkpoint: %w", err)
		}
		var cp struct{ Run, Dispatch, Worktree string }
		err = json.NewDecoder(f).Decode(&cp)
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return result, errors.New("usage: invalid ticket checkpoint")
		}
		if found {
			return result, errors.New("usage: ticket exists in multiple configured repositories")
		}
		found = true
		if cp.Run == "" || cp.Dispatch == "" {
			return result, errors.New("usage: checkpoint missing Run/Dispatch association")
		}
		var metadata struct {
			Dispatch struct {
				RunID string `json:"runId"`
			} `json:"dispatch"`
			Worker struct {
				WorktreeID string `json:"worktreeId"`
			} `json:"worker"`
		}
		if err := usageOrca(ctx, &metadata, "orchestration", "worker-show", "--dispatch", cp.Dispatch); err != nil {
			return result, err
		}
		_, worktreePath, ok := strings.Cut(metadata.Worker.WorktreeID, "::")
		if metadata.Dispatch.RunID != cp.Run || !ok || !filepath.IsAbs(worktreePath) {
			return result, errors.New("usage: Orca dispatch missing verified run/worktree association")
		}
		if cp.Worktree != "" && filepath.Clean(cp.Worktree) != filepath.Clean(worktreePath) {
			return result, errors.New("usage: checkpoint and Orca worktree disagree")
		}
		result.RunID = cp.Run
		result.Worktrees = append(result.Worktrees, usage.Worktree{Path: worktreePath})
		var trees struct {
			Truncated bool `json:"truncated"`
			Worktrees []struct {
				Path   string `json:"path"`
				Ticket string `json:"linkedLinearIssue"`
			} `json:"worktrees"`
		}
		if err := usageOrca(ctx, &trees, "worktree", "list", "--repo", "path:"+repo.Path); err != nil {
			return result, err
		}
		if trees.Truncated || trees.Worktrees == nil {
			return result, errors.New("usage: incomplete Orca worktree list")
		}
		for _, tree := range trees.Worktrees {
			if tree.Ticket == ticket && tree.Path != worktreePath {
				result.Worktrees = append(result.Worktrees, usage.Worktree{Path: tree.Path})
			}
		}
	}
	if !found {
		return result, errors.New("usage: no ticket checkpoint in configured repositories")
	}
	return result, nil
}
func usageOrca(ctx context.Context, result any, args ...string) error {
	path, err := runx.OrcaPath()
	if err != nil {
		return err
	}
	output, err := runx.Run(ctx, runx.Spec{Path: path, Args: append(args, "--json"), Timeout: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("usage: read Orca metadata: %w", err)
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(output, &envelope) != nil || !envelope.OK || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return errors.New("usage: invalid Orca metadata response")
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return errors.New("usage: invalid Orca metadata result")
	}
	return nil
}
