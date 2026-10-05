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
	"github.com/vmxmy/orca-factory/driver/internal/release"
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

// Lane persists the shared arrival order and retries its head until completion.
type Lane struct {
	Repositories map[string]config.Repository
	GitHub       GitHub
	Orca         Orca
	Tickets      Tickets
	Exec         func(context.Context, runx.Spec) ([]byte, error)
	Root         string
	Now          func() time.Time
	Warn         func(format string, args ...any)
	Publisher    release.Publisher
	Allowed      func(string) bool
	entries      []laneEntry
	loaded       bool
	current      map[string]scheduler.Ticket
	pending      []laneEntry
	channels     map[string]*merge.Channel
}

// compareFileLimit is GitHub's silent cap on compare file lists.
const compareFileLimit = 300

// Admit records new feature PRs without advancing the lane. Existing entries
// retain their position even when Linear changes snapshot order.
func eligible(t scheduler.Ticket) bool {
	return t.Owner == machine.OwnerMerge && t.Phase == machine.PhaseMergeQueue && !t.Blocked
}

func (l *Lane) Admit(ctx context.Context, queue []scheduler.Ticket) error {
	l.current = map[string]scheduler.Ticket{}
	if err := l.load(); err != nil {
		return err
	}
	if err := l.flushPending(); err != nil {
		return err
	}
	entries := append([]laneEntry(nil), l.entries...)
	current := map[string]scheduler.Ticket{}
	var problems []error
	for _, t := range queue {
		if !eligible(t) {
			continue
		}
		full, owner, name, err := l.repository(t)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		pr, err := l.pullRequest(ctx, owner, name, t.Identifier)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if pr == nil || (!pr.Merged && pr.State != "open") {
			reason := "合并通道: 找不到 agent/" + t.Identifier + "/executor 的 PR"
			if pr != nil {
				reason = fmt.Sprintf("合并通道: PR #%d 未合并即关闭", pr.Number)
			}
			if err := l.escalate(ctx, t, reason); err != nil {
				problems = append(problems, err)
			}
			// An existing feature remains a barrier, but has no current authority.
			continue
		}
		env := l.Repositories[t.Repo].Env
		r := merge.Request{Repository: full, Path: t.Repo, Number: pr.Number, DeployWorkflow: env["AGENT_DEPLOY_WORKFLOW"], SmokeCommand: env["AGENT_SMOKE_CMD"]}
		found := false
		for i := range entries {
			e := &entries[i]
			if e.Ticket != nil && e.Ticket.ID == t.ID && e.Request.Path == t.Repo {
				// Recovery must retain its arrival position and catch-up budget.
				r.CatchupRounds = e.Request.CatchupRounds
				if e.Blocked == "" {
					e.Ticket = &t
				}
				e.Request = r
				found = true
				break
			}
		}
		if !found {
			entries = append(entries, laneEntry{Request: r, Ticket: &t})
		}
		current[requestKey(full, pr.Number)] = t
	}
	if err := l.accept(entries); err != nil {
		return errors.Join(append(problems, err)...)
	}
	l.current = current
	return errors.Join(problems...)
}
func (l *Lane) Process(ctx context.Context, queue []scheduler.Ticket) error {
	if err := l.Admit(ctx, queue); err != nil {
		return err
	}
	return l.Advance(ctx)
}

func (l *Lane) channel(repo string) *merge.Channel {
	if l.channels == nil {
		l.channels = map[string]*merge.Channel{}
	}
	if l.channels[repo] == nil {
		l.channels[repo] = merge.New(l, merge.CommandGit{Run: l.Exec}, l, l)
	}
	return l.channels[repo]
}

