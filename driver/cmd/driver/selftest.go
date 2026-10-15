package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/executor"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func init() { Register("selftest", selftest) }

// selftest --live re-checks, against the real systems, the external contracts
// recorded in driver/CONTRACTS.md. It is read-only apart from one reviewer call.
func selftest(args []string) int {
	if len(args) != 1 || args[0] != "--live" {
		fmt.Fprintln(os.Stderr, "Usage: driver selftest --live")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot resolve home directory")
		return 1
	}
	cfg, err := config.Load(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	discovered, _, err := discoverProjects(ctx, cfg, &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "project discovery failed:", err)
		return 1
	}
	if len(discovered.Eligible) == 0 {
		fmt.Fprintln(os.Stderr, "project discovery found no repository to take over")
		return 1
	}
	repo := discovered.Eligible[0].Repository
	failed := 0
	check := func(name string, run func() error) {
		if err := run(); err != nil {
			failed++
			fmt.Printf("FAIL %s: %v\n", name, err)
			return
		}
		fmt.Printf("PASS %s\n", name)
	}
	oc := &orca.Client{WorkerStartWait: cfg.Tuning.Orca.WorkerStartWait, Timeout: cfg.Tuning.Orca.CallTimeout}
	check("orca terminal list", func() error { _, err := oc.TerminalList(ctx); return err })
	check("orca task-list carries a parseable worker report", func() error { return liveTaskReport(ctx, oc) })
	check("selected issue backend snapshot with blockers", func() error {
		client, err := issues.New(ctx, repo.Env, cfg.Tuning)
		if err != nil {
			return err
		}
		rows, err := client.Snapshot(ctx, []string{issues.Project(repo.Env)}, time.Time{})
		if err != nil {
			return err
		}
		for _, i := range rows {
			if _, err := executor.ParseTicket(i.ID, i.Identifier, "selftest", ""); err == nil {
				return fmt.Errorf("empty description parsed for %s", i.Identifier)
			}
		}
		return nil
	})
	check("git reaches origin", func() error {
		_, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"ls-remote", "--heads", "origin", "main"}, Dir: repo.Path, Timeout: time.Minute})
		return err
	})
	check("github merge-lane reads parse", func() error { return liveMergeReads(ctx, repo, cfg) })
	check("reviewer output yields a verdict", func() error { return liveReviewer(ctx, repo) })
	if failed > 0 {
		fmt.Printf("selftest: %d check(s) failed\n", failed)
		return 1
	}
	fmt.Println("selftest: all live contract checks passed")
	return 0
}

func liveTaskReport(ctx context.Context, oc *orca.Client) error {
	path, err := runx.OrcaPath()
	if err != nil {
		return err
	}
	raw, err := runx.Run(ctx, runx.Spec{Path: path, Args: []string{"orchestration", "run-list", "--json"}, Timeout: time.Minute})
	if err != nil {
		return err
	}
	var envelope struct {
		Result struct {
			Runs []struct {
				ID string `json:"id"`
			} `json:"runs"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("run-list: %w", err)
	}
	for _, run := range envelope.Result.Runs {
		tasks, err := oc.TaskList(ctx, run.ID)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if report, err := t.WorkerReport(); err == nil && report != nil {
				return nil
			}
		}
	}
	return fmt.Errorf("no settled task with a worker report among %d runs", len(envelope.Result.Runs))
}

func liveReviewer(ctx context.Context, repo config.Repository) error {
	if repo.Env["AGENT_ROLE_REVIEWER_CMD"] == "" {
		return fmt.Errorf("no reviewer command configured")
	}
	small := 1.0
	tools := &executor.Commands{ReviewerCommand: repo.Env["AGENT_ROLE_REVIEWER_CMD"], ReviewerSmallCommand: repo.Env["AGENT_ROLE_REVIEWER_CMD_SMALL"], ReviewerSmallMaxEstimate: &small, InstallCommand: repo.Env["AGENT_INSTALL_CMD"]}
	// An empty diff (origin/main against itself) drives the production reviewer path.
	out, _, err := tools.Review(ctx, repo.Path, "origin/main", "origin/main", &small)
	if err != nil {
		return err
	}
	_, err = executor.ParseReviewVerdict(out)
	return err
}

// liveMergeReads reads the latest merged agent PR the way the merge lane does.
func liveMergeReads(ctx context.Context, repo config.Repository, cfg config.Config) error {
	owner, name, ok := strings.Cut(repo.Env["AGENT_GH_REPO"], "/")
	if !ok {
		return fmt.Errorf("invalid AGENT_GH_REPO")
	}
	gh, err := github.New(github.Options{HTTPTimeout: cfg.Tuning.GitHub.HTTPTimeout})
	if err != nil {
		return err
	}
	main, err := gh.BranchSHA(ctx, owner, name, "main")
	if err != nil || len(main) != 40 {
		return fmt.Errorf("main sha %q: %v", main, err)
	}
	runs, err := gh.CheckRuns(ctx, owner, name, main)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.HeadSHA != main || r.Status == "" {
			return fmt.Errorf("check run %q lacks head or status", r.Name)
		}
	}
	cmp, err := gh.Compare(ctx, owner, name, main+"~1", main)
	if err != nil || len(cmp.Files) == 0 || len(cmp.MergeBaseCommit.SHA) != 40 {
		return fmt.Errorf("compare main~1...main: files=%d base=%q err=%v", len(cmp.Files), cmp.MergeBaseCommit.SHA, err)
	}
	return nil
}
