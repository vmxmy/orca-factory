package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// IssueRecord is the REST issue representation; pull requests are excluded by callers.
type IssueRecord struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	URL         string `json:"html_url"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest json.RawMessage `json:"pull_request"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}
type IssueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (c *Client) Issues(ctx context.Context, owner, repo string) ([]IssueRecord, error) {
	rows := []IssueRecord{}
	seen := map[int]bool{}
	for page := 1; page <= 10000; page++ {
		var batch []IssueRecord
		next, err := c.issuePage(ctx, fmt.Sprintf("%s/issues?state=all&per_page=100&page=%d&sort=created&direction=asc", repoPath(owner, repo), page), page, &batch)
		if err != nil {
			return nil, err
		}
		for _, v := range batch {
			if v.Number <= 0 || seen[v.Number] {
				return nil, errors.New("incomplete GitHub issue pagination")
			}
			seen[v.Number] = true
			rows = append(rows, v)
		}
		if !next && len(batch) < 100 {
			return rows, nil
		}
	}
	return nil, errors.New("GitHub issue pagination limit exceeded")
}
func (c *Client) IssueComments(ctx context.Context, owner, repo string, number int) ([]IssueComment, error) {
	rows := []IssueComment{}
	seen := map[int64]bool{}
	for page := 1; page <= 10000; page++ {
		var batch []IssueComment
		next, err := c.issuePage(ctx, fmt.Sprintf("%s/issues/%d/comments?per_page=100&page=%d", repoPath(owner, repo), number, page), page, &batch)
		if err != nil {
			return nil, err
		}
		for _, v := range batch {
			if v.ID <= 0 || seen[v.ID] {
				return nil, errors.New("incomplete GitHub comment pagination")
			}
			seen[v.ID] = true
			rows = append(rows, v)
		}
		if !next && len(batch) < 100 {
			return rows, nil
		}
	}
	return nil, errors.New("GitHub comment pagination limit exceeded")
}
func (c *Client) CreateIssue(ctx context.Context, owner, repo string, input any) (IssueRecord, error) {
	var v IssueRecord
	err := c.request(ctx, http.MethodPost, repoPath(owner, repo)+"/issues", input, &v)
	return v, err
}
func (c *Client) EditIssue(ctx context.Context, owner, repo string, number int, input any) (IssueRecord, error) {
	var v IssueRecord
	err := c.request(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", repoPath(owner, repo), number), input, &v)
	return v, err
}
func (c *Client) AddIssueComment(ctx context.Context, owner, repo string, number int, body string) (IssueComment, error) {
	var v IssueComment
	err := c.request(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", repoPath(owner, repo), number), map[string]string{"body": body}, &v)
	return v, err
}

// Link headers are part of the cached representation. Never follow an arbitrary
// remote URL or infer exhaustion solely from the number of returned records.
func (c *Client) issuePage(ctx context.Context, path string, page int, out any) (bool, error) {
	var link string
	if err := c.requestDetails(ctx, http.MethodGet, path, nil, out, &link); err != nil {
		return false, err
	}
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		raw, _, ok := strings.Cut(strings.TrimSpace(part), ">;")
		if !ok || !strings.HasPrefix(raw, "<") {
			return false, errors.New("invalid GitHub next link")
		}
		next, err := url.Parse(strings.TrimPrefix(raw, "<"))
		expected, _ := url.Parse(c.base + path)
		if err != nil || next.Scheme != expected.Scheme || next.Host != expected.Host || next.Path != expected.Path || next.User != nil || next.Fragment != "" || next.Query().Get("page") != strconv.Itoa(page+1) {
			return false, errors.New("unsafe or repeated GitHub next link")
		}
		return true, nil
	}
	return false, nil
}
