package executor

import (
	"context"
	"errors"
	"fmt"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Commands runs all subprocesses through runx. ReviewerCommand is trusted local
// configuration, never ticket text. Run may be replaced by a fake in tests.
type Commands struct {
	ReviewerCommand          string
	ReviewerSmallCommand     string
	ReviewerSmallMaxEstimate *float64
	Run                      func(context.Context, runx.Spec) ([]byte, error)
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
	var b []byte
	err := infra.Retry(ctx, infra.Options{Idempotent: true}, func(ctx context.Context) error {
		var runErr error
		b, runErr = c.run(ctx, repo, "git", "ls-remote", "--heads", "origin", "refs/heads/"+branch)
		return runErr
	})
	if err != nil {
		return "", fmt.Errorf("remote branch lookup failed: %w", err)
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

// MergeBase fetches main and returns its merge-base with head.
func (c *Commands) MergeBase(ctx context.Context, repo, head string) (string, error) {
	err := infra.Retry(ctx, infra.Options{Idempotent: true}, func(ctx context.Context) error {
		_, runErr := c.run(ctx, repo, "git", "fetch", "--no-tags", "origin", "main")
		return runErr
	})
	if err != nil {
		return "", fmt.Errorf("main fetch failed: %w", err)
	}
	b, err := c.run(ctx, repo, "git", "merge-base", "FETCH_HEAD", head)
	base := strings.TrimSpace(string(b))
	if err != nil || base == "" {
		return "", fmt.Errorf("merge-base lookup failed: %w", err)
	}
	return base, nil
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
func (c *Commands) Review(ctx context.Context, repo, base, head string, estimate *float64) (string, string, error) {
	command, tier := c.ReviewerCommand, "default"
	maximum := 2.0
	if c.ReviewerSmallMaxEstimate != nil {
		maximum = *c.ReviewerSmallMaxEstimate
	}
	if estimate != nil && *estimate <= maximum && strings.TrimSpace(c.ReviewerSmallCommand) != "" {
		command, tier = c.ReviewerSmallCommand, "small"
	}
	if strings.TrimSpace(command) == "" {
		return "", tier, errors.New("reviewer command is required")
	}
	diff, err := c.run(ctx, repo, "git", "diff", "--no-ext-diff", base+"..."+head, "--")
	if err != nil {
		return "", tier, errors.New("review diff failed")
	}
	prompt := "Review the diff below against the repository constitution. End stdout with JSON {\"unit\":\"executor\",\"verdict\":\"green|red\",\"step\":\"review\",\"reason\":\"...\",\"evidence\":[],\"scope\":\"...\",\"lane\":2}."
	// The role, the instructions and the diff all travel on stdin: claude's
	// --allowedTools is variadic and swallows a trailing positional prompt.
	input := prompt + "\n\n```diff\n" + string(diff) + "```\n"
	if role, readErr := os.ReadFile(filepath.Join(repo, "scripts", "agent-gates", "roles", "reviewer.md")); readErr == nil {
		input = string(role) + "\n\n" + input
	}
	// runx intentionally has no stdin field. A private temporary file supplies
	// stdin through a fixed shell wrapper; no diff bytes become shell source.
	f, err := os.CreateTemp(repo, ".driver-review-*")
	if err != nil {
		return "", tier, errors.New("review input creation failed")
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }() // Best-effort cleanup of the consumed review input.
	if _, err = f.WriteString(input); err != nil {
		_ = f.Close()
		return "", tier, errors.New("review input write failed")
	}
	if err = f.Close(); err != nil {
		return "", tier, errors.New("review input close failed")
	}
	b, err := c.run(ctx, repo, "/bin/sh", "-c", `exec /bin/sh -c "$1" reviewer < "$2"`, "reviewer", command, name)
	return string(b), tier, err
}

// ValidateFollowupScope uses bash's pattern matcher, just like gate match_any:
// notably '*' crosses directory separators. Patterns remain positional data.
func (c *Commands) ValidateFollowupScope(ctx context.Context, repo, scope string, globs []string) (string, error) {
	tree, err := c.run(ctx, repo, "git", "ls-tree", "-r", "-t", "-z", "origin/main")
	if err != nil {
		return "", errors.New("main tree lookup failed")
	}
	var entries []string
	directories := map[string]bool{".": true}
	for _, record := range strings.Split(strings.TrimSuffix(string(tree), "\x00"), "\x00") {
		metadata, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 {
			return "", errors.New("invalid main tree entry")
		}
		entries = append(entries, name)
		if fields[1] == "tree" {
			directories[name] = true
		}
	}
	match := func(value, pattern string) (bool, error) {
		b, err := c.run(ctx, repo, "/bin/bash", "-c", `if [[ $1 == $2 ]]; then printf yes; fi`, "followup-match", value, pattern)
		return string(b) == "yes", err
	}
	for _, scopePath := range strings.Fields(scope) {
		if filepath.IsAbs(scopePath) || scopePath != filepath.ToSlash(filepath.Clean(scopePath)) || scopePath == ".." || strings.HasPrefix(scopePath, "../") {
			return "SCOPE 路径无效", nil
		}
		candidates := []string{scopePath}
		exists := false
		parent := filepath.ToSlash(filepath.Dir(scopePath))
		for _, entry := range entries {
			matched, err := match(entry, scopePath)
			if err != nil {
				return "", err
			}
			if matched {
				exists = true
				candidates = append(candidates, entry)
			}
			if directories[parent] {
				exists = true
			}
			if strings.HasPrefix(entry, scopePath+"/") {
				candidates = append(candidates, entry)
			}
		}
		if !exists {
			return "SCOPE 路径或所在目录在 origin/main 不存在", nil
		}
		for _, candidate := range candidates {
			for _, glob := range globs {
				matched, err := match(candidate, glob)
				if err != nil {
					return "", err
				}
				if matched {
					return "SCOPE 命中 lane 3", nil
				}
			}
		}
	}
	return "", nil
}

// FollowupCheckFails accepts only a normal nonzero exit. Infrastructure errors,
// cancellation and signals are not evidence of a failing behavior check.
func (c *Commands) FollowupCheckFails(ctx context.Context, repo, check string) (failed bool, err error) {
	repo, err = filepath.Abs(repo)
	if err != nil {
		return false, err
	}
	root := filepath.Join(repo, ".omc", "tmp")
	if err = os.MkdirAll(root, 0700); err != nil {
		return false, err
	}
	dir, err := os.MkdirTemp(root, "followup-")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(dir) }() // Temporary directory cleanup; worktree removal is checked separately.
	worktree := filepath.Join(dir, "main")
	if _, err = c.run(ctx, repo, "git", "worktree", "add", "--detach", worktree, "origin/main"); err != nil {
		return false, errors.New("followup checkout failed")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, removeErr := c.run(cleanup, repo, "git", "worktree", "remove", "--force", worktree)
		if removeErr != nil {
			failed = false
			err = errors.Join(err, errors.New("followup checkout cleanup failed"))
		}
	}()
	run := c.Run
	if run == nil {
		run = runx.Run
	}
	_, err = run(ctx, runx.Spec{Path: "/bin/bash", Args: []string{"-c", check}, Dir: worktree, Timeout: 5 * time.Minute})
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err == nil {
		return false, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return true, nil
	}
	return false, err
}
