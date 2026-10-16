package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func init() { Register("projects", runProjects) }

// checkExitBehind is `orca-factory check`'s exit code when every drift is a
// lagging core copy (OF-53).
const checkExitBehind = 3

// factoryChecker is the production projects.Checker (OF-42): it runs this
// machine's own orca-factory binary against a candidate repository and reads
// its `check: BEHIND   <file>` lines when it exits checkExitBehind.
func factoryChecker(factoryRoot string) projects.Checker {
	return func(ctx context.Context, path string) ([]string, error) {
		out, err := runx.Run(ctx, runx.Spec{Path: filepath.Join(factoryRoot, "bin", "orca-factory"), Args: []string{"check", path}, Timeout: 2 * time.Minute})
		if err == nil {
			return nil, nil
		}
		if exitCode(err) != checkExitBehind {
			return nil, err
		}
		var behind []string
		for _, line := range strings.Split(string(out), "\n") {
			if file, ok := strings.CutPrefix(line, "check: BEHIND"); ok {
				behind = append(behind, strings.TrimSpace(file))
			}
		}
		if len(behind) == 0 {
			return nil, errors.New("orca-factory check reported core behind without files")
		}
		return behind, nil
	}
}

// ledgerProjectRows reads the Feishu ledger's 项目 table through the first
// repo whose own profile already names a feishu backend and base: that table
// is base-wide, so any one onboarded repository's credentials reach it. It
// returns nil, nil when no repo qualifies: Discover then rejects every
// feishu-backend repo for having no matching row, which is accurate since
// there is no table to match against.
func ledgerProjectRows(ctx context.Context, repos []orca.Repo, tuning config.Tuning) ([]issues.LedgerProject, error) {
	for _, repo := range repos {
		repository, err := config.LoadRepository(repo.Path)
		if err != nil || repository.Env["AGENT_ISSUE_BACKEND"] != "feishu" || repository.Env["AGENT_FEISHU_BASE"] == "" {
			continue
		}
		client, err := issues.New(ctx, repository.Env, tuning)
		if err != nil {
			return nil, err
		}
		ledger, ok := client.(*issues.Feishu)
		if !ok {
			return nil, errors.New("projects: feishu backend expected")
		}
		return ledger.LedgerProjects(ctx)
	}
	return nil, nil
}

// discoverProjects joins the live Orca repository inventory, the Feishu
// ledger's project table and each repository's own pipeline check (OF-42).
// It is the single project source for serve, the intake gateway and the
// one-shot commands; each caller re-runs it whenever it needs the current
// eligible/rejected set. rows is the ledger project table it joined against.
func discoverProjects(ctx context.Context, cfg config.Config, client *orca.Client) (result projects.Result, rows []issues.LedgerProject, err error) {
	orcaRepos, err := client.RepoList(ctx)
	if err != nil {
		return projects.Result{}, nil, err
	}
	if cfg.FactoryRoot == "" {
		return projects.Discover(ctx, orcaRepos, nil, nil), nil, nil
	}
	if rows, err = ledgerProjectRows(ctx, orcaRepos, cfg.Tuning); err != nil {
		return projects.Result{}, nil, err
	}
	return projects.Discover(ctx, orcaRepos, rows, factoryChecker(cfg.FactoryRoot)), rows, nil
}

// eligibleRepositories is every repository discovery lets this driver take over.
func eligibleRepositories(result projects.Result) []config.Repository {
	repos := make([]config.Repository, 0, len(result.Eligible))
	for _, p := range result.Eligible {
		repos = append(repos, p.Repository)
	}
	return repos
}

func runProjects(args []string) int {
	if len(args) != 1 || args[0] != "--json" {
		fmt.Fprintln(os.Stderr, "usage: orca-factory-driver projects --json")
		return 2
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, _, err := discoverProjects(ctx, cfg, &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
