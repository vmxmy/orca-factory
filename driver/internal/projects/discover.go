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
	ReasonOnboardable             Reason = "可接入"
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
// pipeline check) into a Result. For each repo, in order: scripts/agent-gates/
// itself must exist at all (OF-49: a repository Orca knows about but this
// driver has never onboarded is rejected as ReasonOnboardable instead of
// ReasonPipelineNotInstalled, so the intake gateway can surface it as
// something an approver could set up rather than hide it), its pipeline must
// be installed (agent-graph.env present inside that directory), check must
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
		gatesDir := filepath.Join(repo.Path, "scripts", "agent-gates")
		if _, err := os.Stat(gatesDir); err != nil {
			if os.IsNotExist(err) {
				reject(ReasonOnboardable)
			} else {
				reject(ReasonPipelineNotInstalled)
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(gatesDir, "agent-graph.env")); err != nil {
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

// ReadableRepositories is every repository whose own pipeline profile
// (scripts/agent-gates/agent-graph.env) config.LoadRepository can read, with
// a Feishu ledger backend and a non-empty base. This is resume-deployment's
// manual-recovery criterion (OF-44): unlike Discover, it does not require the
// pipeline check to pass or a ledger project-table row to exist, because a
// deployment barrier can outlive a repository's eligibility, and the recovery
// exit must still be able to find it.
func ReadableRepositories(repos []orca.Repo) []config.Repository {
	result := make([]config.Repository, 0, len(repos))
	for _, repo := range repos {
		repository, err := config.LoadRepository(repo.Path)
		if err != nil || repository.Env["AGENT_ISSUE_BACKEND"] != "feishu" || repository.Env["AGENT_FEISHU_BASE"] == "" {
			continue
		}
		result = append(result, repository)
	}
	return result
}
