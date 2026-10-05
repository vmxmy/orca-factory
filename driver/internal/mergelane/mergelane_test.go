package mergelane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

const (
	head = "c4e86ec5465d1e2d1f6aea45053e663b54a1350b"
	main = "0ab0047178cd32132001eef0682d50a77a6f2622"
	sha  = "1111111111111111111111111111111111111111"
)

type fakeGitHub struct {
	prs     []github.PullRequest
	runs    []github.CheckRun
	merges  []string
	behind  int
	readErr error
}

func (f *fakeGitHub) FindPRs(context.Context, string, string, string) ([]github.PullRequest, error) {
	return f.prs, f.readErr
}
func (f *fakeGitHub) GetPR(_ context.Context, _, _ string, n int) (github.PullRequest, error) {
	for _, pr := range f.prs {
		if pr.Number == n {
			return pr, nil
		}
	}
	return github.PullRequest{}, os.ErrNotExist
}
func (f *fakeGitHub) BranchSHA(context.Context, string, string, string) (string, error) {
	return main, nil
}
func (f *fakeGitHub) Compare(context.Context, string, string, string, string) (github.Comparison, error) {
	return github.Comparison{BehindBy: f.behind, Files: []github.File{{Filename: "mods/a.ts"}}}, nil
}
func (f *fakeGitHub) CheckRuns(context.Context, string, string, string) ([]github.CheckRun, error) {
	return f.runs, nil
}
func (f *fakeGitHub) MergePR(_ context.Context, _, _ string, n int, pinned string) (github.PullRequest, error) {
	f.merges = append(f.merges, pinned)
	return github.PullRequest{Number: n, Merged: true, MergeCommitSHA: sha}, nil
}

type fakeOrca struct{ released, boards []string }

func (f *fakeOrca) WorkerList(context.Context, orca.ListOptions) (orca.Receipt, error) {
	return orca.Receipt{"workers": json.RawMessage(`[{"dispatchId":"ctx_done","dispatchStatus":"completed","terminalState":"reclaimable"},{"dispatchId":"ctx_gone","dispatchStatus":"failed","terminalState":"released"}]`)}, nil
}
func (f *fakeOrca) WorkerRelease(_ context.Context, _, dispatch string) (orca.Receipt, error) {
	f.released = append(f.released, dispatch)
	return orca.Receipt{}, nil
}
func (f *fakeOrca) WorktreeSet(_ context.Context, path, _, status, _ string) (orca.Receipt, error) {
	f.boards = append(f.boards, status+" "+path)
	return orca.Receipt{}, nil
}

type fakeTickets struct{ finished, escalated []string }

func (f *fakeTickets) Apply(_ context.Context, t scheduler.Ticket, a scheduler.Action) error {
	f.escalated = append(f.escalated, t.Identifier+" "+a.Reason)
	return nil
}
func (f *fakeTickets) Finish(_ context.Context, t scheduler.Ticket, body string) error {
	f.finished = append(f.finished, t.Identifier+" "+body)
	return nil
}

func lane(t *testing.T, gh *fakeGitHub) (*Lane, *fakeOrca, *fakeTickets, *[]string, scheduler.Ticket) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "orca-factory", "ZII-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "executor.json"), []byte(`{"Run":"run_one","Worktree":"/worktrees/ZII-1-executor"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o, tk, git := &fakeOrca{}, &fakeTickets{}, &[]string{}
	l := &Lane{
		Repositories: map[string]config.Repository{"/repo": {Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "vmxmy/orca-factory"}}},
		GitHub:       gh, Orca: o, Tickets: tk, Root: root, Warn: func(string, ...any) {},
		Exec: func(_ context.Context, s runx.Spec) ([]byte, error) {
			*git = append(*git, strings.Join(s.Args, " "))
			if s.Args[0] == "ls-remote" {
				return []byte(head + "\trefs/heads/agent/ZII-1/executor\n"), nil
			}
			return nil, nil
		},
	}
	return l, o, tk, git, scheduler.Ticket{ID: "issue_1", Identifier: "ZII-1", Repo: "/repo", Attempt: 1, Owner: machine.OwnerMerge, Phase: machine.PhaseMergeQueue}
}

