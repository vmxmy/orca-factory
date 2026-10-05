package merge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// CommandGit keeps temporary validation worktrees beneath the supplied checkout,
// not in a shared temp directory. Run defaults to runx.Run and is injectable.
type CommandGit struct {
	Run func(context.Context, runx.Spec) ([]byte, error)
}

var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func (g CommandGit) run(ctx context.Context, r Request, args ...string) ([]byte, error) {
	run := g.Run
	if run == nil {
		run = runx.Run
	}
	return run(ctx, runx.Spec{Path: "git", Args: args, Dir: r.Path, Timeout: 2 * time.Minute})
}
func (g CommandGit) MergeTree(ctx context.Context, r Request, head, main string) (string, bool, error) {
	if !objectID.MatchString(head) || !objectID.MatchString(main) {
		return "", false, errors.New("invalid merge object ID")
	}
	out, err := g.run(ctx, r, "merge-tree", "--write-tree", head, main)
	if err != nil {
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", true, nil
		}
		return "", false, errors.New("git merge-tree failed")
	}
	tree := strings.TrimSpace(string(out))
	if !objectID.MatchString(tree) {
		return "", false, errors.New("invalid merge-tree output")
	}
	return tree, false, nil
}
func (g CommandGit) WithWorktree(ctx context.Context, r Request, tree, head, main string, check func(context.Context, string) error) (err error) {
	if !objectID.MatchString(tree) || !objectID.MatchString(head) || !objectID.MatchString(main) || !filepath.IsAbs(r.Path) {
		return errors.New("invalid validation worktree input")
	}
	out, err := g.run(ctx, r, "-c", "user.name=Orca merge validation", "-c", "user.email=merge@localhost", "commit-tree", tree, "-p", head, "-p", main, "-m", "Validate merged tree")
	if err != nil {
		return errors.New("validation commit failed")
	}
	commit := strings.TrimSpace(string(out))
	if !objectID.MatchString(commit) {
		return errors.New("invalid validation commit")
	}
	parent, err := os.MkdirTemp(r.Path, ".merge-validation-")
	if err != nil {
		return errors.New("validation directory unavailable")
	}
	path := filepath.Join(parent, "worktree")
	added := false
	defer func() {
		// Cleanup has its own deadline because a canceled gate must not leak a
		// registered worktree. Keep files if git cannot unregister it for recovery.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, removeErr := g.run(cleanupCtx, r, "worktree", "remove", "--force", path)
		if removeErr != nil && added {
			err = errors.New("validation worktree cleanup failed")
			return
		}
		if removeErr = os.RemoveAll(parent); removeErr != nil {
			err = errors.New("validation directory cleanup failed")
		}
	}()
	if _, err = g.run(ctx, r, "worktree", "add", "--detach", path, commit); err != nil {
		return errors.New("validation worktree creation failed")
	}
	added = true
	return check(ctx, path)
}
