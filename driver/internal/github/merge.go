package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (c *Client) MainSHA(ctx context.Context, owner, repo string) (string, error) {
	var v struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := c.request(ctx, http.MethodGet, repoPath(owner, repo)+"/git/ref/heads/main", nil, &v)
	return v.Object.SHA, err
}

// CheckResult reports the pinned head's check-runs. Failed and FailedNames
// are only meaningful once every check-run has reached a terminal status;
// a still-running run leaves Failed false regardless of other conclusions.
type CheckResult struct {
	Green       bool
	Failed      bool
	FailedNames []string
}

// checkRunFailureConclusions mirrors the terminal-failure set classified for
// deployment workflow runs in deployment.go.
var checkRunFailureConclusions = map[string]bool{
	"failure": true, "cancelled": true, "timed_out": true, "action_required": true, "startup_failure": true,
}

// MergeChecks requires an explicitly complete collection, read at the pinned
// head. Green requires a nonempty set of successful checks/statuses; unknown
// is not green. Path-filtered CI jobs conclude skipped/neutral, so those count
// as passing once at least one check-run truly succeeded; an all
// skipped/neutral set proves nothing ran and is not green. Failed additionally
// requires every check-run to have completed, with at least one reaching a
// terminal failure conclusion.
func (c *Client) MergeChecks(ctx context.Context, owner, repo, head string) (CheckResult, error) {
	count := 0
	expectedChecks := -1
	seenChecks := map[int64]bool{}
	allComplete := true
	allSuccess := true
	hasSuccess := false
	var failedNames []string
	for page := 1; ; page++ {
		var v struct {
			Total *int        `json:"total_count"`
			Runs  *[]CheckRun `json:"check_runs"`
		}
		err := c.request(ctx, http.MethodGet, repoPath(owner, repo)+"/commits/"+url.PathEscape(head)+"/check-runs?per_page=100&page="+strconv.Itoa(page), nil, &v)
		if err != nil {
			return CheckResult{}, err
		}
		if v.Total == nil || v.Runs == nil {
			return CheckResult{}, errors.New("incomplete check runs")
		}
		if expectedChecks < 0 {
			expectedChecks = *v.Total
		}
		if expectedChecks != *v.Total || *v.Total < 0 {
			return CheckResult{}, errors.New("check collection changed")
		}
		for _, r := range *v.Runs {
			if r.ID <= 0 || seenChecks[r.ID] {
				return CheckResult{}, errors.New("invalid or repeated check identity")
			}
			seenChecks[r.ID] = true
			if r.HeadSHA != head {
				return CheckResult{}, nil
			}
			if r.Status != "completed" {
				allComplete = false
				continue
			}
			switch r.Conclusion {
			case "success":
				hasSuccess = true
			case "skipped", "neutral":
				// Counts toward a pass only when paired with at least one real success (checked via hasSuccess below).
			default:
				allSuccess = false
				if checkRunFailureConclusions[r.Conclusion] {
					failedNames = append(failedNames, r.Name)
				}
			}
		}
		count += len(*v.Runs)
		if count == *v.Total {
			break
		}
		if len(*v.Runs) == 0 || count > *v.Total {
			return CheckResult{}, errors.New("incomplete check pagination")
		}
	}
	statuses := 0
	expectedStatuses := -1
	seenStatuses := map[string]bool{}
	for page := 1; ; page++ {
		var v struct {
			Total    *int `json:"total_count"`
			Statuses *[]struct {
				State   string `json:"state"`
				Context string `json:"context"`
			} `json:"statuses"`
		}
		err := c.request(ctx, http.MethodGet, repoPath(owner, repo)+"/commits/"+url.PathEscape(head)+"/status?per_page=100&page="+strconv.Itoa(page), nil, &v)
		if err != nil {
			return CheckResult{}, err
		}
		if v.Total == nil || v.Statuses == nil {
			return CheckResult{}, errors.New("incomplete commit statuses")
		}
		if expectedStatuses < 0 {
			expectedStatuses = *v.Total
		}
		if expectedStatuses != *v.Total || *v.Total < 0 {
			return CheckResult{}, errors.New("status collection changed")
		}
		for _, s := range *v.Statuses {
			if seenStatuses[s.Context] {
				return CheckResult{}, errors.New("repeated status context")
			}
			seenStatuses[s.Context] = true
			if s.State != "success" || s.Context == "" {
				return CheckResult{}, nil
			}
		}
		statuses += len(*v.Statuses)
		if statuses == *v.Total {
			break
		}
		if len(*v.Statuses) == 0 || statuses > *v.Total {
			return CheckResult{}, errors.New("incomplete status pagination")
		}
	}
	return CheckResult{
		Green:       allComplete && allSuccess && (hasSuccess || statuses > 0),
		Failed:      allComplete && len(failedNames) > 0,
		FailedNames: failedNames,
	}, nil
}

// UniqueTicketPR refuses multiple units or candidates. #141 supports only the
// directly mergeable single-PR ticket; other shapes retain merge ownership.
// After merge, require no remaining open candidate before completing the ticket.
func (c *Client) UniqueTicketPR(ctx context.Context, owner, repo, identifier string, number int, merged bool) error {
	found := 0
	for page := 1; page <= 1000; page++ {
		var rows []PullRequest
		err := c.request(ctx, http.MethodGet, repoPath(owner, repo)+"/pulls?state=open&per_page=100&page="+strconv.Itoa(page), nil, &rows)
		if err != nil {
			return err
		}
		if rows == nil {
			return errors.New("incomplete PR enumeration")
		}
		for _, p := range rows {
			if p.Head.Ref == "" || p.Number <= 0 {
				return errors.New("incomplete PR identity")
			}
			if strings.HasPrefix(p.Head.Ref, "agent/"+identifier+"/") {
				found++
				if merged || p.Number != number {
					return errors.New("multiple ticket PRs")
				}
			}
		}
		if len(rows) < 100 {
			want := 1
			if merged {
				want = 0
			}
			if found != want {
				return errors.New("ambiguous ticket PR")
			}
			return nil
		}
	}
	return errors.New("PR enumeration limit")
}
