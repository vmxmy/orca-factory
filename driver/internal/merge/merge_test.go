package merge

import (
	"context"
	"errors"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeGitHub struct {
	snapshots     []Snapshot
	reads, merges int
	mergeErr      error
	order         []int
}

func (f *fakeGitHub) Snapshot(context.Context, Request) (Snapshot, error) {
	i := f.reads
	f.reads++
	if i >= len(f.snapshots) {
		i = len(f.snapshots) - 1
	}
	return f.snapshots[i], nil
}
func (f *fakeGitHub) Squash(_ context.Context, r Request, head, main string) (string, error) {
	f.merges++
	f.order = append(f.order, r.Number)
	return "merged", f.mergeErr
}

type fakeGit struct {
	conflict   bool
	trees      int
	head, main string
	worktrees  []string
}

func (f *fakeGit) MergeTree(_ context.Context, _ Request, head, main string) (string, bool, error) {
	f.trees++
	f.head, f.main = head, main
	return "tree", f.conflict, nil
}
func (f *fakeGit) WithWorktree(ctx context.Context, _ Request, tree, head, main string, run func(context.Context, string) error) error {
	f.worktrees = append(f.worktrees, tree)
	return run(ctx, "isolated-result")
}

type fakeGate struct {
	paths []string
	err   error
}

func (f *fakeGate) Run(_ context.Context, _ Request, path string) error {
	f.paths = append(f.paths, path)
	return f.err
}

type fakeDeploy struct {
	state DeploymentState
	calls []string
}

func (f *fakeDeploy) Check(_ context.Context, _ Request, sha string) (DeploymentState, error) {
	f.calls = append(f.calls, sha)
	return f.state, nil
}
func ready() Snapshot {
	return Snapshot{Head: "head", Main: "main", CIHead: "head", CIGreen: true, MergeableState: "clean", Behind: true, PRFiles: []string{"feature"}, MainFiles: []string{"other"}}
}
func fixture(s Snapshot) (*Channel, *fakeGitHub, *fakeGit, *fakeGate, *fakeDeploy) {
	h := &fakeGitHub{snapshots: []Snapshot{s}}
	g := &fakeGit{}
	gate := &fakeGate{}
	d := &fakeDeploy{state: DeploymentPassed}
	return New(h, g, gate, d), h, g, gate, d
}
func req(n int) Request { return Request{Repository: "owner/repo", Number: n} }
func next(t *testing.T, c *Channel, repo string) Result {
	t.Helper()
	r, e := c.Next(context.Background(), repo)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestDisjointDirectSquash(t *testing.T) {
	c, h, g, gate, _ := fixture(ready())
	c.Enqueue(req(1))
	r := next(t, c, "owner/repo")
	if r.Status != Merged || r.MergeSHA != "merged" || h.merges != 1 || g.trees != 0 || len(gate.paths) != 0 {
		t.Fatalf("result=%+v merges=%d trees=%d gate=%v", r, h.merges, g.trees, gate.paths)
	}
}
func TestOverlapGatesMergedTree(t *testing.T) {
	s := ready()
	s.MainFiles = []string{"feature"}
	c, h, g, gate, _ := fixture(s)
	c.Enqueue(req(1))
	r := next(t, c, "owner/repo")
	if r.Status != Merged || h.merges != 1 || !reflect.DeepEqual(g.worktrees, []string{"tree"}) || !reflect.DeepEqual(gate.paths, []string{"isolated-result"}) || g.head != "head" || g.main != "main" {
		t.Fatalf("result=%+v git=%+v gate=%+v", r, g, gate)
	}
}
func TestConflictReturnsSchedulerCorrection(t *testing.T) {
	s := ready()
	s.MainFiles = []string{"feature"}
	s.MergeableState = "dirty"
	c, h, g, _, _ := fixture(s)
	g.conflict = true
	c.Enqueue(req(1))
	r := next(t, c, "owner/repo")
	if r.Status != Correction || r.Corrections != 1 || h.merges != 0 {
		t.Fatalf("%+v merges=%d", r, h.merges)
	}
	if next(t, c, "owner/repo").Status != Empty {
		t.Fatal("conflict retained in queue")
	}
}
func TestTwoMovingMainRoundsEscalate(t *testing.T) {
	c, h, _, _, _ := fixture(ready())
	h.mergeErr = ErrMainAdvanced
	c.Enqueue(req(1))
	a := next(t, c, "owner/repo")
	b := next(t, c, "owner/repo")
	if a.Status != Waiting || b.Status != Human || b.Rounds != 2 || h.merges != 2 {
		t.Fatalf("first=%+v second=%+v merges=%d", a, b, h.merges)
	}
}
func TestDeploymentWaitAndSmokeFailurePause(t *testing.T) {
	c, h, _, _, d := fixture(ready())
	a := req(1)
	a.DeployWorkflow = "deploy.yml"
	a.SmokeCommand = "smoke"
	c.Enqueue(a)
	c.Enqueue(req(2))
	if next(t, c, a.Repository).Status != Merged {
		t.Fatal("first merge")
	}
	d.state = DeploymentPending
	if next(t, c, a.Repository).Status != Waiting || h.merges != 1 {
		t.Fatal("merged before deployment")
	}
	d.state = SmokeFailed
	if next(t, c, a.Repository).Status != Human || next(t, c, a.Repository).Status != Paused || h.merges != 1 {
		t.Fatal("smoke failure did not pause")
	}
	c.Enqueue(Request{Repository: "other/repo", Number: 3})
	if next(t, c, "other/repo").Status != Merged {
		t.Fatal("other repository paused")
	}
	if !reflect.DeepEqual(d.calls, []string{"merged", "merged"}) {
		t.Fatal(d.calls)
	}
}
func TestDeploymentPassReleasesFIFO(t *testing.T) {
	c, h, _, _, _ := fixture(ready())
	a := req(1)
	a.DeployWorkflow = "deploy"
	a.SmokeCommand = "smoke"
	c.Enqueue(a)
	c.Enqueue(req(2))
	next(t, c, a.Repository)
	if next(t, c, a.Repository).Status != Merged || !reflect.DeepEqual(h.order, []int{1, 2}) {
		t.Fatal(h.order)
	}
}
func TestFailClosed(t *testing.T) {
	for _, kind := range []string{"ci-head", "ci-red", "unknown", "gate"} {
		t.Run(kind, func(t *testing.T) {
			s := ready()
			switch kind {
			case "ci-head":
				s.CIHead = "old"
			case "ci-red":
				s.CIGreen = false
			case "unknown":
				s.MergeableState = "unknown"
			case "gate":
				s.MainFiles = []string{"feature"}
			}
			c, h, _, gate, _ := fixture(s)
			if kind == "gate" {
				gate.err = errors.New("red")
			}
			c.Enqueue(req(1))
			r := next(t, c, "owner/repo")
			if h.merges != 0 || (r.Status != Waiting && r.Status != Correction) {
				t.Fatalf("%+v merges=%d", r, h.merges)
			}
		})
	}
}

func TestCommandGitUsesMergedTreeAndCleansAfterGateFailure(t *testing.T) {
	root := t.TempDir()
	head := strings.Repeat("a", 40)
	main := strings.Repeat("b", 40)
	tree := strings.Repeat("c", 40)
	commit := strings.Repeat("d", 40)
	var calls []runx.Spec
	g := CommandGit{Run: func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Timeout <= 0 || s.Path != "git" || ctx.Err() != nil {
			t.Fatalf("unbounded or canceled command: %+v", s)
		}
		calls = append(calls, s)
		switch s.Args[0] {
		case "merge-tree":
			return []byte(tree + "\n"), nil
		case "-c":
			return []byte(commit + "\n"), nil
		}
		return nil, nil
	}}
	r := Request{Path: root}
	got, conflict, err := g.MergeTree(context.Background(), r, head, main)
	if err != nil || conflict || got != tree {
		t.Fatalf("%s %v %v", got, conflict, err)
	}
	var path string
	err = g.WithWorktree(context.Background(), r, tree, head, main, func(_ context.Context, p string) error { path = p; return errors.New("gate failed") })
	if err == nil || filepath.Dir(filepath.Dir(path)) != root {
		t.Fatalf("path=%s err=%v", path, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("temporary directory retained: %v", err)
	}
	if len(calls) != 4 || !reflect.DeepEqual(calls[0].Args, []string{"merge-tree", "--write-tree", head, main}) || !strings.Contains(strings.Join(calls[1].Args, " "), "commit-tree "+tree+" -p "+head+" -p "+main) || !reflect.DeepEqual(calls[2].Args, []string{"worktree", "add", "--detach", path, commit}) || !reflect.DeepEqual(calls[3].Args, []string{"worktree", "remove", "--force", path}) {
		t.Fatalf("calls=%+v", calls)
	}
}

type exitFailure int

func (e exitFailure) Error() string { return "git failed" }
func (e exitFailure) ExitCode() int { return int(e) }
func TestCommandGitDistinguishesConflictFromInfrastructure(t *testing.T) {
	for _, code := range []int{1, 128} {
		g := CommandGit{Run: func(context.Context, runx.Spec) ([]byte, error) { return nil, exitFailure(code) }}
		_, conflict, err := g.MergeTree(context.Background(), Request{}, strings.Repeat("a", 40), strings.Repeat("b", 40))
		if conflict != (code == 1) || (err != nil) != (code == 128) {
			t.Fatalf("code=%d conflict=%v err=%v", code, conflict, err)
		}
	}
}
func TestMainMovesDuringGateNeverMergesStaleTree(t *testing.T) {
	s := ready()
	s.MainFiles = []string{"feature"}
	moved := s
	moved.Main = "new-main"
	c, h, g, _, _ := fixture(s)
	h.snapshots = []Snapshot{s, moved, s, moved}
	c.Enqueue(req(1))
	a := next(t, c, "owner/repo")
	b := next(t, c, "owner/repo")
	if a.Status != Waiting || b.Status != Human || h.merges != 0 || g.trees != 2 {
		t.Fatalf("%+v %+v merges=%d trees=%d", a, b, h.merges, g.trees)
	}
}

func TestDeploymentPolicyBelongsToRepository(t *testing.T) {
	c, h, _, _, d := fixture(ready())
	first := req(1)
	first.DeployWorkflow = "deploy"
	first.SmokeCommand = "smoke"
	c.Enqueue(first)
	c.Enqueue(req(2))
	c.Enqueue(req(3))
	next(t, c, first.Repository)
	next(t, c, first.Repository)
	d.state = DeploymentPending
	r := next(t, c, first.Repository)
	if r.Status != Waiting || h.merges != 2 {
		t.Fatalf("repository deployment barrier lost: %+v merges=%d", r, h.merges)
	}
}