func openPR() github.PullRequest {
	pr := github.PullRequest{Number: 7, State: "open", MergeableState: "clean"}
	pr.Head.SHA, pr.Base.Ref = head, "main"
	return pr
}

func green() []github.CheckRun {
	return []github.CheckRun{{Name: "GitGuardian Security Checks", Status: "completed", Conclusion: "success", HeadSHA: head}}
}

func TestGreenPRIsMergedAndFinished(t *testing.T) {
	gh := &fakeGitHub{prs: []github.PullRequest{openPR()}, runs: green()}
	l, o, tk, git, ticket := lane(t, gh)
	if err := l.Process(context.Background(), []scheduler.Ticket{ticket}); err != nil {
		t.Fatal(err)
	}
	if len(gh.merges) != 1 || gh.merges[0] != head {
		t.Fatalf("merge must pin the validated head: %v", gh.merges)
	}
	if len(tk.finished) != 1 || !strings.Contains(tk.finished[0], "已合并: PR #7 squash "+sha) || !strings.Contains(tk.finished[0], "归属: 无") {
		t.Fatalf("finished=%q", tk.finished)
	}
	if strings.Join(o.released, ",") != "ctx_done" || len(o.boards) != 1 || o.boards[0] != "completed /worktrees/ZII-1-executor" {
		t.Fatalf("released=%v boards=%v", o.released, o.boards)
	}
	if strings.Join(*git, "|") != "ls-remote --heads origin refs/heads/agent/ZII-1/executor|push origin --delete refs/heads/agent/ZII-1/executor" {
		t.Fatalf("git=%q", *git)
	}
}

func TestPendingCIWaits(t *testing.T) {
	for name, runs := range map[string][]github.CheckRun{
		"no check runs yet": nil,
		"check in progress": {{Status: "in_progress", HeadSHA: head}},
		"check on old head": {{Status: "completed", Conclusion: "success", HeadSHA: main}},
	} {
		t.Run(name, func(t *testing.T) {
			gh := &fakeGitHub{prs: []github.PullRequest{openPR()}, runs: runs}
			l, _, tk, _, ticket := lane(t, gh)
			if err := l.Process(context.Background(), []scheduler.Ticket{ticket}); err != nil {
				t.Fatal(err)
			}
			if len(gh.merges) != 0 || len(tk.finished) != 0 || len(tk.escalated) != 0 {
				t.Fatalf("merges=%v finished=%v escalated=%v", gh.merges, tk.finished, tk.escalated)
			}
		})
	}
}

// A merge confirmed in an earlier tick whose Linear update failed is finished, not merged again.
func TestAlreadyMergedPRIsFinished(t *testing.T) {
	pr := github.PullRequest{Number: 7, State: "closed", Merged: true, MergeCommitSHA: sha}
	gh := &fakeGitHub{prs: []github.PullRequest{pr}}
	l, _, tk, _, ticket := lane(t, gh)
	if err := l.Process(context.Background(), []scheduler.Ticket{ticket}); err != nil {
		t.Fatal(err)
	}
	if len(gh.merges) != 0 || len(tk.finished) != 1 || !strings.Contains(tk.finished[0], sha) {
		t.Fatalf("merges=%v finished=%v", gh.merges, tk.finished)
	}
}

func TestMissingPRGoesToHuman(t *testing.T) {
	gh := &fakeGitHub{}
	l, _, tk, _, ticket := lane(t, gh)
	if err := l.Process(context.Background(), []scheduler.Ticket{ticket}); err != nil {
		t.Fatal(err)
	}
	if len(tk.escalated) != 1 || !strings.Contains(tk.escalated[0], "找不到 agent/ZII-1/executor 的 PR") {
		t.Fatalf("escalated=%v", tk.escalated)
	}
}
