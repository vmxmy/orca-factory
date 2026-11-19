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
	"slices"
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
		result, ledgerRows, err := discoverProjects(ctx, cfg, client)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		rows := []gearStatus{}
		add := func(remote, prefix, path string, repo config.Repository) {
			mode, source := repo.Env["AGENT_MODE"], "agent-graph.env"
			if mode == "" {
				mode, source = "manual", "default"
			}
			if repo.Sources["AGENT_MODE"] == "override" {
				source = "override"
			}
			rows = append(rows, gearStatus{Remote: remote, Prefix: prefix, Path: path, Mode: mode, Source: source})
		}
		for _, p := range result.Eligible {
			add(p.Remote, p.Prefix, p.Path, p.Repository)
		}
		// A rejected repository may still hold in-flight tickets. The scheduler
		// applies only its gear override to them and otherwise keeps the mode it
		// tracked, which this process cannot read, so only overrides are listed.
		for _, r := range result.Rejected {
			if r.Repository == nil {
				if _, err := config.LoadRepository(r.Path); err == nil {
					if _, err := config.LoadRegisteredRepository(r.Remote, r.Path); err != nil {
						fmt.Fprintf(os.Stderr, "gear: %s: %v\n", r.Path, err)
						return 1
					}
				}
				continue
			}
			if r.Repository.Sources["AGENT_MODE"] != "override" {
				continue
			}
			prefix := ""
			if i := slices.IndexFunc(ledgerRows, func(row issues.LedgerProject) bool { return row.Remote == r.Remote }); i >= 0 {
				prefix = ledgerRows[i].Prefix
			}
			add(r.Remote, prefix, r.Path, *r.Repository)
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

var errRepositoryNotRegistered = errors.New("repository prefix or path not found")

func gearTarget(ctx context.Context, selector string, client *orca.Client, tuning config.Tuning) (orca.Repo, error) {
	repos, err := client.RepoList(ctx)
	if err != nil {
		return orca.Repo{}, err
	}
	var root string
	var remotes map[string]bool
	if selector == "." || selector == ".." || strings.Contains(selector, "/") {
		root, err = physicalPath(selector)
		if err != nil {
			return orca.Repo{}, err
		}
	} else {
		remotes, err = gearPrefixRemotes(ctx, selector, repos, tuning)
		if err != nil {
			return orca.Repo{}, err
		}
	}

	var matches []orca.Repo
	for _, repo := range repos {
		if remotes[repo.CanonicalKey] || (root != "" && resolvesTo(repo.Path, root)) {
			matches = append(matches, repo)
		}
	}
	switch len(matches) {
	case 0:
		return orca.Repo{}, errRepositoryNotRegistered
	case 1:
		return matches[0], nil
	default:
		return orca.Repo{}, errors.New("ambiguous repository prefix or path")
	}
}

// physicalPath resolves selector against the physical working directory, so
// ".." follows the file system rather than the shell's logical PWD.
func physicalPath(selector string) (string, error) {
	resolved, err := filepath.EvalSymlinks(selector)
	if err != nil || filepath.IsAbs(resolved) {
		return resolved, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if cwd, err = filepath.EvalSymlinks(cwd); err != nil {
		return "", err
	}
	return filepath.Join(cwd, resolved), nil
}

func resolvesTo(path, root string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == root
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
