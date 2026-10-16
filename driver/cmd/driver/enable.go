package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// mergedAgentGraphInstalled reports whether req's target repository's
// origin/main (not the local checkout's current branch) already carries
// scripts/agent-gates/agent-graph.env, read via `git ls-tree` after a fetch
// (OF-54): the onboarding PR's merge into main is the fact that unlocks
// enabling, and the local checkout discovery already walked can sit on any
// branch (an onboarding worktree still on feat/agent-graph, for instance).
func mergedAgentGraphInstalled(ctx context.Context, repo string) (bool, error) {
	if _, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "fetch", "--quiet", "origin", "main"}, Timeout: 2 * time.Minute}); err != nil {
		return false, fmt.Errorf("git fetch origin main 失败：%w", err)
	}
	out, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "ls-tree", "origin/main", "--", "scripts/agent-gates/agent-graph.env"}, Timeout: 30 * time.Second})
	if err != nil {
		return false, fmt.Errorf("git ls-tree origin/main 失败：%w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// enabler builds intake.Enabler (OF-54): an approver's "启用" press
// re-validates live, rather than trusting a card's rendering (which can be
// stale, or absent entirely right after OF-50's onboarding completes), that
// the onboarding PR merged into origin/main and that the repository's own
// pipeline check still passes, before writing the ledger row's 启用=true.
type enabler struct {
	checker  func(ctx context.Context, path string) ([]string, error)
	writeRow func(ctx context.Context, remote, operator string) (issues.LedgerProject, error)
}

func (e enabler) run(ctx context.Context, req intake.EnableRequest) (intake.EnableResult, error) {
	merged, err := mergedAgentGraphInstalled(ctx, req.Repo)
	if err != nil {
		return intake.EnableResult{}, err
	}
	if !merged {
		return intake.EnableResult{}, errors.New("接入 PR 未合并（origin/main 上没有 scripts/agent-gates/agent-graph.env）")
	}
	if _, err := e.checker(ctx, req.Repo); err != nil {
		return intake.EnableResult{}, fmt.Errorf("check 失败：%w", err)
	}
	row, err := e.writeRow(ctx, req.Remote, req.Requester)
	if err != nil {
		return intake.EnableResult{}, fmt.Errorf("项目表写入失败：%w", err)
	}
	return intake.EnableResult{Row: row}, nil
}

// factoryEnabledWriter writes the enabled ledger row through the factory's
// own Feishu ledger profile, mirroring factoryLedgerRow (OF-50).
func factoryEnabledWriter(cfg config.Config) func(context.Context, string, string) (issues.LedgerProject, error) {
	return func(ctx context.Context, remote, operator string) (issues.LedgerProject, error) {
		repository, err := config.LoadRepository(cfg.FactoryRoot)
		if err != nil {
			return issues.LedgerProject{}, err
		}
		client, err := issues.New(ctx, repository.Env, cfg.Tuning)
		if err != nil {
			return issues.LedgerProject{}, err
		}
		ledger, ok := client.(*issues.Feishu)
		if !ok {
			return issues.LedgerProject{}, errors.New("factory 账本不是飞书")
		}
		return ledger.EnableLedgerProject(ctx, remote, operator)
	}
}
