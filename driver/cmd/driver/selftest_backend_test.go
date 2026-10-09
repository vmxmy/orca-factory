package main

import (
	"context"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

type snapshotProbe struct {
	projects []string
	updated  time.Time
}

func (p *snapshotProbe) Snapshot(_ context.Context, projects []string, updated time.Time) ([]linear.Issue, error) {
	p.projects = projects
	p.updated = updated
	return nil, nil
}

func (p *snapshotProbe) ListComments(context.Context, string) ([]linear.Comment, error) {
	return nil, nil
}
func (p *snapshotProbe) UpdateIssue(context.Context, string, string, []string, []string) (linear.Issue, error) {
	return linear.Issue{}, nil
}
func (p *snapshotProbe) CreateComment(context.Context, string, string) (linear.Comment, error) {
	return linear.Comment{}, nil
}

func TestSnapshotSelectedBackendUsesGitHubRepository(t *testing.T) {
	p := &snapshotProbe{}
	if _, err := snapshotSelectedBackend(context.Background(), p, map[string]string{
		"AGENT_ISSUE_BACKEND":  "github",
		"AGENT_GH_REPO":        "owner/repo",
		"AGENT_LINEAR_PROJECT": "legacy",
	}); err != nil {
		t.Fatal(err)
	}
	if len(p.projects) != 1 || p.projects[0] != "owner/repo" {
		t.Fatalf("projects=%v", p.projects)
	}
	if !p.updated.IsZero() {
		t.Fatalf("GitHub snapshot used Linear freshness filter: %v", p.updated)
	}
}
