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
	NodeID      string `json:"node_id"`
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
	cursors := map[string]bool{}
	path := repoPath(owner, repo) + "/issues?state=all&per_page=100&page=1&sort=created&direction=asc"
	for page := 1; page <= 10000; page++ {
		current, err := url.Parse(path)
		if err != nil {
			return nil, fmt.Errorf("parse GitHub pagination path: %w", err)
		}
		if cursor := current.Query().Get("after"); cursor != "" {
			if cursors[cursor] {
				return nil, errors.New("repeated GitHub pagination cursor")
			}
			cursors[cursor] = true
		}
		var batch []IssueRecord
		next, err := c.issuePage(ctx, owner, repo, path, page, &batch)
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
		if next == "" && len(batch) < 100 {
			return rows, nil
		}
		if next == "" {
			next = nextIssuePage(path, page+1)
		}
		path = next
	}
	return nil, errors.New("GitHub issue pagination limit exceeded")
}
func (c *Client) IssueComments(ctx context.Context, owner, repo string, number int) ([]IssueComment, error) {
	rows := []IssueComment{}
	seen := map[int64]bool{}
	cursors := map[string]bool{}
	path := fmt.Sprintf("%s/issues/%d/comments?per_page=100&page=1", repoPath(owner, repo), number)
	for page := 1; page <= 10000; page++ {
		current, err := url.Parse(path)
		if err != nil {
			return nil, fmt.Errorf("parse GitHub pagination path: %w", err)
		}
		if cursor := current.Query().Get("after"); cursor != "" {
			if cursors[cursor] {
				return nil, errors.New("repeated GitHub pagination cursor")
			}
			cursors[cursor] = true
		}
		var batch []IssueComment
		next, err := c.issuePage(ctx, owner, repo, path, page, &batch)
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
		if next == "" && len(batch) < 100 {
			return rows, nil
		}
		if next == "" {
			next = nextIssuePage(path, page+1)
		}
		path = next
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

func nextIssuePage(path string, page int) string {
	next, _ := url.Parse(path)
	query := next.Query()
	query.Set("page", strconv.Itoa(page))
	next.RawQuery = query.Encode()
	return next.String()
}

// Link headers are cached with the response. Preserve cursors only after checking
// the origin, repository identity, collection and monotonically advancing page.
func (c *Client) issuePage(ctx context.Context, owner, repo, path string, page int, out any) (string, error) {
	var link string
	if err := c.requestDetails(ctx, http.MethodGet, path, nil, out, &link); err != nil {
		return "", err
	}
	var result string
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		raw, _, ok := strings.Cut(strings.TrimSpace(part), ">;")
		if !ok || !strings.HasPrefix(raw, "<") || result != "" {
			return "", errors.New("invalid GitHub next link")
		}
		next, err := url.Parse(strings.TrimPrefix(raw, "<"))
		expected, _ := url.Parse(c.base + path)
		if err != nil || next.Scheme != expected.Scheme || next.Host != expected.Host || next.User != nil || next.Fragment != "" || next.RawPath != "" || !issueQueryMatches(expected, next, page+1) {
			return "", errors.New("unsafe or repeated GitHub next link")
		}
		if next.Path != expected.Path {
			if err := c.canonicalIssuePath(ctx, owner, repo, expected.Path, next.Path); err != nil {
				return "", err
			}
		}
		result = strings.TrimPrefix(next.String(), c.base)
	}
	return result, nil
}

func issueQueryMatches(current, next *url.URL, page int) bool {
	query, err := url.ParseQuery(next.RawQuery)
	if err != nil || query.Get("page") != strconv.Itoa(page) {
		return false
	}
	original := current.Query()
	for key, values := range query {
		if len(values) != 1 || (key != "page" && key != "after" && (len(original[key]) != 1 || values[0] != original.Get(key))) {
			return false
		}
	}
	for key, values := range original {
		if key != "page" && key != "after" && (len(query[key]) != 1 || query.Get(key) != values[0]) {
			return false
		}
	}
	return true
}

func (c *Client) canonicalIssuePath(ctx context.Context, owner, repo, current, next string) error {
	base, _ := url.Parse(c.base)
	prefix := base.Path + repoPath(owner, repo)
	suffix, ok := strings.CutPrefix(current, prefix+"/issues")
	if !ok {
		return errors.New("GitHub pagination changed collection")
	}
	var identity struct {
		ID   int64  `json:"id"`
		Name string `json:"full_name"`
	}
	if err := c.request(ctx, http.MethodGet, repoPath(owner, repo), nil, &identity); err != nil {
		return err
	}
	if identity.ID <= 0 || !strings.EqualFold(identity.Name, owner+"/"+repo) || next != fmt.Sprintf("%s/repositories/%d/issues%s", base.Path, identity.ID, suffix) {
		return errors.New("GitHub pagination repository identity mismatch")
	}
	return nil
}
