package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

func init() { Register("gear", runGear) }

const gearUsage = "usage: driver gear status [--json] | set <repo-prefix-or-path> <auto|manual> --operator <name> --reason <reason> | clear <repo-prefix-or-path>"

type gearStatus struct {
	Remote string `json:"remote"`
	Prefix string `json:"prefix"`
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Source string `json:"source"`
}

func runGear(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, gearUsage)
		return 2
	}
	var mode, operator, reason string
	switch args[0] {
	case "status":
		if len(args) != 1 && (len(args) != 2 || args[1] != "--json") {
			fmt.Fprintln(os.Stderr, gearUsage)
			return 2
		}
	case "set":
		if len(args) < 3 || (args[2] != "auto" && args[2] != "manual") {
			fmt.Fprintln(os.Stderr, gearUsage)
			return 2
		}
		mode = args[2]
		flags := flag.NewFlagSet("gear set", flag.ContinueOnError)
		flags.StringVar(&operator, "operator", "", "operator name")
		flags.StringVar(&reason, "reason", "", "reason for changing mode")
		if flags.Parse(args[3:]) != nil || flags.NArg() != 0 || strings.TrimSpace(operator) == "" || strings.TrimSpace(reason) == "" {
			fmt.Fprintln(os.Stderr, gearUsage)
			return 2
		}
	case "clear":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, gearUsage)
			return 2
		}
		current, err := user.Current()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		operator, reason = current.Username, "restore repository configuration"
	default:
		fmt.Fprintln(os.Stderr, gearUsage)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg, err := config.Load(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	client := &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout}
	if args[0] == "status" {
		result, _, err := discoverProjects(ctx, cfg, client)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		rows := []gearStatus{}
		for _, p := range result.Eligible {
			mode, source := p.Repository.Env["AGENT_MODE"], "agent-graph.env"
			if mode == "" {
				mode, source = "manual", "default"
			}
			if p.Repository.Sources["AGENT_MODE"] == "override" {
				source = "override"
			}
			rows = append(rows, gearStatus{Remote: p.Remote, Prefix: p.Prefix, Path: p.Path, Mode: mode, Source: source})
		}
		if len(args) == 2 {
			if err := json.NewEncoder(os.Stdout).Encode(rows); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		} else {
			for _, row := range rows {
				if _, err := fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\n", row.Prefix, row.Mode, row.Source, row.Path); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
			}
		}
		return 0
	}
	repo, err := gearTarget(ctx, args[1], client, cfg.Tuning)
	if err == nil {
		err = config.SetGearOverride(repo.CanonicalKey, repo.Path, mode, operator, reason)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func gearTarget(ctx context.Context, selector string, client *orca.Client, tuning config.Tuning) (orca.Repo, error) {
	repos, err := client.RepoList(ctx)
	if err != nil {
		return orca.Repo{}, err
	}
	var root string
	var remotes map[string]bool
	if selector == "." || selector == ".." || strings.Contains(selector, "/") {
		root, err = filepath.Abs(selector)
		if err != nil {
			return orca.Repo{}, err
		}
		root = filepath.Clean(root)
	} else {
		remotes, err = gearPrefixRemotes(ctx, selector, repos, tuning)
		if err != nil {
			return orca.Repo{}, err
		}
	}

	var matches []orca.Repo
	for _, repo := range repos {
		if filepath.Clean(repo.Path) == root || remotes[repo.CanonicalKey] {
			matches = append(matches, repo)
		}
	}
	switch len(matches) {
	case 0:
		return orca.Repo{}, errors.New("repository prefix or path not found")
	case 1:
		return matches[0], nil
	default:
		return orca.Repo{}, errors.New("ambiguous repository prefix or path")
	}
}

func gearPrefixRemotes(ctx context.Context, selector string, repos []orca.Repo, tuning config.Tuning) (map[string]bool, error) {
	for _, repo := range repos {
		profile, err := config.LoadRepository(repo.Path)
		if err != nil || profile.Env["AGENT_ISSUE_BACKEND"] != "feishu" || profile.Env["AGENT_FEISHU_BASE"] == "" {
			continue
		}
		store, err := issues.New(ctx, profile.Env, tuning)
		if err != nil {
			return nil, err
		}
		ledger, ok := store.(*issues.Feishu)
		if !ok {
			return nil, errors.New("gear: feishu backend expected")
		}
		rows, err := ledger.LedgerProjects(ctx)
		if err != nil {
			return nil, err
		}
		remotes := make(map[string]bool)
		for _, row := range rows {
			if strings.EqualFold(selector, row.Prefix) {
				remotes[row.Remote] = true
			}
		}
		return remotes, nil
	}
	return nil, nil
}
