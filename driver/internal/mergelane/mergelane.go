// Package mergelane connects the scheduler's merge queue to merge.Channel and
// finishes merged tickets: Linear Done, worker release, board status, branch.
package mergelane

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
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type GitHub interface {
	FindPRs(ctx context.Context, owner, repo, head string) ([]github.PullRequest, error)
	GetPR(ctx context.Context, owner, repo string, number int) (github.PullRequest, error)
	BranchSHA(ctx context.Context, owner, repo, branch string) (string, error)
	Compare(ctx context.Context, owner, repo, base, head string) (github.Comparison, error)
	CheckRuns(ctx context.Context, owner, repo, ref string) ([]github.CheckRun, error)
	MergePR(ctx context.Context, owner, repo string, number int, sha string) (github.PullRequest, error)
}
type Orca interface {
	WorkerList(context.Context, orca.ListOptions) (orca.Receipt, error)
	WorkerRelease(context.Context, string, string) (orca.Receipt, error)
	WorktreeSet(context.Context, string, string, string, string) (orca.Receipt, error)
}

// Tickets is the scheduler side the lane writes back to.
type Tickets interface {
	Apply(context.Context, scheduler.Ticket, scheduler.Action) error
	Finish(ctx context.Context, t scheduler.Ticket, body string) error
}

// Lane rebuilds a fresh FIFO channel from every tick's queue: ownership lives in
// Linear, so the queue order is the snapshot order and only catch-up rounds carry over.
type Lane struct {
	Repositories map[string]config.Repository
	GitHub       GitHub
	Orca         Orca
	Tickets      Tickets
	Exec         func(context.Context, runx.Spec) ([]byte, error)
	Root         string
	Now          func() time.Time
	Warn         func(format string, args ...any)
	rounds       map[string]int
	current      map[string]scheduler.Ticket
}

// compareFileLimit is GitHub's silent cap on compare file lists.
const compareFileLimit = 300

func (l *Lane) Process(ctx context.Context, queue []scheduler.Ticket) error {
	if l.rounds == nil {
		l.rounds = map[string]int{}
	}
	l.current = map[string]scheduler.Ticket{}
	channel := merge.New(l, merge.CommandGit{Run: l.Exec}, l, l)
	var problems []error
	var repos []string
	for _, t := range queue {
		full, owner, name, err := l.repository(t)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		pr, err := l.pullRequest(ctx, owner, name, t.Identifier)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s PR lookup: %w", t.Identifier, err))
			continue
		}
		switch {
		case pr == nil:
			problems = append(problems, l.escalate(ctx, t, "合并通道: 找不到 agent/"+t.Identifier+"/executor 的 PR"))
			continue
		case pr.Merged:
			problems = append(problems, l.finish(ctx, t, owner, name, pr.Number, pr.MergeCommitSHA))
			continue
		case pr.State != "open":
			problems = append(problems, l.escalate(ctx, t, fmt.Sprintf("合并通道: PR #%d 未合并即关闭", pr.Number)))
			continue
		}
		env := l.Repositories[t.Repo].Env
		if env["AGENT_DEPLOY_WORKFLOW"] != "" || env["AGENT_SMOKE_CMD"] != "" {
			problems = append(problems, fmt.Errorf("%s: deployment-gated merges are not implemented; repository lane refused", t.Identifier))
			continue
		}
		key := requestKey(full, pr.Number)
		l.current[key] = t
		if err := channel.Enqueue(merge.Request{Repository: full, Path: t.Repo, Number: pr.Number, CatchupRounds: l.rounds[key]}); err != nil {
			problems = append(problems, err)
			continue
		}
		if !contains(repos, full) {
			repos = append(repos, full)
		}
	}
	for _, full := range repos {
		result, err := channel.Next(ctx, full)
		if err != nil && result.Status != merge.Merged {
			problems = append(problems, fmt.Errorf("%s merge: %w", full, err))
			continue
		}
		key := requestKey(full, result.Request.Number)
		t, ok := l.current[key]
		if !ok {
			continue
		}
		l.rounds[key] = result.Rounds
		_, owner, name, _ := l.repository(t)
		switch result.Status {
		case merge.Merged:
			delete(l.rounds, key)
			problems = append(problems, l.finish(ctx, t, owner, name, result.Request.Number, result.MergeSHA))
		case merge.Correction:
			// A text conflict (or a red gate on the merged tree) goes back to the
			// executor as a merge-conflict resolver, not to a person.
			delete(l.rounds, key)
			problems = append(problems, l.resume(ctx, t, result.Rounds))
		case merge.Human:
			delete(l.rounds, key)
			problems = append(problems, l.escalate(ctx, t, "合并通道: "+result.Reason))
		}
	}
	return errors.Join(problems...)
}

func requestKey(full string, number int) string { return fmt.Sprintf("%s#%d", full, number) }
func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (l *Lane) repository(t scheduler.Ticket) (full, owner, name string, err error) {
	full = l.Repositories[t.Repo].Env["AGENT_GH_REPO"]
	owner, name, ok := strings.Cut(full, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", "", fmt.Errorf("%s: invalid AGENT_GH_REPO", t.Identifier)
	}
	return full, owner, name, nil
}