// Advance uses only the current successful admission and the durable FIFO.
// Restoring entries alone never restores a feature ticket's write authority.
func (l *Lane) Advance(ctx context.Context) error {
	if err := l.load(); err != nil {
		return err
	}
	if err := l.flushPending(); err != nil {
		return err
	}
	if err := l.save(); err != nil {
		return err
	}
	seen := map[string]bool{}
	var problems []error
	for i := 0; i < len(l.entries); {
		e := &l.entries[i]
		r := e.Request
		if (l.Allowed != nil && !l.Allowed(r.Path)) || seen[r.Repository] {
			i++
			continue
		}
		seen[r.Repository] = true
		t, authorized := l.current[requestKey(r.Repository, r.Number)]
		if e.Ticket != nil && (!authorized || !eligible(t)) {
			i++
			continue
		}
		owner, name, _ := strings.Cut(r.Repository, "/")
		pr, err := l.GitHub.GetPR(ctx, owner, name, r.Number)
		if err != nil {
			problems = append(problems, err)
			i++
			continue
		}
		if e.Blocked != "" {
			// A fresh handoff plus a changed head can resume a correction. Human
			// deployment recovery stays paused until an operator repairs its record.
			fresh := e.Ticket != nil && (t.Attempt > e.Ticket.Attempt || t.UpdatedAt.After(e.Ticket.UpdatedAt))
			if e.MergeSHA == "" && fresh && pr.Head.SHA != "" && pr.Head.SHA != e.BlockedHead && r.CatchupRounds < 2 {
				e.Blocked, e.BlockedHead, e.Escalated = "", "", false
				e.Ticket = &t
				if err := l.save(); err != nil {
					return errors.Join(append(problems, err)...)
				}
			} else {
				if err := l.notifyBlocked(ctx, e); err != nil {
					problems = append(problems, err)
				}
				i++
				continue
			}
		}
		if e.Ticket == nil && e.Release == nil {
			problems = append(problems, errors.New("merge entry has no owner"))
			i++
			continue
		}
		channel := l.channel(r.Repository)
		result := merge.Result{Request: r, Status: merge.Merged, MergeSHA: e.MergeSHA}
		if e.MergeSHA == "" {
			if pr.Merged {
				result.MergeSHA = pr.MergeCommitSHA
				// An externally confirmed merge supersedes a queued attempt, but
				// its deployment barrier remains in the durable head below.
				l.channels[r.Repository] = merge.New(l, merge.CommandGit{Run: l.Exec}, l, l)
			} else {
				if pr.State != "open" {
					problems = append(problems, fmt.Errorf("PR #%d closed without merge", r.Number))
					i++
					continue
				}
				if err = channel.Enqueue(r); err == nil {
					result, err = channel.Next(ctx, r.Repository)
				}
			}
			if err != nil {
				problems = append(problems, err)
				if result.Status != merge.Merged {
					i++
					continue
				}
			}
			e.Request.CatchupRounds = result.Rounds
			switch result.Status {
			case merge.Correction, merge.Human:
				e.Blocked, e.BlockedHead = "合并通道: "+result.Reason, pr.Head.SHA
				if err := l.save(); err != nil {
					return errors.Join(append(problems, err)...)
				}
				if err := l.notifyBlocked(ctx, e); err != nil {
					problems = append(problems, err)
				}
				i++
				continue
			case merge.Merged:
				if result.MergeSHA == "" {
					problems = append(problems, errors.New("squash commit missing"))
					i++
					continue
				}
				e.MergeSHA = result.MergeSHA
				if err := l.save(); err != nil {
					return errors.Join(append(problems, err)...)
				}
				if r.DeployWorkflow != "" || r.SmokeCommand != "" {
					i++
					continue
				}
			default:
				if err := l.save(); err != nil {
					return errors.Join(append(problems, err)...)
				}
				i++
				continue
			}
		}
		if (r.DeployWorkflow != "" || r.SmokeCommand != "") && !e.DeploymentReady {
			// The lifetime Channel owns normal waits. After restart it has no
			// previous squash, so recover that wait from the durable head.
			waiting, err := channel.Next(ctx, r.Repository)
			if err != nil {
				problems = append(problems, err)
				i++
				continue
			}
			state := merge.DeploymentPending
			switch waiting.Status {
			case merge.Empty:
				state, err = l.Check(ctx, r, e.MergeSHA)
			case merge.Human, merge.Paused:
				state = merge.DeploymentFailed
			}
			if err != nil {
				problems = append(problems, err)
				i++
				continue
			}
			if state == merge.DeploymentFailed || state == merge.SmokeFailed {
				e.Blocked = "合并通道: deployment requires human recovery: " + string(state)
				if err := l.save(); err != nil {
					return errors.Join(append(problems, err)...)
				}
				if err := l.notifyBlocked(ctx, e); err != nil {
					problems = append(problems, err)
				}
				i++
				continue
			}
			if state != merge.DeploymentPassed {
				i++
				continue
			}
			e.DeploymentReady = true
			if err := l.save(); err != nil {
				return errors.Join(append(problems, err)...)
			}
		}
		result = merge.Result{Request: r, Status: merge.Merged, MergeSHA: e.MergeSHA}
		if e.Release != nil {
			err = release.Publish(ctx, l.Publisher, *e.Release, result)
		} else {
			err = l.finish(ctx, t, owner, name, r.Number, e.MergeSHA)
		}
		if err != nil {
			problems = append(problems, err)
			i++
			continue
		}
		entries := append(append([]laneEntry(nil), l.entries[:i]...), l.entries[i+1:]...)
		if err := l.accept(entries); err != nil {
			return errors.Join(append(problems, err)...)
		}
	}
	return errors.Join(problems...)
}

func (l *Lane) notifyBlocked(ctx context.Context, e *laneEntry) error {
	if e.Escalated || e.Ticket == nil {
		return nil
	}
	if err := l.escalate(ctx, *e.Ticket, e.Blocked); err != nil {
		return err
	}
	e.Escalated = true
	return l.save()
}

func requestKey(full string, number int) string { return fmt.Sprintf("%s#%d", full, number) }
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
	var spec string
	if t, ok := l.current[requestKey(r.Repository, r.Number)]; ok {
		_, name, _ := strings.Cut(r.Repository, "/")
		spec = filepath.Join(l.Root, name, t.Identifier, "gate.spec")
		if _, err := os.Stat(spec); err != nil {
			return fmt.Errorf("gate spec unavailable: %w", err)
		}
	} else {
		var plan *release.Plan
		for _, e := range l.entries {
			if e.Request.Repository == r.Repository && e.Request.Number == r.Number && e.Release != nil {
				plan = &e.Release.Plan
				break
			}
		}
		if plan == nil {
			return errors.New("merge gate has no owner")
		}
		// Run the repository's full project gate against the merged tree with the
		// release's exact two-file scope, just as feature entries use their spec.
		root := filepath.Join(r.Path, ".omc", "tmp")
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
		f, err := os.CreateTemp(root, "release-gate-*")
		if err != nil {
			return err
		}
		spec = f.Name()
		defer os.Remove(spec)
		body := fmt.Sprintf("UNIT: release-%s\nSCOPE: mods/%s/.claude-plugin/plugin.json mods/%s/CHANGELOG.md\nCHECK: git diff --check\nCHECK_IS_NEW_TEST: no\n", plan.Plugin, plan.Plugin, plan.Plugin)
		if _, err = f.WriteString(body); err != nil {
			f.Close()
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
	}
	run := l.Exec
	if run == nil {
		run = runx.Run
	}
	_, err := run(ctx, runx.Spec{Path: filepath.Join(r.Path, "scripts", "agent-gates", "gate.sh"), Args: []string{path, spec}, Dir: r.Path, Timeout: 45 * time.Minute})
	return err
}
