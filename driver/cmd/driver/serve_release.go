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

// Tick admits feature requests first. AfterTick scans releases and advances the
// same lane once, including ticks with no feature PRs. Waiting timer polls do neither.
type serveReleases struct {
	Lane *mergelane.Lane
	Scan func(context.Context) error
}

func (r *serveReleases) Process(ctx context.Context, queue []scheduler.Ticket) error {
	return r.Lane.Admit(ctx, queue)
}
func (r *serveReleases) AfterTick(ctx context.Context) error {
	err := r.Scan(ctx)
	if r.Lane != nil {
		return errors.Join(err, r.Lane.Process(ctx, nil))
	}
	return err
}
func wireReleases(s *scheduler.Scheduler, lane *mergelane.Lane, gh *github.Client) {
	// Publication reads specify the canonical repository in each request; this
	// adapter only needs a working directory for the gh process.
	lane.Publisher = &release.RecoveringPublisher{Source: &release.RepositorySource{}, Client: gh}
	s.Merger = &serveReleases{Lane: lane, Scan: func(ctx context.Context) error {
		var problems []error
		now := s.Now
		if now == nil {
			now = time.Now
		}
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
			if err = scanRepository(ctx, repo, lane, gh, now); err != nil {
				problems = append(problems, err)
			}
		}
		return errors.Join(problems...)
	}}
}
func scanRepository(ctx context.Context, repo config.Repository, lane *mergelane.Lane, gh *github.Client, now func() time.Time) error {
	src := &release.RepositorySource{Repository: repo}
	if err := src.Refresh(ctx); err != nil {
		return err
	}
	plans, err := release.Scan(ctx, src, now)
	if err != nil {
		return err
	}
	var problems []error
	for _, plan := range plans {
		err = lane.SubmitRelease(ctx, &release.Writer{Source: src, GitHub: gh}, merge.Request{Repository: repo.Env["AGENT_GH_REPO"], Path: repo.Path}, plan)
		if err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}