// pullRequest prefers a merged PR, then an open one, so a merge confirmed in an
// earlier tick but not yet finished in Linear is finished instead of escalated.
func (l *Lane) pullRequest(ctx context.Context, owner, name, ticket string) (*github.PullRequest, error) {
	prs, err := l.GitHub.FindPRs(ctx, owner, name, "agent/"+ticket+"/executor")
	if err != nil {
		return nil, err
	}
	var open, closed *github.PullRequest
	for i := range prs {
		pr := &prs[i]
		switch {
		case pr.Merged:
			return pr, nil
		case pr.State == "open" && open == nil:
			open = pr
		case closed == nil:
			closed = pr
		}
	}
	if open != nil {
		return open, nil
	}
	if closed != nil {
		// The list endpoint omits merged; read it back before treating closed as unmerged.
		full, err := l.GitHub.GetPR(ctx, owner, name, closed.Number)
		if err != nil {
			return nil, err
		}
		return &full, nil
	}
	return nil, nil
}

// resume hands a conflicted PR back to the executor as a merge resolver.
func (l *Lane) resume(ctx context.Context, t scheduler.Ticket, rounds int) error {
	if err := l.Tickets.Apply(ctx, t, scheduler.Action{Kind: scheduler.Resume, Attempt: t.Attempt, Phase: machine.PhaseCorrection}); err != nil {
		return fmt.Errorf("%s merge resume: %w", t.Identifier, err)
	}
	return nil
}

func (l *Lane) escalate(ctx context.Context, t scheduler.Ticket, reason string) error {
	if err := l.Tickets.Apply(ctx, t, scheduler.Action{Kind: scheduler.Human, Attempt: t.Attempt, Reason: reason}); err != nil {
		return fmt.Errorf("%s escalation: %w", t.Identifier, err)
	}
	return nil
}

// finish marks Done before anything else: a Done ticket leaves the snapshot, and
// a failure before Done is retried next tick through the merged-PR lookup.
func (l *Lane) finish(ctx context.Context, t scheduler.Ticket, owner, name string, number int, sha string) error {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	body := fmt.Sprintf("已合并: PR #%d squash %s\n阶段: 已合并\n归属: 无 since=%s", number, sha, now().UTC().Format(time.RFC3339))
	if err := l.Tickets.Finish(ctx, t, body); err != nil {
		return fmt.Errorf("%s finish: %w", t.Identifier, err)
	}
	l.cleanup(ctx, t, name, number)
	return nil
}

// cleanup is best effort: the merge is complete, so each failure only warns.
func (l *Lane) cleanup(ctx context.Context, t scheduler.Ticket, name string, number int) {
	var cp struct{ Run, Worktree string }
	raw, err := os.ReadFile(filepath.Join(l.Root, name, t.Identifier, "executor.json"))
	if err == nil {
		err = json.Unmarshal(raw, &cp)
	}
	if err != nil {
		l.warn("%s cleanup: executor checkpoint unreadable: %v", t.Identifier, err)
	}
	if cp.Run != "" {
		l.releaseWorkers(ctx, t, cp.Run)
	}
	if filepath.IsAbs(cp.Worktree) {
		if _, err := l.Orca.WorktreeSet(ctx, cp.Worktree, t.Identifier, "completed", fmt.Sprintf("driver: 已合并 #%d", number)); err != nil {
			l.warn("%s cleanup: board status: %v", t.Identifier, err)
		}
	}
	branch := "refs/heads/agent/" + t.Identifier + "/executor"
	out, err := l.run(ctx, t.Repo, "git", "ls-remote", "--heads", "origin", branch)
	if err != nil {
		l.warn("%s cleanup: branch lookup: %v", t.Identifier, err)
		return
	}
	if strings.TrimSpace(string(out)) == "" {
		return
	}
	if _, err := l.run(ctx, t.Repo, "git", "push", "origin", "--delete", branch); err != nil {
		l.warn("%s cleanup: branch delete: %v", t.Identifier, err)
	}
}

func (l *Lane) releaseWorkers(ctx context.Context, t scheduler.Ticket, run string) {
	receipt, err := l.Orca.WorkerList(ctx, orca.ListOptions{Run: run, Limit: 100, IncludeRemote: true})
	if err != nil {
		l.warn("%s cleanup: worker list: %v", t.Identifier, err)
		return
	}
	var workers []struct {
		Dispatch      string `json:"dispatchId"`
		Status        string `json:"dispatchStatus"`
		TerminalState string `json:"terminalState"`
	}
	if err := json.Unmarshal(receipt["workers"], &workers); err != nil {
		l.warn("%s cleanup: worker list unreadable", t.Identifier)
		return
	}
	for _, w := range workers {
		if w.TerminalState == "released" || (w.Status != "completed" && w.Status != "failed") {
			continue
		}
		request := orca.RequestID(t.Identifier, "merge-lane", "release-"+w.Dispatch, 1)
		if _, err := l.Orca.WorkerRelease(ctx, request, w.Dispatch); err != nil {
			l.warn("%s cleanup: release %s: %v", t.Identifier, w.Dispatch, err)
		}
	}
}

