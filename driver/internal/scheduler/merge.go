package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

func readMergeHandoff(t *Ticket, comments []linear.Comment) {
	rows := append([]linear.Comment(nil), comments...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	seen := map[string]string{}
	for _, c := range rows {
		for _, line := range strings.Split(c.Body, "\n") {
			if strings.HasPrefix(line, merge.ProgressPrefix) {
				var p merge.Progress
				if json.Unmarshal([]byte(strings.TrimPrefix(line, merge.ProgressPrefix)), &p) != nil {
					t.MergeProblem = "invalid merge progress"
				} else {
					if !deploymentTransition(t.Progress, &p) || (t.deploymentReservedQueue == p.QueueID && t.Progress != nil && t.Progress.Deployment == nil && p.Deployment != nil) {
						t.MergeProblem = "conflicting deployment progress"
					}
					if p.Phase == "merge-intent" || p.Phase == "merged" {
						t.deploymentReservedQueue = p.QueueID
					}
					t.Progress = &p
				}
			}
			if strings.HasPrefix(line, merge.HandoffPrefix) {
				raw := strings.TrimPrefix(line, merge.HandoffPrefix)
				var h merge.Handoff
				if json.Unmarshal([]byte(raw), &h) != nil {
					t.MergeProblem = "invalid handoff"
					continue
				}
				if old, ok := seen[h.QueueID]; ok && old != raw {
					t.MergeProblem = "conflicting handoff"
				}
				seen[h.QueueID] = raw
				if old := t.Progress; old != nil && old.QueueID != h.QueueID && old.Phase == "correction" && unmergedDeploymentDecision(old) && deploymentIdentity(*t) == nil {
					// The new validated handoff supersedes an unmerged correction, not a deployment.
					t.Progress = nil
				}
				t.Handoff = &h
			}
		}
	}
	if t.Progress != nil && t.Progress.Deployment != nil {
		if err := deploymentIdentity(*t); err != nil {
			t.MergeProblem = err.Error()
		}
	}
}

// AdvanceMerges reconstructs FIFO from external frozen evidence each tick. It
// attempts only the oldest ticket per canonical repository, retaining ownership
// until both the merged receipt and Done write are confirmed.
func (r *Runtime) AdvanceMerges(ctx context.Context, tickets []Ticket) error {
	groups := map[string][]Ticket{}
	for _, t := range tickets {
		repo := r.Repositories[t.Repo].Env["AGENT_GH_REPO"]
		groups[repo] = append(groups[repo], t)
	}
	var problems []error
	names := make([]string, 0, len(groups))
	for repo := range groups {
		names = append(names, repo)
	}
	sort.Strings(names)
	for _, repo := range names {
		rows := groups[repo]
		blocked := false
		for _, t := range rows {
			if t.Handoff == nil || t.MergeProblem != "" || t.Handoff.QueueID == "" || t.Handoff.EnqueuedAt.IsZero() {
				problems = append(problems, fmt.Errorf("merge waiting %s: missing or conflicting durable handoff", t.Identifier))
				blocked = true
			}
		}
		if blocked {
			continue
		}
		sort.Slice(rows, func(i, j int) bool {
			a, b := rows[i].Handoff, rows[j].Handoff
			if a.EnqueuedAt.Equal(b.EnqueuedAt) {
				return a.QueueID < b.QueueID
			}
			return a.EnqueuedAt.Before(b.EnqueuedAt)
		})
		if err := r.advanceMerge(ctx, rows[0]); err != nil {
			problems = append(problems, fmt.Errorf("merge waiting %s: %w", rows[0].Identifier, err))
		}
	}
	return errors.Join(problems...)
}
func (r *Runtime) advanceMerge(ctx context.Context, t Ticket) (resultErr error) {
	if err := r.verifyGitHubDecision(ctx, t, false); err != nil {
		return err
	}
	var cleanupErr error
	defer func() { resultErr = errors.Join(resultErr, cleanupErr) }()
	h := t.Handoff
	env := r.Repositories[t.Repo].Env
	if h.Version != 1 || h.TicketID != t.ID || h.Identifier != t.Identifier || h.Repository != env["AGENT_GH_REPO"] || !identifier.MatchString(h.Unit) || h.PR <= 0 || !commitID(h.Head) || !commitID(h.Baseline) || h.GateSpec == "" || h.GateOutput == "" || h.ReviewOutput == "" || h.Run == "" || h.Task == "" || !filepath.IsAbs(h.Worktree) || h.Attempt < 1 || h.Corrections < 0 || h.Infra < 0 || h.CatchupRounds < 0 {
		return errors.New("incomplete validation identity")
	}
	if err := h.Validated(); err != nil {
		return err
	}
	if err := deploymentConfig(env); err != nil {
		return err
	}
	if r.GitHub == nil {
		var err error
		r.GitHub, err = github.New(github.Options{})
		if err != nil {
			return err
		}
	}
	owner, repo, ok := strings.Cut(h.Repository, "/")
	if !ok || owner == "" || repo == "" {
		return errors.New("invalid repository")
	}
	a := &mergeAdapter{runtime: r, ticket: t, owner: owner, repo: repo}
	if p := t.Progress; p != nil && p.QueueID == h.QueueID && p.Phase != "merge-intent" && p.Phase != "merged" && p.Phase != "waiting" && p.Phase != "correction" && p.Phase != "human" {
		return errors.New("unknown merge progress")
	}
	pr, err := a.readPR(ctx)
	if err != nil {
		return err
	}
	sha := ""
	rounds, progressErr := mergeRounds(t)
	if progressErr != nil {
		return progressErr
	}
	if pr.Merged {
		sha = pr.MergeSHA
		if p := t.Progress; p != nil && p.QueueID == h.QueueID {
			if (p.Phase != "merge-intent" && p.Phase != "merged") || p.Version != 1 || p.TicketID != t.ID || p.Repository != h.Repository || p.Head != h.Head || p.PR != h.PR || (!commitID(p.Main) && (!p.MainUnknown || p.Main != "" || p.Phase != "merged")) || (p.MainUnknown && p.Main != "") || (p.MergeSHA != "" && p.MergeSHA != sha) {
				return errors.New("conflicting merge progress")
			}
			a.main = p.Main
		}
	} else {
		channel := merge.New(a, merge.CommandGit{Run: r.Run}, a, nil)
		req := merge.Request{Repository: h.Repository, Path: t.Repo, Number: h.PR, CatchupRounds: rounds}
		if err = channel.Enqueue(req); err != nil {
			return err
		}
		result, err := channel.Next(ctx, h.Repository)
		if errors.Is(err, merge.ErrWorktreeCleanup) {
			cleanupErr = merge.ErrWorktreeCleanup
		}
		if err != nil && (!errors.Is(err, merge.ErrWorktreeCleanup) || (result.Status != merge.Merged && result.Status != merge.Correction && result.Status != merge.Human && result.Status != merge.Waiting)) {
			return err
		}
		if result.Status != merge.Merged {
			return r.mergeDecision(ctx, a.ticket, a.main, result)
		}
		sha = result.MergeSHA
	}
	if err := r.GitHub.UniqueTicketPR(ctx, owner, repo, t.Identifier, h.PR, true); err != nil {
		return err
	}
	if !commitID(sha) {
		return errors.New("merged SHA missing")
	}
	progress := merge.Progress{Version: 1, QueueID: h.QueueID, TicketID: t.ID, Repository: h.Repository, PR: h.PR, Head: h.Head, Main: a.main, MainUnknown: a.main == "", MergeSHA: sha, Phase: "merged", CatchupRounds: rounds, Corrections: h.Corrections, Infra: h.Infra, Attempt: h.Attempt}
	if a.ticket.Progress != nil && a.ticket.Progress.QueueID == h.QueueID {
		retainMergeProgress(&progress, a.ticket.Progress)
	}
	progress.Deployment = deploymentFor(a.ticket, env)
	if progress.Deployment != nil {
		if err := r.advanceDeployment(ctx, t, &progress); err != nil {
			return err
		}
	}
	// Persist the result while retaining merge ownership. A failed Done write
	// leaves the same FIFO head to reconcile from GitHub on the next tick.
	b, _ := json.Marshal(progress)
	if err = r.mergeComment(ctx, t, merge.ProgressPrefix+string(b)); err != nil {
		return err
	}
	issue, err := r.Clients[t.Repo].UpdateIssue(ctx, t.ID, "Done", nil, []string{"auto-impl", "ready-for-agent"})
	if err != nil || issue.State.Name != "Done" {
		return errors.New("completion write unconfirmed")
	}
	// A failed final comment must not suppress cleanup after confirmed Done.
	defer r.cleanupMerged(ctx, t)
	return r.mergeComment(ctx, t, fmt.Sprintf("已合并: PR #%d squash %s\n阶段: %s\n归属: %s\n%s%s", h.PR, sha, machine.PhaseMerged, machine.OwnerNone, merge.ProgressPrefix, b))
}
func (r *Runtime) mergeComment(ctx context.Context, t Ticket, body string) error {
	client := r.Clients[t.Repo]
	rows, err := client.ListComments(ctx, t.ID)
	if err != nil {
		return errors.New("merge receipt read failed")
	}
	for _, c := range rows {
		if c.Body == body {
			return nil
		}
	}
	if _, err = client.CreateComment(ctx, t.ID, body); err == nil {
		return nil
	}
	rows, readErr := client.ListComments(ctx, t.ID)
	if readErr == nil {
		for _, c := range rows {
			if c.Body == body {
				return nil
			}
		}
	}
	return errors.New("merge receipt write unconfirmed")
}

type mergeAdapter struct {
	runtime           *Runtime
	ticket            Ticket
	owner, repo, main string
}

func (a *mergeAdapter) readPR(ctx context.Context) (github.PullRequest, error) {
	h := a.ticket.Handoff
	p, err := a.runtime.GitHub.GetPR(ctx, a.owner, a.repo, h.PR)
	if err != nil {
		return p, err
	}
	if p.Number != h.PR || p.Head.SHA != h.Head || p.Head.Ref != "agent/"+h.Identifier+"/"+h.Unit || p.Head.Repo.FullName != h.Repository || p.Base.Ref != "main" || p.Base.Repo.FullName != h.Repository {
		return p, errors.New("PR identity changed")
	}
	return p, nil
}
func (a *mergeAdapter) Snapshot(ctx context.Context, _ merge.Request) (merge.Snapshot, error) {
	p, err := a.readPR(ctx)
	if err != nil {
		return merge.Snapshot{}, err
	}
	if p.Merged || p.State != "open" || p.Draft == nil || *p.Draft || p.Mergeable == nil {
		return merge.Snapshot{}, errors.New("PR not explicitly mergeable")
	}
	if err := a.runtime.GitHub.UniqueTicketPR(ctx, a.owner, a.repo, a.ticket.Identifier, a.ticket.Handoff.PR, false); err != nil {
		return merge.Snapshot{}, err
	}
	main, err := a.runtime.GitHub.MainSHA(ctx, a.owner, a.repo)
	if err != nil || !commitID(main) {
		return merge.Snapshot{}, errors.New("main unavailable")
	}
	a.main = main
	run := func(args ...string) ([]byte, error) { return a.runtime.command(ctx, a.ticket.Repo, "git", args...) }
	if _, err = run("fetch", "--no-tags", "origin", p.Head.SHA, main); err != nil {
		return merge.Snapshot{}, errors.New("fixed commits unavailable")
	}
	raw, err := run("merge-base", p.Head.SHA, main)
	base := strings.TrimSpace(string(raw))
	if err != nil || !commitID(base) {
		return merge.Snapshot{}, errors.New("merge base unavailable")
	}
	paths := func(head string) ([]string, error) {
		b, e := run("diff", "--name-only", "--no-renames", "-z", base, head, "--")
		if e != nil {
			return nil, e
		}
		if len(b) > 0 && b[len(b)-1] != 0 {
			return nil, errors.New("incomplete paths")
		}
		return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), nil
	}
	prFiles, err := paths(p.Head.SHA)
	if err != nil {
		return merge.Snapshot{}, err
	}
	mainFiles, err := paths(main)
	if err != nil {
		return merge.Snapshot{}, err
	}
	if err := a.policy(prFiles); err != nil {
		return merge.Snapshot{}, err
	}
	green, err := a.runtime.GitHub.MergeChecks(ctx, a.owner, a.repo, p.Head.SHA)
	if err != nil {
		return merge.Snapshot{}, err
	}
	if !*p.Mergeable {
		p.MergeableState = "dirty"
	}
	return merge.Snapshot{Head: p.Head.SHA, Main: main, CIHead: p.Head.SHA, CIGreen: green, MergeableState: p.MergeableState, Behind: base != main, PRFiles: prFiles, MainFiles: mainFiles}, nil
}
func (a *mergeAdapter) Squash(ctx context.Context, req merge.Request, head, main string) (string, error) {
	fresh, err := a.Snapshot(ctx, req)
	if err != nil {
		return "", err
	}
	if fresh.Main != main {
		return "", merge.ErrMainAdvanced
	}
	if fresh.Head != head || !fresh.CIGreen || fresh.MergeableState != "clean" {
		return "", errors.New("validation changed")
	}
	h := a.ticket.Handoff
	intent := merge.Progress{Version: 1, QueueID: h.QueueID, TicketID: h.TicketID, Repository: h.Repository, PR: h.PR, Head: head, Main: main, Phase: "merge-intent", CatchupRounds: req.CatchupRounds, Corrections: h.Corrections, Infra: h.Infra, Attempt: h.Attempt}
	if a.ticket.Progress != nil && a.ticket.Progress.QueueID == h.QueueID {
		retainMergeProgress(&intent, a.ticket.Progress)
	}
	intent.Deployment = deploymentFor(a.ticket, a.runtime.Repositories[a.ticket.Repo].Env)
	raw, _ := json.Marshal(intent)
	if err := a.runtime.mergeComment(ctx, a.ticket, merge.ProgressPrefix+string(raw)); err != nil {
		return "", err
	}
	a.ticket.Progress = &intent
	// The durable write can take time; reread all pinned evidence afterwards.
	fresh, err = a.Snapshot(ctx, req)
	if err != nil {
		return "", err
	}
	if fresh.Main != main {
		return "", merge.ErrMainAdvanced
	}
	if fresh.Head != head || !fresh.CIGreen || fresh.MergeableState != "clean" {
		return "", errors.New("validation changed after intent")
	}
	p, err := a.runtime.GitHub.MergePR(ctx, a.owner, a.repo, req.Number, head)
	if err != nil {
		return "", err
	}
	if p.Head.SHA != head {
		return "", errors.New("merged head changed")
	}
	return p.MergeSHA, nil
}

func (a *mergeAdapter) policy(files []string) error {
	env := a.runtime.Repositories[a.ticket.Repo].Env
	lane := a.ticket.Handoff.Lane
	if lane == 2 && env["AGENT_LANE2_AUTOMERGE"] != "1" {
		return errors.New("lane 2 requires manual approval")
	}
	for _, key := range []string{"AGENT_LANE3_GLOBS", "AGENT_LANE2_MANUAL_GLOBS", "AGENT_LANE2_GLOBS"} {
		for _, pattern := range strings.Fields(env[key]) {
			if _, err := path.Match(pattern, ""); err != nil {
				return errors.New("invalid merge policy glob")
			}
			for _, file := range files {
				if scopeMatch(pattern, file) {
					if key != "AGENT_LANE2_GLOBS" || lane != 2 {
						return errors.New("changed path requires manual approval or new lane validation")
					}
				}
			}
		}
	}
	return nil
}
