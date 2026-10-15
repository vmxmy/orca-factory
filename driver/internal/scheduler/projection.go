package scheduler

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

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

type snapshotReadFailures struct {
	byID map[string]error
}

func (e *snapshotReadFailures) Error() string {
	var messages []string
	for _, id := range slices.Sorted(maps.Keys(e.byID)) {
		messages = append(messages, id+": "+e.byID[id].Error())
	}
	return strings.Join(messages, "; ")
}

func (e *snapshotReadFailures) Unwrap() []error {
	var failures []error
	for _, id := range slices.Sorted(maps.Keys(e.byID)) {
		failures = append(failures, e.byID[id])
	}
	return failures
}

func readSnapshotComments(ctx context.Context, client LinearClient, ids []string) (map[string][]linear.Comment, error) {
	if batch, ok := client.(interface {
		ListCommentsBatch(context.Context, []string) (map[string][]linear.Comment, error)
	}); ok {
		return batch.ListCommentsBatch(ctx, ids)
	}
	rows := map[string][]linear.Comment{}
	failures := map[string]error{}
	for _, id := range ids {
		comments, err := client.ListComments(ctx, id)
		if err != nil {
			failures[id] = err
			continue
		}
		rows[id] = comments
	}
	if len(failures) != 0 {
		return nil, &snapshotReadFailures{byID: failures}
	}
	return rows, nil
}