func (l *Lane) run(ctx context.Context, dir, path string, args ...string) ([]byte, error) {
	run := l.Exec
	if run == nil {
		run = runx.Run
	}
	return run(ctx, runx.Spec{Path: path, Args: args, Dir: dir, Timeout: 2 * time.Minute})
}

func (l *Lane) warn(format string, args ...any) {
	if l.Warn != nil {
		l.Warn(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...)
}

// Snapshot implements merge.GitHub.
func (l *Lane) Snapshot(ctx context.Context, r merge.Request) (merge.Snapshot, error) {
	owner, name, _ := strings.Cut(r.Repository, "/")
	pr, err := l.GitHub.GetPR(ctx, owner, name, r.Number)
	if err != nil {
		return merge.Snapshot{}, err
	}
	if pr.State != "open" || pr.Merged || pr.Head.SHA == "" || pr.Base.Ref == "" {
		return merge.Snapshot{}, errors.New("PR is not an open mergeable request")
	}
	main, err := l.GitHub.BranchSHA(ctx, owner, name, pr.Base.Ref)
	if err != nil {
		return merge.Snapshot{}, err
	}
	head := pr.Head.SHA
	side, err := l.GitHub.Compare(ctx, owner, name, main, head)
	if err != nil {
		return merge.Snapshot{}, err
	}
	if len(side.Files) >= compareFileLimit {
		return merge.Snapshot{}, errors.New("PR file list may be truncated")
	}
	s := merge.Snapshot{Head: head, Main: main, CIHead: head, MergeableState: pr.MergeableState, Behind: side.BehindBy > 0, PRFiles: paths(side.Files)}
	if s.Behind {
		mainSide, err := l.GitHub.Compare(ctx, owner, name, side.MergeBaseCommit.SHA, main)
		if err != nil {
			return merge.Snapshot{}, err
		}
		if len(mainSide.Files) >= compareFileLimit {
			return merge.Snapshot{}, errors.New("main file list may be truncated")
		}
		s.MainFiles = paths(mainSide.Files)
	}
	runs, err := l.GitHub.CheckRuns(ctx, owner, name, head)
	if err != nil {
		return merge.Snapshot{}, err
	}
	s.CIGreen = ciGreen(runs, head)
	return s, nil
}

func paths(files []github.File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Filename)
		if f.PreviousFilename != "" {
			out = append(out, f.PreviousFilename)
		}
	}
	return out
}

// ciGreen fails closed: a head with no check runs yet is not green.
func ciGreen(runs []github.CheckRun, head string) bool {
	if len(runs) == 0 {
		return false
	}
	for _, r := range runs {
		if r.HeadSHA != head || r.Status != "completed" {
			return false
		}
		switch r.Conclusion {
		case "success", "neutral", "skipped":
		default:
			return false
		}
	}
	return true
}

// Squash implements merge.GitHub: it refuses when main moved since validation.
func (l *Lane) Squash(ctx context.Context, r merge.Request, head, main string) (string, error) {
	owner, name, _ := strings.Cut(r.Repository, "/")
	pr, err := l.GitHub.GetPR(ctx, owner, name, r.Number)
	if err != nil {
		return "", err
	}
	now, err := l.GitHub.BranchSHA(ctx, owner, name, pr.Base.Ref)
	if err != nil {
		return "", err
	}
	if now != main {
		return "", merge.ErrMainAdvanced
	}
	merged, err := l.GitHub.MergePR(ctx, owner, name, r.Number, head)
	if err != nil {
		return "", err
	}
	return merged.MergeCommitSHA, nil
}

// Run implements merge.Gate with the ticket's executor gate spec.
func (l *Lane) Run(ctx context.Context, r merge.Request, path string) error {
	t, ok := l.current[requestKey(r.Repository, r.Number)]
	if !ok {
		return errors.New("merge gate has no ticket")
	}
	_, name, _ := strings.Cut(r.Repository, "/")
	spec := filepath.Join(l.Root, name, t.Identifier, "gate.spec")
	if _, err := os.Stat(spec); err != nil {
		return fmt.Errorf("gate spec unavailable: %w", err)
	}
	run := l.Exec
	if run == nil {
		run = runx.Run
	}
	_, err := run(ctx, runx.Spec{Path: filepath.Join(r.Path, "scripts", "agent-gates", "gate.sh"), Args: []string{path, spec}, Dir: r.Path, Timeout: 45 * time.Minute})
	return err
}

// Check implements merge.Deployment. Deployment-gated repositories are refused
// before enqueue, so reaching this is a configuration error.
func (l *Lane) Check(context.Context, merge.Request, string) (merge.DeploymentState, error) {
	return "", errors.New("deployment checks are not implemented")
}
