// Package projects discovers which of this machine's local Orca-managed
// repositories this driver can take over for Feishu intake and ticket work
// (OF-42): joining Orca's repository inventory, the Feishu ledger's project
// table and each repository's own pipeline check into an eligible set, and a
// typed reason for every repository that is not eligible.
package projects

import (
	"context"
	"os"
	"path/filepath"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// Checker runs a repository's own pipeline check (production: `<factory
// root>/bin/orca-factory check <path>`) and reports whether it passed. A nil
// Checker means factory_root is not configured: Discover then rejects every
// repository without evaluating any other condition.
type Checker func(ctx context.Context, path string) error

// Reason is why a repository cannot be taken over right now. Discover
// evaluates conditions in a fixed order and stops at the first unmet one, so
// each repository carries exactly one Reason.
type Reason string

const (
	ReasonFactoryRootUnconfigured Reason = "factory_root 未配置"
	ReasonPipelineNotInstalled    Reason = "未装流水线"
	ReasonCheckFailed             Reason = "check 失败"
	ReasonLedgerNotFeishu         Reason = "账本不是飞书"
	ReasonNoLedgerRow             Reason = "项目表无对应行"
	ReasonProjectDisabled         Reason = "项目未启用"
)

// Project is one repository this driver can take over: a local checkout
// joined with its ledger project-table row and its own pipeline profile.
type Project struct {
	Path, Remote, Prefix, Name string
	// Repository is this repository's own scripts/agent-gates/agent-graph.env,
	// already parsed: MaxInFlight, StaleHours and Env (for OF-43's scheduler
	// migration and the intake gateway's per-project tiering).
	Repository config.Repository
}

// Rejection is one repository this driver cannot take over right now.
type Rejection struct {
	Path, Remote string
	Reason       Reason
}

// Result is every repository Discover considered, split into what this
// driver can take over and why every other one cannot.
type Result struct {
	Eligible []Project
	Rejected []Rejection
}

// Discover joins repos (Orca's repository inventory), rows (the Feishu
// ledger's 项目 table, possibly empty) and check (each repository's own
// pipeline check) into a Result. For each repo, in order: its pipeline must
// be installed (scripts/agent-gates/agent-graph.env present), check must
// pass, its own profile's AGENT_ISSUE_BACKEND must be "feishu", rows must
// have a row whose 仓库远端 equals its CanonicalKey, and that row must be
// enabled. The first unmet condition is recorded as the Reason; no
// repository is silently skipped.
func Discover(ctx context.Context, repos []orca.Repo, rows []issues.LedgerProject, check Checker) Result {
	var result Result
	if check == nil {
		for _, repo := range repos {
			result.Rejected = append(result.Rejected, Rejection{Path: repo.Path, Remote: repo.CanonicalKey, Reason: ReasonFactoryRootUnconfigured})
		}
		return result
	}
	byRemote := map[string]issues.LedgerProject{}
	for _, row := range rows {
		byRemote[row.Remote] = row
	}
	for _, repo := range repos {
		reject := func(reason Reason) {
			result.Rejected = append(result.Rejected, Rejection{Path: repo.Path, Remote: repo.CanonicalKey, Reason: reason})
		}
		if _, err := os.Stat(filepath.Join(repo.Path, "scripts", "agent-gates", "agent-graph.env")); err != nil {
			reject(ReasonPipelineNotInstalled)
			continue
		}
		if err := check(ctx, repo.Path); err != nil {
			reject(ReasonCheckFailed)
			continue
		}
		repository, err := config.LoadRepository(repo.Path)
		if err != nil {
			reject(ReasonCheckFailed)
			continue
		}
		if repository.Env["AGENT_ISSUE_BACKEND"] != "feishu" {
			reject(ReasonLedgerNotFeishu)
			continue
		}
		row, ok := byRemote[repo.CanonicalKey]
		if !ok {
			reject(ReasonNoLedgerRow)
			continue
		}
		if !row.Enabled {
			reject(ReasonProjectDisabled)
			continue
		}
		result.Eligible = append(result.Eligible, Project{Path: repo.Path, Remote: repo.CanonicalKey, Prefix: row.Prefix, Name: row.Name, Repository: repository})
	}
	return result
}
