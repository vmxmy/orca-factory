package executor

import (
	"context"
	"errors"
	"fmt"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/agentcli"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

const (
	gateTimeout  = 60 * time.Minute
	gateLockWait = 30 * time.Minute
)

// Commands runs all subprocesses through runx. ReviewerCommand is trusted local
// configuration, never ticket text. Run may be replaced by a fake in tests.
type Commands struct {
	ReviewerCommand          string
	ReviewerSmallCommand     string
	ReviewerSmallMaxEstimate *float64
	// ReviewerCandidates and ReviewerSmallCandidates list each structured
	// role's configured model and its fallbacks; StateRoot holds the model
	// registry that decides between them.
	ReviewerCandidates      []modelhealth.Ref
	ReviewerSmallCandidates []modelhealth.Ref
	StateRoot               string
	// InstallCommand comes from the primary checkout's profile, never from the
	// head under verification. Gate runs it in the verify checkout first.
	InstallCommand string
	// ModelProxyURL, when set, reaches the reviewer as ANTHROPIC_BASE_URL.
	ModelProxyURL string
	Run           func(context.Context, runx.Spec) ([]byte, error)
}

func (c *Commands) run(ctx context.Context, dir, path string, args ...string) ([]byte, error) {
	return c.runSpec(ctx, runx.Spec{Path: path, Args: args, Dir: dir, Timeout: 10 * time.Minute})
}
func (c *Commands) runSpec(ctx context.Context, spec runx.Spec) ([]byte, error) {
	run := c.Run
	if run == nil {
		run = runx.Run
	}
	return run(ctx, spec)
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
	b, err := c.run(ctx, repo, "git", "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", base+".."+head, "--")
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
	if strings.TrimSpace(c.InstallCommand) != "" {
		if _, err = c.run(ctx, dir, "/bin/bash", "-c", c.InstallCommand); err != nil {
			return "", &infra.Ticket{Err: errors.New("verification install failed")}
		}
	}
	b, err := c.runSpec(ctx, runx.Spec{
		Path:    filepath.Join(repo, "scripts", "agent-gates", "gate.sh"),
		Args:    []string{dir, spec},
		Dir:     repo,
		Env:     append(os.Environ(), fmt.Sprintf("GATE_LOCK_WAIT=%d", gateLockWait/time.Second)),
		Timeout: gateTimeout,
	})
	return string(b), err
}
func (c *Commands) Review(ctx context.Context, repo, base, head string, estimate *float64) (out, tier string, err error) {
	command, tier := c.ReviewerCommand, "default"
	maximum := 2.0
	if c.ReviewerSmallMaxEstimate != nil {
		maximum = *c.ReviewerSmallMaxEstimate
	}
	candidates := c.ReviewerCandidates
	if estimate != nil && *estimate <= maximum && strings.TrimSpace(c.ReviewerSmallCommand) != "" {
		command, tier, candidates = c.ReviewerSmallCommand, "small", c.ReviewerSmallCandidates
	}
	command, used, selectErr := config.SelectReviewerCommand(command, candidates, c.StateRoot, time.Now())
	if selectErr != nil {
		runx.LoggerFrom(ctx).Warn("reviewer model fallback unavailable; keeping configured model", "tier", tier, "err", selectErr)
	}
	if len(candidates) > 0 && used != candidates[0] {
		runx.LoggerFrom(ctx).Info("reviewer model falls back", "tier", tier, "configured", candidates[0].String(), "selected", used.String())
	}
	if strings.TrimSpace(command) == "" {
		return "", tier, errors.New("reviewer command is required")
	}
	diff, err := c.run(ctx, repo, "git", "diff", "--no-ext-diff", base+"..."+head, "--")
	if err != nil {
		return "", tier, errors.New("review diff failed")
	}
	// The role, the brief and the diff all travel on stdin: claude's
	// --allowedTools is variadic and swallows a trailing positional prompt.
	input := "```diff\n" + string(diff) + "```\n"
	brief, hasBrief := reviewBriefFrom(ctx)
	if brief.Prior != "" {
		input = "## 上一轮 reviewer 意见\n" + brief.Prior + "\n\n" + input
	}
	if hasBrief && strings.TrimSpace(brief.Text) != "" {
		// reviewer.md judges the diff against the ticket brief, and expects the
		// lane-3 globs on the line before it. Without them every change outside the
		// ticket's SCOPE (wiring, central types) is flagged as a defect.
		header := ""
		if brief.Lane3Globs != "" {
			header = "本仓库 AGENT_LANE3_GLOBS=" + brief.Lane3Globs + "\n"
		}
		input = header + brief.Text + "\n\n" + input
	}
	// reviewer.md reviews "the current worktree" and the reviewer reads files to
	// check the diff, so it runs in a detached checkout of head, not the repo
	// checkout that sits on origin/main.
	parent, err := os.MkdirTemp("", "driver-review-")
	if err != nil {
		return "", tier, errors.New("review directory failed")
	}
	defer func() { _ = os.RemoveAll(parent) }() // Best-effort cleanup after the checkout is removed.
	dir := filepath.Join(parent, "head")
	if _, err = c.run(ctx, repo, "git", "worktree", "add", "--detach", dir, head); err != nil {
		return "", tier, errors.New("review checkout failed")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, removeErr := c.run(cleanup, repo, "git", "worktree", "remove", "--force", dir); removeErr != nil {
			err = errors.Join(err, errors.New("review checkout cleanup failed"))
		}
	}()
	rules, err := c.reviewRules(ctx, repo, base)
	if err != nil {
		return "", tier, err
	}
	input = rules + "\n\n" + input
	// runx intentionally has no stdin field. A private file outside the checkout
	// supplies stdin through a fixed shell wrapper; no diff bytes become shell source.
	name := filepath.Join(parent, "input")
	if err = os.WriteFile(name, []byte(input), 0o600); err != nil {
		return "", tier, errors.New("review input write failed")
	}
	spec := runx.Spec{Path: "/bin/sh", Args: []string{"-c", `exec /bin/sh -c "$1" reviewer < "$2"`, "reviewer", command, name}, Dir: dir, Timeout: 10 * time.Minute}
	profile, _ := agentcli.Lookup(used.Agent)
	if len(candidates) == 0 {
		var matched bool
		used, profile, matched = config.MatchReviewer(command)
		if !matched {
			profile.Relay = true // Legacy shell commands inherit the configured proxy.
		}
	}
	spec.Env = c.reviewerEnv(profile)
	b, err := c.runSpec(ctx, spec)
	if err == nil {
		return string(b), tier, nil
	}
	next, retry, modelErr := config.RetryAfterModelError(c.StateRoot, used, candidates, b, err, time.Now())
	if modelErr != nil {
		runx.LoggerFrom(ctx).Warn("reviewer model error not recorded", "tier", tier, "err", modelErr)
	}
	if !retry {
		return string(b), tier, err
	}
	if spec.Args[3], err = config.ReviewerShellCommand(next); err != nil {
		return "", tier, errors.New("reviewer fallback command invalid")
	}
	profile, _ = agentcli.Lookup(next.Agent)
	spec.Env = c.reviewerEnv(profile)
	runx.LoggerFrom(ctx).Info("reviewer model unavailable; retrying with next candidate", "tier", tier, "unavailable", used.String(), "selected", next.String())
	if b, err = c.runSpec(ctx, spec); err != nil {
		if _, _, modelErr = config.RetryAfterModelError(c.StateRoot, next, nil, b, err, time.Now()); modelErr != nil {
			runx.LoggerFrom(ctx).Warn("reviewer model error not recorded", "tier", tier, "err", modelErr)
		}
	}
	return string(b), tier, err
}

func (c *Commands) reviewerEnv(profile agentcli.Profile) []string {
	if profile.Relay && c.ModelProxyURL == "" && os.Getenv("AGENT_MODEL_PROXY_TOKEN") == "" {
		return nil
	}
	env := os.Environ()
	env = slices.DeleteFunc(env, func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return !profile.InheritEnv(key)
	})
	if profile.Relay && c.ModelProxyURL != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+c.ModelProxyURL)
	}
	return env
}

