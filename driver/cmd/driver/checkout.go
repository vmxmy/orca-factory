package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// fastForwardCheckout moves a repository's own checkout to origin/main when it
// sits on main with no tracked changes. The resolver role and the core check
// read that checkout, so a stale one keeps merged changes from taking effect.
func fastForwardCheckout(ctx context.Context, repo string) error {
	if _, err := runGit(ctx, repo, 2*time.Minute, "fetch", "--quiet", "origin", "main"); err != nil {
		return fmt.Errorf("fetch origin main 失败: %w", err)
	}
	branch, err := runGit(ctx, repo, 10*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch != "main" {
		return fmt.Errorf("检出在 %s 分支，未快进", branch)
	}
	status, err := runGit(ctx, repo, 30*time.Second, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("检出有未提交改动，未快进")
	}
	if _, err := runGit(ctx, repo, time.Minute, "merge", "--ff-only", "--quiet", "origin/main"); err != nil {
		return fmt.Errorf("快进到 origin/main 失败: %w", err)
	}
	return nil
}
