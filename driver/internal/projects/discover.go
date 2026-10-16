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
// root>/bin/orca-factory check <path>`). It has three outcomes (OF-53): a
// nil error and no files means the core copy matches the factory; a nil
// error with files means the copy only lags (each listed core file is a
// released factory version or newly added), which `orca-factory update`
// fixes; an error means the check failed (a locally modified core file, a
// leftover placeholder, a missing project file). A nil Checker means
// factory_root is not configured: Discover then rejects every repository
// without evaluating any other condition.
type Checker func(ctx context.Context, path string) (behind []string, err error)

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
	// CoreBehind is every core file whose installed copy lags the factory
	// (OF-53); empty when the copy is in step.
	CoreBehind []string `json:",omitempty"`
	// RemoteMismatch is this project's live `git remote get-url origin`,
	// normalized, when it no longer agrees with Remote (OF-56): Orca caches
	// canonicalKey at registration and never refreshes it when the local
	// origin changes later. Empty when they agree, or when the live remote
	// could not be read or normalized — an unconfirmed comparison is not
	// reported as a mismatch. This never changes eligibility: the drift may
	// be an intentional migration a person decides whether to act on.
	RemoteMismatch string `json:",omitempty"`
}

// LiveRemote reads a repository's actual `git remote get-url origin`
// (production: `git -C <path> remote get-url origin`). Discover calls it
// only for repositories that are otherwise eligible, the same scope OF-56
// marks when Orca's cached canonicalKey has drifted. A nil LiveRemote skips
// the comparison entirely, leaving RemoteMismatch empty on every Project.
type LiveRemote func(ctx context.Context, path string) (string, error)

// Rejection is one repository this driver cannot take over right now.
type Rejection struct {
	Path, Remote string
	Reason       Reason
	// Repository is this repository's own profile when ReadableRepositories
	// would accept it, nil otherwise: serve still reads such a repository's
	// ledger for driver-owned in-flight tickets (OF-98).
	Repository *config.Repository `json:"-"`
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
// pass or report only lagging core files (kept on Project.CoreBehind),
// its own profile's AGENT_ISSUE_BACKEND must be "feishu", rows must
// have a row whose 仓库远端 equals its CanonicalKey, and that row must be
// enabled. The first unmet condition is recorded as the Reason; no
// repository is silently skipped. liveRemote, when non-nil, additionally
// compares each eligible project's live origin against its canonicalKey
// (OF-56), filling Project.RemoteMismatch without affecting eligibility.
func Discover(ctx context.Context, repos []orca.Repo, rows []issues.LedgerProject, check Checker, liveRemote LiveRemote) Result {
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
			rejection := Rejection{Path: repo.Path, Remote: repo.CanonicalKey, Reason: reason}
			if repository, ok := readable(repo.Path); ok {
				rejection.Repository = &repository
			}
			result.Rejected = append(result.Rejected, rejection)
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
		behind, err := check(ctx, repo.Path)
		if err != nil {
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
		result.Eligible = append(result.Eligible, Project{Path: repo.Path, Remote: repo.CanonicalKey, Prefix: row.Prefix, Name: row.Name,
			Repository: repository, CoreBehind: behind, RemoteMismatch: remoteMismatch(ctx, repo.Path, repo.CanonicalKey, liveRemote)})
	}
	return result
}

// remoteMismatch is Discover's OF-56 comparison for one eligible
// repository: it returns the normalized live remote when it disagrees with
// canonicalKey, or "" when they agree, liveRemote is nil, the read fails, or
// the live value does not normalize — none of those last three confirm a
// mismatch, so none is reported as one.
func remoteMismatch(ctx context.Context, path, canonicalKey string, liveRemote LiveRemote) string {
	if liveRemote == nil {
		return ""
	}
	live, err := liveRemote(ctx, path)
	if err != nil {
		return ""
	}
	normalized, err := orca.NormalizeRemote(live)
	if err != nil || normalized == canonicalKey {
		return ""
	}
	return normalized
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
		if repository, ok := readable(repo.Path); ok {
			result = append(result, repository)
		}
	}
	return result
}

func readable(path string) (config.Repository, bool) {
	repository, err := config.LoadRepository(path)
	if err != nil || repository.Env["AGENT_ISSUE_BACKEND"] != "feishu" || repository.Env["AGENT_FEISHU_BASE"] == "" {
		return config.Repository{}, false
	}
	return repository, true
}
