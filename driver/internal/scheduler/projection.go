package scheduler

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	issuebackend "github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

const commentProjectionMaxAge = 5 * time.Minute

type commentProjection struct {
	updatedAt time.Time
	readAt    time.Time
	comments  []linear.Comment
}

func (r *Runtime) snapshotComments(ctx context.Context, client LinearClient, issues []linear.Issue) (map[string][]linear.Comment, map[string]commentProjection, error) {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	_, cacheable := client.(*linear.Client)
	comments := map[string][]linear.Comment{}
	projection := map[string]commentProjection{}
	ids := []string{}
	for _, issue := range issues {
		cached, ok := r.projections[client][issue.ID]
		age := now.Sub(cached.readAt)
		if cacheable && ok && !issue.UpdatedAt.IsZero() && cached.updatedAt.Equal(issue.UpdatedAt) && age >= 0 && age < commentProjectionMaxAge {
			comments[issue.ID] = cached.comments
			projection[issue.ID] = cached
		} else {
			ids = append(ids, issue.ID)
		}
	}
	if _, ok := client.(*issuebackend.Feishu); ok {
		for _, issue := range issues {
			history, err := client.ListComments(ctx, issue.ID)
			if err != nil {
				if strings.Contains(err.Error(), "evidence conflict") {
					for path, repo := range r.Repositories {
						if repo.Env["AGENT_FEISHU_PROJECT"] != issue.Project.ID {
							continue
						}
						ticket := Ticket{ID: issue.ID, Identifier: issue.Identifier, Repo: path, Title: issue.Title, URL: issue.URL, MergeProblem: err.Error()}
						return nil, nil, &feishuEvidenceConflict{ticket: ticket, err: err}
					}
				}
				return nil, nil, err
			}
			comments[issue.ID] = history
		}
		return comments, nil, nil
	}
	rows, err := readSnapshotComments(ctx, client, ids)
	if err != nil {
		return nil, nil, err
	}
	for _, issue := range issues {
		if _, ok := comments[issue.ID]; ok {
			continue
		}
		history, ok := rows[issue.ID]
		if !ok {
			return nil, nil, errors.New("incomplete ownership snapshot")
		}
		comments[issue.ID] = append([]linear.Comment(nil), history...)
		projection[issue.ID] = commentProjection{updatedAt: issue.UpdatedAt, readAt: now, comments: append([]linear.Comment(nil), history...)}
	}
	if !cacheable {
		projection = nil
	}
	return comments, projection, nil
}

func (r *Runtime) verifyFreshOwnership(ctx context.Context, client LinearClient, t Ticket) (linear.Issue, error) {
	issue, err := r.claimIssue(ctx, client, t)
	if err != nil {
		return linear.Issue{}, err
	}
	comments, err := client.ListComments(ctx, t.ID)
	if err != nil {
		return linear.Issue{}, err
	}
	fresh := Ticket{ID: t.ID, Identifier: t.Identifier, Repo: t.Repo}
	if err := readOwnership(&fresh, comments); err != nil {
		return linear.Issue{}, err
	}
	readMergeHandoff(&fresh, comments)
	fresh.Resolution, err = readResolution(comments)
	if err != nil {
		return linear.Issue{}, err
	}
	if fresh.Owner != t.Owner || fresh.Phase != t.Phase || fresh.Terminal != t.Terminal || fresh.Run != t.Run || fresh.Attempt != t.Attempt || !reflect.DeepEqual(fresh.Handoff, t.Handoff) || !reflect.DeepEqual(fresh.Progress, t.Progress) || !reflect.DeepEqual(fresh.Resolution, t.Resolution) || fresh.MergeProblem != t.MergeProblem {
		return linear.Issue{}, errors.New("ownership or recovery evidence changed; resnapshot required")
	}
	closed := issue.State.Name == "Canceled" || issue.State.Name == "Cancelled" || issue.State.Name == "Duplicate" || (issue.State.Name == "Done" && fresh.Owner != machine.OwnerMerge)
	if closed && !DeploymentBarrier(fresh) {
		return linear.Issue{}, errors.New("issue closed before decision")
	}
	return issue, nil
}

func readSnapshotComments(ctx context.Context, client LinearClient, ids []string) (map[string][]linear.Comment, error) {
	if batch, ok := client.(interface {
		ListCommentsBatch(context.Context, []string) (map[string][]linear.Comment, error)
	}); ok {
		return batch.ListCommentsBatch(ctx, ids)
	}
	rows := map[string][]linear.Comment{}
	for _, id := range ids {
		comments, err := client.ListComments(ctx, id)
		if err != nil {
			return nil, err
		}
		rows[id] = comments
	}
	return rows, nil
}
