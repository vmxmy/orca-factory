package mergelane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

// Check waits for the configured workflow at the confirmed squash SHA, then
// runs smoke. It never treats a missing, mismatched or unreadable run as passed.
func (l *Lane) Check(ctx context.Context, r merge.Request, sha string) (merge.DeploymentState, error) {
	for _, e := range l.entries {
		if e.Request.Repository == r.Repository && e.MergeSHA == sha && e.DeploymentReady {
			return merge.DeploymentPassed, nil
		}
	}
	if sha == "" {
		return "", errors.New("deployment squash SHA missing")
	}
	if r.DeployWorkflow != "" {
		var runs struct {
			Runs []struct {
				ID         int64  `json:"id"`
				Head       string `json:"head_sha"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"workflow_runs"`
		}
		endpoint := fmt.Sprintf("repos/%s/actions/workflows/%s/runs?head_sha=%s&per_page=100&page=1", r.Repository, url.PathEscape(r.DeployWorkflow), url.QueryEscape(sha))
		b, err := l.run(ctx, r.Path, "gh", "api", endpoint)
		if err != nil {
			return "", errors.New("deployment workflow read unavailable")
		}
		if err := json.Unmarshal(b, &runs); err != nil {
			return "", errors.New("invalid deployment workflow receipt")
		}
		latest := -1
		for i, run := range runs.Runs {
			if run.Head == sha && (latest < 0 || run.ID > runs.Runs[latest].ID) {
				latest = i
			}
		}
		if latest < 0 || runs.Runs[latest].Status != "completed" {
			return merge.DeploymentPending, nil
		}
		if runs.Runs[latest].Conclusion == "" {
			return "", errors.New("deployment conclusion missing")
		}
		if runs.Runs[latest].Conclusion != "success" {
			return merge.DeploymentFailed, nil
		}
	}
	if r.SmokeCommand != "" {
		if _, err := l.run(ctx, r.Path, "bash", "-lc", r.SmokeCommand); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return merge.SmokeFailed, nil
		}
	}
	// Persist smoke completion before publication or another merge is allowed.
	for i := range l.entries {
		e := &l.entries[i]
		if e.Request.Repository == r.Repository && e.MergeSHA == sha {
			e.DeploymentReady = true
			if err := l.save(); err != nil {
				e.DeploymentReady = false
				return "", err
			}
			break
		}
	}
	return merge.DeploymentPassed, nil
}