const (
	reviewerRolePath = "scripts/agent-gates/roles/reviewer.md"
	constitutionDir  = "scripts/agent-gates/constitution/"
	constitutionPath = constitutionDir + "constitution.md"
)

// reviewRules is reviewer.md followed by every constitution file, all read from
// base. The merge decision rests on this review, so the reviewed head, whose
// checkout is the reviewer's working directory, must not supply its own rules.
func (c *Commands) reviewRules(ctx context.Context, repo, base string) (string, error) {
	show := func(path string) (string, error) {
		b, err := c.run(ctx, repo, "git", "show", base+":"+path)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return "", fmt.Errorf("baseline %s missing", path)
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	role, err := show(reviewerRolePath)
	if err != nil {
		return "", err
	}
	listing, err := c.run(ctx, repo, "git", "ls-tree", "--name-only", base, constitutionDir)
	if err != nil {
		return "", errors.New("baseline constitution listing failed")
	}
	paths := []string{constitutionPath}
	for _, path := range strings.Split(string(listing), "\n") {
		if strings.HasSuffix(path, ".md") && path != constitutionPath {
			paths = append(paths, path)
		}
	}
	parts := []string{role}
	for _, path := range paths {
		text, err := show(path)
		if err != nil {
			return "", err
		}
		parts = append(parts, "## 基线 constitution：`"+path+"`\n\n"+text)
	}
	return strings.Join(parts, "\n\n"), nil
}

// ReviewBrief is the ticket brief the reviewer judges the diff against, plus the
// previous red findings when this review is a re-review.
type ReviewBrief struct{ Text, Lane3Globs, Prior string }

type reviewBriefKey struct{}

// WithReviewBrief carries the brief to Commands.Review without widening the Tools
// interface that every test double implements.
func WithReviewBrief(ctx context.Context, b ReviewBrief) context.Context {
	return context.WithValue(ctx, reviewBriefKey{}, b)
}
func reviewBriefFrom(ctx context.Context) (ReviewBrief, bool) {
	b, ok := ctx.Value(reviewBriefKey{}).(ReviewBrief)
	return b, ok
}
