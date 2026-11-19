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
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
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
	if args[0] == "status" {
		result, err := gearProjects(ctx)
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
	remote, err := gearRemote(ctx, args[1])
	if err == nil {
		err = config.SetGearOverride(remote, mode, operator, reason)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func gearProjects(ctx context.Context) (projects.Result, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return projects.Result{}, err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return projects.Result{}, err
	}
	result, _, err := discoverProjects(ctx, cfg, &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout})
	return result, err
}

func gearRemote(ctx context.Context, selector string) (string, error) {
	if info, err := os.Stat(selector); err == nil && info.IsDir() {
		path, err := filepath.Abs(selector)
		if err != nil {
			return "", err
		}
		return config.RepositoryRemote(path)
	}
	result, err := gearProjects(ctx)
	if err != nil {
		return "", err
	}
	var remote string
	for _, p := range result.Eligible {
		if !strings.EqualFold(p.Prefix, selector) {
			continue
		}
		if remote != "" && remote != p.Remote {
			return "", errors.New("ambiguous repository prefix")
		}
		remote, err = config.RepositoryRemote(p.Path)
		if err != nil {
			return "", err
		}
		if remote != p.Remote {
			return "", errors.New("repository origin differs from project remote")
		}
	}
	if remote == "" {
		return "", errors.New("repository prefix or path not found")
	}
	return remote, nil
}
