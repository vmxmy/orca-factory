package executor

import (
	"context"
	"os"
	"strings"
	"testing"
)

// applySteps honors whole-file keep-ours / keep-theirs and refuses finer merges.
func TestApplySteps(t *testing.T) {
	e := &Executor{}
	calls := &[]string{}
	stub := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		return nil, nil
	}
	orig := runGit
	runGit = stub
	defer func() { runGit = orig }()

	steps := "keep ours in driver/a.go\nkeep theirs in driver/b.go\n"
	if err := e.applySteps(context.Background(), "/wt", "driver/a.go", steps); err != nil {
		t.Fatal(err)
	}
	if err := e.applySteps(context.Background(), "/wt", "driver/b.go", steps); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*calls, "|")
	if !strings.Contains(got, "checkout --ours driver/a.go") || !strings.Contains(got, "checkout --theirs driver/b.go") {
		t.Fatalf("calls=%q", got)
	}

	if err := e.applySteps(context.Background(), "/wt", "driver/c.go", steps); err == nil {
		t.Fatal("unresolved file must fail")
	}
	if err := e.applySteps(context.Background(), "/wt", "driver/a.go", "combine both in driver/a.go: keep ours and theirs\n"); err == nil {
		t.Fatal("combine must fail so the worker edits it")
	}
}

type recordingCoordinator struct {
	sessions int
	steps    string
	linear   *fakeLinear
}

func (r *recordingCoordinator) Correction(_ context.Context, req CorrectionRequest) error {
	r.sessions++
	if r.linear != nil {
		r.linear.comments = append(r.linear.comments, "correction.md written")
	}
	return os.WriteFile(req.Path, []byte(r.steps), 0o600)
}
func (r *recordingCoordinator) ChildTask(context.Context, string, string, string, string) (string, error) {
	return "child_task", nil
}
func (r *recordingCoordinator) Complete(context.Context, string, string) (bool, error) {
	return false, nil
}
func (r *recordingCoordinator) Worktree(context.Context, string, string) (string, error) {
	return "/original/worktree", nil
}

// resolveMergeConflict consults the coordinator, merges, resolves, and pushes.
func TestResolveMergeConflict(t *testing.T) {
	e, _, _, _, _, ticket := fixture(t, "conflict-sha")
	wt := t.TempDir()
	coordinator := &recordingCoordinator{steps: "keep ours in driver/x.go\nkeep theirs in driver/y.go\n"}
	e.Recovery = coordinator
	e.Config.Merging = true
	calls := &[]string{}
	orig := runGit
	runGit = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		switch args[0] {
		case "diff":
			return []byte("driver/x.go\ndriver/y.go\n"), nil
		}
		return nil, nil
	}
	defer func() { runGit = orig }()
	// Inject a coordinator that writes the steps file without a subprocess.
	origCoordinator := mergeConflictRunner
	mergeConflictRunner = func(ctx context.Context, m MergeConflictCorrection, req CorrectionRequest) error {
		return os.WriteFile(req.Path, []byte("keep ours in driver/x.go\nkeep theirs in driver/y.go\n"), 0o600)
	}
	defer func() { mergeConflictRunner = origCoordinator }()
	if err := e.resolveMergeConflict(context.Background(), e.Config, ticket, wt, "origin/main", "conflict-sha"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) == 0 {
		t.Fatal("no git commands ran")
	}
	got := strings.Join(*calls, "|")
	for _, want := range []string{"checkout --ours driver/x.go", "checkout --theirs driver/y.go", "commit", "push origin HEAD:refs/heads/agent/ZII-861/executor"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}
