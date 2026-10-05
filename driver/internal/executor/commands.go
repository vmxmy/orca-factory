package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Commands runs all subprocesses through runx. ReviewerCommand is trusted local
// configuration, never ticket text. Run may be replaced by a fake in tests.
type Commands struct {
	ReviewerCommand string
	Run             func(context.Context, runx.Spec) ([]byte, error)
}

func (c *Commands) run(ctx context.Context, dir, path string, args ...string) ([]byte, error) {
	run := c.Run
	if run == nil {
		run = runx.Run
	}
	return run(ctx, runx.Spec{Path: path, Args: args, Dir: dir, Timeout: 10 * time.Minute})
}
func (c *Commands) Baseline(ctx context.Context, repo string) (string, error) {
	b, err := c.run(ctx, repo, "git", "rev-parse", "--verify", "origin/main^{commit}")
	return strings.TrimSpace(string(b)), err
}
func (c *Commands) Head(ctx context.Context, repo, branch string) (string, error) {
	// ls-remote distinguishes a missing branch from a network/read failure and
	// fetching the exact observed SHA avoids trusting an old remote-tracking ref.
	b, err := c.run(ctx, repo, "git", "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", errors.New("remote branch lookup failed")
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "", nil
	}
	if len(fields) != 2 || fields[1] != "refs/heads/"+branch {
		return "", errors.New("invalid branch response")
	}
	sha := fields[0]
	if _, err = c.run(ctx, repo, "git", "fetch", "--no-tags", "origin", sha); err != nil {
		return "", errors.New("branch fetch failed")
	}
	return sha, nil
}
func (c *Commands) ChangedFiles(ctx context.Context, repo, base, head string) ([]string, error) {
	b, err := c.run(ctx, repo, "git", "diff", "--no-ext-diff", "--name-only", "-z", base+".."+head, "--")
	if err != nil {
		return nil, errors.New("failed revision diff failed")
	}
	if len(b) == 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), nil
}
func (c *Commands) Gate(ctx context.Context, repo, head, spec string) (out string, err error) {
	// Validate the exact pushed commit in an isolated checkout. Never run the gate
	// against the executor's own checkout or a worker's uncommitted modifications.
	dir, err := os.MkdirTemp(filepath.Dir(spec), "verify-")
	if err != nil {
		return "", errors.New("verification directory failed")
	}
	if err = os.Remove(dir); err != nil {
		return "", errors.New("verification directory preparation failed")
	}
	if _, err = c.run(ctx, repo, "git", "worktree", "add", "--detach", dir, head); err != nil {
		return "", errors.New("verification checkout failed")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, removeErr := c.run(cleanup, repo, "git", "worktree", "remove", "--force", dir)
		if removeErr != nil {
			err = errors.Join(err, errors.New("verification checkout cleanup failed"))
		}
	}()
	b, err := c.run(ctx, repo, filepath.Join(repo, "scripts", "agent-gates", "gate.sh"), dir, spec)
	return string(b), err
}
func (c *Commands) Review(ctx context.Context, repo, base, head string) (string, error) {
	if strings.TrimSpace(c.ReviewerCommand) == "" {
		return "", errors.New("reviewer command is required")
	}
	diff, err := c.run(ctx, repo, "git", "diff", "--no-ext-diff", base+"..."+head, "--")
	if err != nil {
		return "", errors.New("review diff failed")
	}
	// runx intentionally has no stdin field. A private temporary file supplies
	// stdin through a fixed shell wrapper; no diff bytes become shell source.
	f, err := os.CreateTemp(repo, ".driver-review-*")
	if err != nil {
		return "", errors.New("review input creation failed")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(diff); err != nil {
		_ = f.Close()
		return "", errors.New("review input write failed")
	}
	if err = f.Close(); err != nil {
		return "", errors.New("review input close failed")
	}
	prompt := "Review the diff on stdin against the repository constitution. End stdout with JSON {\"unit\":\"executor\",\"verdict\":\"green|red\",\"step\":\"review\",\"reason\":\"...\",\"evidence\":[],\"scope\":\"...\",\"lane\":2}."
	b, err := c.run(ctx, repo, "/bin/sh", "-c", `exec /bin/sh -c "$1" reviewer "$3" < "$2"`, "reviewer", c.ReviewerCommand+` "$1"`, name, prompt)
	return string(b), err
}
