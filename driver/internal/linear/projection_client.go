package linear

import (
	"context"
	"time"
)

// ProjectionClient keeps ordinary reads local when a complete projection is
// available. Mutations and explicit remote reads always go to the server.
type ProjectionClient struct {
	remote     *Client
	projection *Projection
	maxAge     time.Duration
}

func NewProjectionClient(remote *Client, projection *Projection, maxAge time.Duration) *ProjectionClient {
	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}
	return &ProjectionClient{remote: remote, projection: projection, maxAge: maxAge}
}

func (c *ProjectionClient) Snapshot(ctx context.Context, projects []string, since time.Time) ([]Issue, error) {
	if since.IsZero() && c.projection != nil {
		if read, err := c.projection.Read(ctx, ReadOptions{MaxAge: c.maxAge, RequireComplete: true}); err == nil && read.Meta.Fresh {
			return filterProjectionIssues(read.Issues, projects), nil
		}
	}
	issues, err := c.remote.Snapshot(ctx, projects, since)
	if err != nil {
		return nil, err
	}
	if c.projection != nil {
		comments := make(map[string][]Comment, len(issues))
		for _, issue := range issues {
			rows, readErr := c.remote.ListCommentsRemote(ctx, issue.ID)
			if readErr != nil {
				return issues, readErr
			}
			comments[issue.ID] = rows
		}
		snapshot := ProjectionSnapshot{Issues: issues, Comments: comments, Complete: true, Watermark: projectionWatermark(issues)}
		if since.IsZero() {
			err = c.projection.Publish(ctx, snapshot)
		} else {
			err = c.projection.ApplyIncremental(ctx, snapshot)
		}
		if err != nil {
			return issues, err
		}
	}
	return issues, nil
}

func (c *ProjectionClient) ListComments(ctx context.Context, id string) ([]Comment, error) {
	if c.projection != nil {
		if read, err := c.projection.Read(ctx, ReadOptions{MaxAge: c.maxAge, RequireComplete: true}); err == nil && read.Meta.Fresh {
			if rows, ok := read.Comments[id]; ok {
				return rows, nil
			}
			for _, issue := range read.Issues {
				if issue.ID == id {
					return []Comment{}, nil
				}
			}
		}
	}
	return c.remote.ListComments(ctx, id)
}

// ListCommentsRemote is the critical verification seam used by
// CriticalComments and by write readbacks.
func (c *ProjectionClient) ListCommentsRemote(ctx context.Context, id string) ([]Comment, error) {
	return c.remote.ListCommentsRemote(ctx, id)
}

func (c *ProjectionClient) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (Issue, error) {
	return c.remote.UpdateIssue(ctx, id, state, add, remove)
}

func (c *ProjectionClient) CreateComment(ctx context.Context, id, body string) (Comment, error) {
	return c.remote.CreateComment(ctx, id, body)
}

func (c *ProjectionClient) ProbeQuota(ctx context.Context) error { return c.remote.ProbeQuota(ctx) }
func (c *ProjectionClient) Usage() Usage                         { return c.remote.Usage() }
func (c *ProjectionClient) Metrics() Metrics                     { return c.remote.Metrics() }
func (c *ProjectionClient) ReserveCritical(now time.Time, reserve int64) bool {
	return c.remote.ReserveCritical(now, reserve)
}
func (c *ProjectionClient) ReleaseCritical() { c.remote.ReleaseCritical() }

func filterProjectionIssues(issues []Issue, projects []string) []Issue {
	allowed := map[string]bool{}
	for _, project := range projects {
		allowed[project] = true
	}
	filtered := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if allowed[issue.Project.ID] || allowed[issue.Project.Name] {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

func projectionWatermark(issues []Issue) string {
	latest := time.Time{}
	for _, issue := range issues {
		if issue.UpdatedAt.After(latest) {
			latest = issue.UpdatedAt
		}
	}
	if latest.IsZero() {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return latest.UTC().Format(time.RFC3339Nano)
}
