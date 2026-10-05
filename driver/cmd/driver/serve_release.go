package main

import (
	"context"
	"errors"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/mergelane"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type releaseRepository func(context.Context, config.Repository) (release.Source, release.PRWriter, error)

// Tick admits feature requests first. AfterTick scans releases and advances the
// same lane once, including ticks with no feature PRs. Waiting timer polls do neither.
type serveReleases struct {
	scheduler *scheduler.Scheduler
	lane      *mergelane.Lane
	open      releaseRepository
}

func (r *serveReleases) Process(ctx context.Context, queue []scheduler.Ticket) error {
	return r.lane.Admit(ctx, queue)
}
func (r *serveReleases) AfterTick(ctx context.Context) error {
	var problems []error
	s := r.scheduler
	now := s.Now
	if now == nil {
		now = time.Now
	}
	allowed := map[string]bool{}
	r.lane.Allowed = func(path string) bool { return allowed[path] }
	for _, repo := range s.Config.Repositories {
		if s.Pauses != nil && s.Pauses.Paused(repo.Path) {
			continue
		}
		disabled, err := s.Backend.AutomationDisabled(ctx, repo)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if !disabled {
			continue
		}
		// Deployment-gated repositories are not supported by the shared lane yet;
		// reject before creating any release branch or PR.
		if repo.Env["AGENT_DEPLOY_WORKFLOW"] != "" || repo.Env["AGENT_SMOKE_CMD"] != "" {
			problems = append(problems, errors.New("release lane refuses deployment-gated repository"))
			continue
		}
		allowed[repo.Path] = true
		src, writer, err := r.open(ctx, repo)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		plans, err := release.Scan(ctx, src, now)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for _, plan := range plans {
			if err = r.lane.SubmitRelease(ctx, writer, merge.Request{Repository: repo.Env["AGENT_GH_REPO"], Path: repo.Path}, plan); err != nil {
				problems = append(problems, err)
			}
		}
	}
	return errors.Join(append(problems, r.lane.Process(ctx, nil))...)
}
func wireReleases(s *scheduler.Scheduler, lane *mergelane.Lane, gh *github.Client) {
	lane.Publisher = &release.RecoveringPublisher{Source: &release.RepositorySource{}, Client: gh}
	s.Merger = &serveReleases{scheduler: s, lane: lane, open: func(ctx context.Context, repo config.Repository) (release.Source, release.PRWriter, error) {
		src := &release.RepositorySource{Repository: repo}
		if err := src.Refresh(ctx); err != nil {
			return nil, nil, err
		}
		return src, &release.Writer{Source: src, GitHub: gh}, nil
	}}
}
