package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// sceneProbe is the raw residue evidence around one repository's onboarding
// scene; classifyScene below is its single interpreter, shared by deriveStage
// and preflight group (d), so "resumable" and "residue" can never disagree.
type sceneProbe struct {
	PathExists   bool
	OurWorktree  bool
	LocalBranch  bool
	RemoteBranch bool
}

// sceneState is classifyScene's verdict on a sceneProbe.
type sceneState int

const (
	sceneClear sceneState = iota
	sceneOurs
	sceneResidue
)

// classifyScene is pure: clear when nothing exists; ours when this machine's
// onboarding worktree sits on the onboarding branch and the local branch
// exists (the remote branch may too — the agent pushed, PR creation failed);
// residue for everything else.
func classifyScene(p sceneProbe) sceneState {
	switch {
	case !p.PathExists && !p.LocalBranch && !p.RemoteBranch:
		return sceneClear
	case p.OurWorktree && p.LocalBranch:
		return sceneOurs
	default:
		return sceneResidue
	}
}

// sceneResidueDetail names exactly what exists in a scene that is not clear,
// in the wording preflight group (d) has always used, so the Blocked verdict
// and the gateway's refusal tell a person what to clean.
func sceneResidueDetail(p sceneProbe) string {
	var items []string
	if p.PathExists {
		items = append(items, "worktree 已存在")
	}
	if p.LocalBranch {
		items = append(items, "本地分支 "+onboardBranch+" 已存在")
	}
	if p.RemoteBranch {
		items = append(items, "远端分支 "+onboardBranch+" 已存在")
	}
	return strings.Join(items, "；")
}

// rowState is the repository's ledger 项目 row: absent, present with 启用
// false, or enabled.
type rowState int

const (
	rowAbsent rowState = iota
	rowDisabled
	rowEnabled
)

// prState is the onboarding PR's gh state; prNone when gh reports no PR from
// this repository on the onboarding branch.
type prState int

const (
	prNone prState = iota
	prOpen
	prMerged
	prClosed
)

// onboardPR is the onboarding PR read back from gh.
type onboardPR struct {
	State  prState
	Number int
}

var (
	prStateByName = map[string]prState{"OPEN": prOpen, "MERGED": prMerged, "CLOSED": prClosed}
	prStateRank   = map[prState]int{prOpen: 3, prMerged: 2, prClosed: 1}
)

// onboardFacts is everything stage derivation reads, observed fresh on every
// wizard loop iteration from the ledger, origin/main, gh, git and the
// onboarding lock. Never persisted. Residue carries sceneResidueDetail's
// list so a Blocked verdict can name what to clean.
type onboardFacts struct {
	Repo, Remote string
	Row          rowState
	Installed    bool
	PR           onboardPR
	Scene        sceneState
	Residue      string
	Busy         bool
}

// onboardStage is what the wizard does next for one repository.
type onboardStage int

const (
	stageStart onboardStage = iota
	stageResume
	stageWaitMerge
	stageEnable
	stageDone
	stageBlocked
)

// stageVerdict is deriveStage's decision; Why carries the problem plus the
// fix a person applies, for Blocked only.
type stageVerdict struct {
	Stage onboardStage
	PR    onboardPR
	Why   string
}

// deriveStage maps one observation to the wizard's next stage. Pure; first
// match wins (OF-197). The merge fact is origin/main only and the discovery
// reason never routes: a checkout that predates the merge still routes to
// Enable, and a closed PR only blocks a scene that is not clear.
func deriveStage(f onboardFacts) stageVerdict {
	switch {
	case f.Row == rowEnabled:
		return stageVerdict{Stage: stageDone}
	case f.Busy:
		return stageVerdict{Stage: stageBlocked, Why: "已有进行中的接入（另一终端或飞书卡片）；补法：等它结束，或在其续接"}
	case f.Installed && f.Row == rowAbsent:
		return stageVerdict{Stage: stageBlocked, Why: "origin/main 已有 scripts/agent-gates/agent-graph.env，但项目表没有 " + f.Remote + " 的行；补法：在飞书项目表为该仓库补一行（启用=false）后重跑"}
	case f.Installed:
		return stageVerdict{Stage: stageEnable}
	case f.PR.State == prOpen:
		return stageVerdict{Stage: stageWaitMerge, PR: f.PR}
	case f.PR.State == prMerged:
		return stageVerdict{Stage: stageBlocked, Why: fmt.Sprintf("接入 PR #%d 已合并，但 origin/main 上没有 scripts/agent-gates/agent-graph.env；补法：核对该 PR 实际合入的提交", f.PR.Number)}
	case f.PR.State == prClosed && f.Scene != sceneClear:
		return stageVerdict{Stage: stageBlocked, Why: fmt.Sprintf("接入 PR #%d 已关闭，现场仍有残留；补法：清理 worktree 与 %s 分支后重跑（现场干净时会开新 PR）", f.PR.Number, onboardBranch)}
	case f.Scene == sceneOurs:
		return stageVerdict{Stage: stageResume}
	case f.Scene == sceneClear:
		return stageVerdict{Stage: stageStart}
	default:
		return stageVerdict{Stage: stageBlocked, Why: "现场有接入残留：" + f.Residue + "；补法：确认归属后清理（git worktree remove <路径>、git branch -D " + onboardBranch + "、git push origin --delete " + onboardBranch + "），再重跑"}
	}
}

