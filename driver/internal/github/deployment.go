package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var workflowName = regexp.MustCompile(`^([1-9][0-9]*|[A-Za-z0-9_.-]+\.ya?ml)$`)

// ValidWorkflow also accepts the existing display-name configuration contract.
func ValidWorkflow(workflow string) bool {
	return workflow != "" && strings.TrimSpace(workflow) == workflow && !strings.ContainsAny(workflow, "/\\{}") && strings.IndexFunc(workflow, unicode.IsControl) < 0
}

// Deployment observes only the configured workflow at the actual squash SHA.
// All pages must be complete; unknown states and ambiguous receipts stay pending.
func (c *Client) Deployment(ctx context.Context, owner, repo, workflow, sha string) (string, error) {
	if !ValidWorkflow(workflow) {
		return "pending", errors.New("invalid deployment workflow")
	}
	if !workflowName.MatchString(workflow) {
		id, err := c.workflowID(ctx, owner, repo, workflow)
		if err != nil {
			return "pending", err
		}
		workflow = strconv.FormatInt(id, 10)
	}
	type run struct {
		ID         int64  `json:"id"`
		Workflow   int64  `json:"workflow_id"`
		Path       string `json:"path"`
		SHA        string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	var latest run
	seen := map[int64]bool{}
	total := -1
	for page := 1; page <= 10; page++ {
		var v struct {
			Total *int   `json:"total_count"`
			Runs  *[]run `json:"workflow_runs"`
		}
		err := c.request(ctx, http.MethodGet, repoPath(owner, repo)+"/actions/workflows/"+url.PathEscape(workflow)+"/runs?head_sha="+url.QueryEscape(sha)+"&per_page=100&page="+strconv.Itoa(page), nil, &v)
		if err != nil {
			return "pending", err
		}
		if v.Total == nil || v.Runs == nil || *v.Total < 0 || (total >= 0 && total != *v.Total) {
			return "pending", nil
		}
		total = *v.Total
		for _, r := range *v.Runs {
			file, _, _ := strings.Cut(r.Path, "@")
			if r.ID <= 0 || seen[r.ID] || r.SHA != sha || r.Workflow <= 0 || (strconv.FormatInt(r.Workflow, 10) != workflow && file != ".github/workflows/"+workflow) {
				return "pending", nil
			}
			if r.Status != "completed" {
				return "pending", nil
			}
			seen[r.ID] = true
			if r.ID > latest.ID {
				latest = r
			}
		}
		if len(seen) == total {
			if latest.ID == 0 || latest.Status != "completed" {
				return "pending", nil
			}
			switch latest.Conclusion {
			case "success":
				return "passed", nil
			case "failure", "cancelled", "timed_out", "action_required", "startup_failure":
				return "failed", nil
			default:
				return "pending", nil
			}
		}
		if len(*v.Runs) == 0 || len(seen) > total {
			return "pending", nil
		}
	}
	return "pending", nil
}

// Resolve display names across the complete collection; duplicates fail closed.
func (c *Client) workflowID(ctx context.Context, owner, repo, name string) (int64, error) {
	total := -1
	seen := map[int64]bool{}
	var found int64
	for page := 1; page <= 100; page++ {
		var v struct {
			Total     *int `json:"total_count"`
			Workflows *[]struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"workflows"`
		}
		if err := c.request(ctx, http.MethodGet, repoPath(owner, repo)+"/actions/workflows?per_page=100&page="+strconv.Itoa(page), nil, &v); err != nil {
			return 0, err
		}
		if v.Total == nil || v.Workflows == nil || *v.Total < 0 || (total >= 0 && total != *v.Total) {
			break
		}
		total = *v.Total
		for _, w := range *v.Workflows {
			if w.ID <= 0 || seen[w.ID] {
				return 0, errors.New("incomplete workflows")
			}
			seen[w.ID] = true
			if w.Name == name {
				if found != 0 {
					return 0, errors.New("ambiguous deployment workflow")
				}
				found = w.ID
			}
		}
		if len(seen) == total {
			if found != 0 {
				return found, nil
			}
			break
		}
		if len(*v.Workflows) == 0 || len(seen) > total {
			break
		}
	}
	return 0, errors.New("deployment workflow unresolved")
}
