package github

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

type PullRequest struct {
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	Merged         bool   `json:"merged"`
	MergeableState string `json:"mergeable_state"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}
type CreatePRRequest struct {
	Head  string `json:"head"`
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Draft bool   `json:"draft,omitempty"`
}

// FindOpenPR accepts a branch or owner:branch and returns nil when none exists.
func (c *Client) FindOpenPR(ctx context.Context, owner, repo, head string) (*PullRequest, error) {
	if !strings.Contains(head, ":") {
		head = owner + ":" + head
	}
	var prs []PullRequest
	q := url.Values{"state": {"open"}, "head": {head}, "per_page": {"100"}}
	err := c.request(ctx, http.MethodGet, repoPath(owner, repo)+"/pulls?"+q.Encode(), nil, &prs)
	if err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}

// CreatePR serializes find/create in this client and recovers uncertain writes
// by reading before returning an error. It never blindly replays a POST.
func (c *Client) CreatePR(ctx context.Context, owner, repo string, input CreatePRRequest) (PullRequest, error) {
	c.createMu.Lock()
	defer c.createMu.Unlock()
	pr, err := c.FindOpenPR(ctx, owner, repo, input.Head)
	if err != nil {
		return PullRequest{}, err
	}
	if pr != nil {
		return *pr, nil
	}
	var created PullRequest
	err = c.request(ctx, http.MethodPost, repoPath(owner, repo)+"/pulls", input, &created)
	if err == nil {
		return created, nil
	}
	pr, readErr := c.FindOpenPR(ctx, owner, repo, input.Head)
	if readErr != nil {
		return PullRequest{}, readErr
	}
	if pr != nil {
		return *pr, nil
	}
	return PullRequest{}, err
}

func (c *Client) GetPR(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	var pr PullRequest
	err := c.request(ctx, http.MethodGet, repoPath(owner, repo)+"/pulls/"+strconv.Itoa(number), nil, &pr)
	return pr, err
}

// MergePR pins the expected head and confirms the authoritative merged field,
// including when the merge response is lost. False read-back is retryable.
func (c *Client) MergePR(ctx context.Context, owner, repo string, number int, sha string) (PullRequest, error) {
	writeErr := c.request(ctx, http.MethodPut, repoPath(owner, repo)+"/pulls/"+strconv.Itoa(number)+"/merge", map[string]string{"merge_method": "squash", "sha": sha}, nil)
	pr, err := c.GetPR(ctx, owner, repo, number)
	if err != nil {
		return pr, err
	}
	if pr.Merged {
		return pr, nil
	}
	if writeErr != nil {
		return pr, writeErr
	}
	return pr, infra.Classify(infra.ErrWriteUnconfirmed)
}

type File struct {
	Filename         string `json:"filename"`
	Status           string `json:"status"`
	PreviousFilename string `json:"previous_filename,omitempty"`
}

// CompareFiles returns GitHub's comparison file list (at most 300 files).
func (c *Client) CompareFiles(ctx context.Context, owner, repo, base, head string) ([]File, error) {
	var result struct {
		Files []File `json:"files"`
	}
	err := c.request(ctx, http.MethodGet, repoPath(owner, repo)+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head), nil, &result)
	return result.Files, err
}

type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadSHA    string `json:"head_sha"`
}

func (c *Client) CheckRuns(ctx context.Context, owner, repo, ref string) ([]CheckRun, error) {
	var all []CheckRun
	for page := 1; ; page++ {
		var result struct {
			CheckRuns []CheckRun `json:"check_runs"`
		}
		err := c.request(ctx, http.MethodGet, repoPath(owner, repo)+"/commits/"+url.PathEscape(ref)+"/check-runs?per_page=100&page="+strconv.Itoa(page), nil, &result)
		if err != nil {
			return nil, err
		}
		all = append(all, result.CheckRuns...)
		if len(result.CheckRuns) < 100 {
			return all, nil
		}
	}
}

// CreateTag creates a lightweight tag referencing an existing commit.
func (c *Client) CreateTag(ctx context.Context, owner, repo, tag, sha string) error {
	return c.request(ctx, http.MethodPost, repoPath(owner, repo)+"/git/refs", map[string]string{"ref": "refs/tags/" + tag, "sha": sha}, nil)
}

type ReleaseRequest struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	Body       string `json:"body,omitempty"`
	Draft      bool   `json:"draft,omitempty"`
	Prerelease bool   `json:"prerelease,omitempty"`
}
type Release struct {
	ID      int64  `json:"id"`
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func (c *Client) CreateRelease(ctx context.Context, owner, repo string, input ReleaseRequest) (Release, error) {
	var release Release
	err := c.request(ctx, http.MethodPost, repoPath(owner, repo)+"/releases", input, &release)
	return release, err
}