// observe gathers one repository's onboardFacts from external systems only
// (OF-197): one discovery pass for the remote and the ledger row, origin/main
// for the merge fact, gh for the PR, git for the scene and flock for busy.
// Nothing is persisted; the wizard re-runs it every loop iteration, which is
// what makes an interrupted run resumable.
func (o onboarder) observe(ctx context.Context, repo string) (onboardFacts, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return onboardFacts{}, err
	}
	result, rows, err := discoverProjects(ctx, o.cfg, o.orca)
	if err != nil {
		return onboardFacts{}, fmt.Errorf("项目发现失败：%w", err)
	}
	remote := ""
	for _, r := range result.Rejected {
		if r.Path == abs {
			remote = r.Remote
		}
	}
	for _, p := range result.Eligible {
		if p.Path == abs {
			remote = p.Remote
		}
	}
	if remote == "" {
		return onboardFacts{}, fmt.Errorf("%s 不在 Orca 仓库清单", abs)
	}
	facts := onboardFacts{Repo: abs, Remote: remote, Row: rowAbsent}
	for _, row := range rows {
		if row.Remote != remote {
			continue
		}
		facts.Row = rowDisabled
		if row.Enabled {
			facts.Row = rowEnabled
		}
	}
	if facts.Installed, err = mergedAgentGraphInstalled(ctx, abs); err != nil {
		return onboardFacts{}, err
	}
	if facts.PR, err = readOnboardPR(ctx, abs, remote); err != nil {
		return onboardFacts{}, err
	}
	probe, err := probeScene(ctx, o, abs, remote)
	if err != nil {
		return onboardFacts{}, err
	}
	facts.Scene = classifyScene(probe)
	if facts.Scene == sceneResidue {
		facts.Residue = sceneResidueDetail(probe)
	}
	if lock, err := flock.TryLock(filepath.Join(o.stateRoot, "onboard", onboardKey(remote)+".lock")); err == nil {
		_ = lock.Close()
	} else if errors.Is(err, syscall.EWOULDBLOCK) {
		facts.Busy = true
	} else {
		return onboardFacts{}, fmt.Errorf("接入锁不可用：%w", err)
	}
	return facts, nil
}

// probeScene reads the raw scene facts for repo: whether the onboarding
// worktree path exists, whether it is this machine's own worktree on the
// onboarding branch of this repository, and whether the branch exists
// locally and on origin. A path git cannot read as a worktree (a stale
// directory left after its repository was re-cloned) is simply not ours,
// so it classifies as residue instead of failing the probe.
func probeScene(ctx context.Context, o onboarder, repo, remote string) (sceneProbe, error) {
	var p sceneProbe
	worktree := onboardWorktree(o.cacheDir, remote)
	if _, err := os.Stat(worktree); err == nil {
		p.PathExists = true
	}
	if p.PathExists {
		branch, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", worktree, "rev-parse", "--abbrev-ref", "HEAD"}, Timeout: 10 * time.Second})
		p.OurWorktree = err == nil && strings.TrimSpace(string(branch)) == onboardBranch && sameGitCommonDir(ctx, worktree, repo)
	}
	if _, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "show-ref", "--quiet", "refs/heads/" + onboardBranch}, Timeout: 10 * time.Second}); err == nil {
		p.LocalBranch = true
	} else if exitCode(err) != 1 {
		return p, fmt.Errorf("git show-ref 失败：%w", err)
	}
	out, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", repo, "ls-remote", "--heads", "origin", onboardBranch}, Timeout: 30 * time.Second})
	if err != nil {
		return p, fmt.Errorf("git ls-remote origin 失败：%w", err)
	}
	p.RemoteBranch = strings.TrimSpace(string(out)) != ""
	return p, nil
}

// sameGitCommonDir reports whether worktree and repo share one git directory
// (a linked worktree's common dir is the main repository's); either side
// unreadable by git means they do not.
func sameGitCommonDir(ctx context.Context, worktree, repo string) bool {
	read := func(dir string) (string, bool) {
		out, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir"}, Timeout: 10 * time.Second})
		if err != nil {
			return "", false
		}
		path := filepath.Clean(strings.TrimSpace(string(out)))
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved, true
		}
		return path, true
	}
	a, okA := read(worktree)
	b, okB := read(repo)
	return okA && okB && a == b
}

// readOnboardPR reads the onboarding PR back from gh — the only PR fact the
// wizard trusts, since the agent's self-report is not evidence:
// `gh pr list --head <branch> --state all --json syncPRFields --limit 100`,
// keeping PRs from the repository itself whose head is the onboarding branch
// (the same fromRepo judgment core sync uses, OF-53/OF-159). State priority
// OPEN > MERGED > CLOSED; within a state the highest number wins.
func readOnboardPR(ctx context.Context, repo, remote string) (onboardPR, error) {
	target, err := parseGitHubRepo(remote)
	if err != nil {
		return onboardPR{}, err
	}
	out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "list", "--head", onboardBranch, "--state", "all", "--json", syncPRFields, "--limit", "100"}, Dir: repo, Timeout: 30 * time.Second})
	if err != nil {
		return onboardPR{}, fmt.Errorf("gh pr list --head %s 失败：%w", onboardBranch, err)
	}
	var prs []syncPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return onboardPR{}, fmt.Errorf("gh pr list --head %s 输出不可解析：%w", onboardBranch, err)
	}
	best := onboardPR{}
	for _, pr := range prs {
		state, ok := prStateByName[pr.State]
		if !ok || !pr.fromRepo(target) || pr.HeadRefName != onboardBranch {
			continue
		}
		if prStateRank[state] > prStateRank[best.State] || (state == best.State && pr.Number > best.Number) {
			best = onboardPR{State: state, Number: pr.Number}
		}
	}
	return best, nil
}
