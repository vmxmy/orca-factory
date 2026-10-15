package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func init() { Register("projects", runProjects) }

// factoryChecker is the production projects.Checker (OF-42): it runs this
// machine's own orca-factory binary against a candidate repository.
func factoryChecker(factoryRoot string) projects.Checker {
	return func(ctx context.Context, path string) error {
		_, err := runx.Run(ctx, runx.Spec{Path: filepath.Join(factoryRoot, "bin", "orca-factory"), Args: []string{"check", path}, Timeout: 2 * time.Minute})
		return err
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

// toDiscoverRepos adapts orca.Client.RepoList's result to projects.Discover's input.
func toDiscoverRepos(repos []orca.Repo) []projects.Repo {
	out := make([]projects.Repo, 0, len(repos))
	for _, r := range repos {
		out = append(out, projects.Repo{Path: r.Path, CanonicalKey: r.CanonicalKey})
	}
	return out
}

// discoverProjects joins the live Orca repository inventory, the Feishu
// ledger's project table and each repository's own pipeline check (OF-42).
// It never reads driver.toml's repositories: the caller re-runs it whenever
// it needs the current eligible/rejected set.
func discoverProjects(ctx context.Context, cfg config.Config, client *orca.Client) (projects.Result, error) {
	orcaRepos, err := client.RepoList(ctx)
	if err != nil {
		return projects.Result{}, err
	}
	if cfg.FactoryRoot == "" {
		return projects.Discover(ctx, toDiscoverRepos(orcaRepos), nil, nil), nil
	}
	rows, err := ledgerProjectRows(ctx, orcaRepos, cfg.Tuning)
	if err != nil {
		return projects.Result{}, err
	}
	return projects.Discover(ctx, toDiscoverRepos(orcaRepos), rows, factoryChecker(cfg.FactoryRoot)), nil
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
	result, err := discoverProjects(ctx, cfg, &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout})
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
