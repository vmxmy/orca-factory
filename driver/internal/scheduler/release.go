package scheduler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// EnableReleases is called only by live serve, after acquiring its singleton
// lock. Dry-run and one-shot inspection never construct publication adapters.
func (r *Runtime) EnableReleases(stateRoot string) error {
	if r.GitHub == nil {
		var err error
		r.GitHub, err = github.New(github.Options{})
		if err != nil {
			return err
		}
	}
	r.Releases = map[string]*release.Manager{}
	for path, cfg := range r.Repositories {
		name := cfg.Env["AGENT_GH_REPO"]
		if name == "" {
			continue
		}
		adapter := &release.Repository{Path: path, Name: name, EnvValues: cfg.Env, Run: r.Run, GitHub: r.GitHub}
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
		m := &release.Manager{File: filepath.Join(stateRoot, "releases", key+".json"), Request: merge.Request{Repository: name, Path: path, DeployWorkflow: cfg.Env["AGENT_DEPLOY_WORKFLOW"], SmokeCommand: cfg.Env["AGENT_SMOKE_CMD"]}, Source: adapter.Snapshot, Writer: adapter, Publisher: adapter, Now: r.Now}
		m.Merger = &release.MergeAdapter{Repository: adapter, Manager: m}
		m.Deploy = &releaseDeployment{runtime: r}
		r.Releases[path] = m
	}
	return nil
}
func (r *Runtime) AfterTick(ctx context.Context) error {
	var problems []error
	paths := make([]string, 0, len(r.ReleaseEligible))
	for path, ok := range r.ReleaseEligible {
		if ok {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		if m := r.Releases[path]; m != nil {
			if err := m.Scan(ctx); err != nil {
				problems = append(problems, fmt.Errorf("release scan %s: %w", path, err))
			}
		}
	}
	return errors.Join(problems...)
}

type releaseDeployment struct{ runtime *Runtime }

func (d *releaseDeployment) Check(ctx context.Context, req merge.Request, sha string) (merge.DeploymentState, error) {
	if req.DeployWorkflow == "" {
		return merge.DeploymentPassed, nil
	}
	owner, repo, ok := splitReleaseRepo(req.Repository)
	if !ok {
		return merge.DeploymentPending, errors.New("invalid repository")
	}
	state, err := d.runtime.GitHub.Deployment(ctx, owner, repo, req.DeployWorkflow, sha)
	switch state {
	case "passed":
		return merge.DeploymentPassed, err
	case "failed":
		return merge.DeploymentFailed, err
	default:
		return merge.DeploymentPending, err
	}
}
func splitReleaseRepo(name string) (string, string, bool) {
	owner, repo, ok := strings.Cut(name, "/")
	return owner, repo, ok && owner != "" && repo != "" && !strings.Contains(repo, "/")
}
func (d *releaseDeployment) Smoke(ctx context.Context, req merge.Request, sha string, beforeStart func() error) error {
	release, guardErr := maintenance.Write()
	if guardErr != nil {
		return guardErr
	}
	defer release()
	if !commitID(sha) {
		return errors.New("release smoke SHA invalid")
	}
	// GitHub squash creates a new object after merge validation fetched main.
	if _, err := d.runtime.command(ctx, req.Path, "git", "fetch", "--no-tags", "origin", sha); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "driver-release-smoke-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if _, err = d.runtime.command(ctx, req.Path, "git", "clone", "--no-checkout", "--shared", req.Path, dir); err != nil {
		return err
	}
	if _, err = d.runtime.command(ctx, dir, "git", "checkout", "--detach", sha); err != nil {
		return err
	}
	run := d.runtime.Run
	if run == nil {
		run = runx.Run
	}
	env := []string{}
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "AGENT_MERGE_SHA=") && !strings.HasPrefix(item, "AGENT_GH_REPO=") {
			env = append(env, item)
		}
	}
	env = append(env, "AGENT_MERGE_SHA="+sha, "AGENT_GH_REPO="+req.Repository)
	if err = beforeStart(); err != nil {
		return err
	}
	_, err = run(ctx, runx.Spec{Path: "bash", Dir: dir, Args: []string{"-c", req.SmokeCommand}, Env: env, Timeout: 30 * time.Second})
	return err
}
