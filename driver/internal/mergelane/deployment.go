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
		type workflowRun struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Head       string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		}
		var latest workflowRun
		count := 0
		for page := 1; ; page++ {
			var runs struct {
				Total *int          `json:"total_count"`
				Runs  []workflowRun `json:"workflow_runs"`
			}
			endpoint := fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100&page=%d", r.Repository, url.QueryEscape(sha), page)
			b, err := l.run(ctx, r.Path, "gh", "api", endpoint)
			if err != nil {
				return "", errors.New("deployment workflow read unavailable")
			}
			if err := json.Unmarshal(b, &runs); err != nil || runs.Total == nil {
				return "", errors.New("invalid deployment workflow receipt")
			}
			for _, run := range runs.Runs {
				if run.Head == sha && run.Name == r.DeployWorkflow && run.ID > latest.ID {
					latest = run
				}
			}
			count += len(runs.Runs)
			if count > *runs.Total {
				return "", errors.New("inconsistent deployment workflow receipt")
			}
			if len(runs.Runs) < 100 || page == 10 {
				if count != *runs.Total {
					return "", errors.New("incomplete deployment workflow receipt")
				}
				break
			}
		}
		if latest.ID == 0 || latest.Status != "completed" {
			return merge.DeploymentPending, nil
		}
		if latest.Conclusion == "" {
			return "", errors.New("deployment conclusion missing")
		}
		if latest.Conclusion != "success" {
			return merge.DeploymentFailed, nil
		}

	}
	if r.SmokeCommand != "" {
		if _, err := l.run(ctx, r.Path, "bash", "-c", r.SmokeCommand); err != nil {
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
