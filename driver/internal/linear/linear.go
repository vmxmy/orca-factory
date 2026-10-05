// Package linear provides a team-scoped Linear GraphQL client. Names are resolved
// from startup metadata; writes are never automatically replayed.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

const endpoint = "https://api.linear.app/graphql"

type Options struct {
	TeamID string
	// BaseURL overrides the GraphQL endpoint for tests. Redirects are forbidden.
	BaseURL    string
	HTTPClient *http.Client
}
type NamedID struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type IssueRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
}

// InverseRelation is a relation whose source is another issue; a `blocks`
// inverse relation means that issue blocks this one.
type InverseRelation struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Issue struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		State      struct {
			Type string `json:"type"`
		} `json:"state"`
	} `json:"issue"`
}
type Relation struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	RelatedIssue IssueRef `json:"relatedIssue"`
}
type PageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}
type Connection[T any] struct {
	Nodes    []T      `json:"nodes"`
	PageInfo PageInfo `json:"pageInfo"`
}
type Issue struct {
	Priority   int                  `json:"priority"`
	CreatedAt  time.Time            `json:"createdAt"`
	Project    NamedID              `json:"project"`
	ID         string               `json:"id"`
	Identifier string               `json:"identifier"`
	State      NamedID              `json:"state"`
	Labels     Connection[NamedID]  `json:"labels"`
	Relations  Connection[Relation] `json:"relations"`
	UpdatedAt  time.Time            `json:"updatedAt"`

	InverseRelations Connection[InverseRelation] `json:"inverseRelations"`
}

// Blocked reports whether any blocker is unresolved. A truncated blocker list
// counts as blocked so that a partial read can never admit a blocked ticket.
func (i Issue) Blocked() bool {
	if i.InverseRelations.PageInfo.HasNextPage {
		return true
	}
	for _, r := range i.InverseRelations.Nodes {
		if r.Type != "blocks" {
			continue
		}
		switch r.Issue.State.Type {
		case "completed", "canceled", "duplicate":
		default:
			return true
		}
	}
	return false
}

type Comment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type RateLimit struct {
	Limit     int64
	Remaining int64
	Reset     time.Time
}

// Usage is local consumption for the current UTC clock hour, with the latest
// server quota headers. Server quotas may also include other processes.
type Usage struct {
	Hour            time.Time
	Requests        int64
	Complexity      int64
	RequestLimit    RateLimit
	ComplexityLimit RateLimit
}
type Client struct {
	key, endpoint            string
	http                     *http.Client
	states, labels, projects map[string]string
	mu                       sync.Mutex
	usage                    Usage
	now                      func() time.Time
}

// New reads LINEAR_API_KEY and caches all team states, labels and projects.
// The client is safe for concurrent calls after construction.
func New(ctx context.Context, options Options) (*Client, error) {
	key := os.Getenv("LINEAR_API_KEY")
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("LINEAR_API_KEY is required")
	}
	if options.TeamID == "" {
		return nil, errors.New("Linear team ID is required")
	}
	target := options.BaseURL
	if target == "" {
		target = endpoint
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid Linear endpoint")
	}
	hc := http.Client{Timeout: 30 * time.Second}
	if options.HTTPClient != nil {
		hc = *options.HTTPClient
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := &Client{key: key, endpoint: target, http: &hc, now: time.Now}
	for _, item := range []struct {
		field string
		dest  *map[string]string
	}{{"states", &c.states}, {"labels", &c.labels}, {"projects", &c.projects}} {
		ids, err := c.metadata(ctx, options.TeamID, item.field)
		if err != nil {
			return nil, err
		}
		*item.dest = ids
	}
	return c, nil
}

func (c *Client) metadata(ctx context.Context, team, field string) (map[string]string, error) {
	ids := make(map[string]string)
	after := ""
	seen := map[string]bool{}
	query := `query Metadata($team: String!, $after: String) { team(id: $team) { ` + field + `(first: 100, after: $after) { nodes { id name } pageInfo { hasNextPage endCursor } } } }`
	for {
		var data struct {
			Team map[string]Connection[NamedID] `json:"team"`
		}
		if err := c.do(ctx, query, map[string]any{"team": team, "after": cursor(after)}, &data); err != nil {
			return nil, err
		}
		page, ok := data.Team[field]
		if !ok {
			return nil, protocolError()
		}
		for _, node := range page.Nodes {
			if node.ID == "" || node.Name == "" {
				return nil, protocolError()
			}
			if previous, ok := ids[node.Name]; ok && previous != node.ID {
				return nil, infra.Classify(errors.New("ambiguous Linear metadata name"))
			}
			ids[node.Name] = node.ID
		}
		if !page.PageInfo.HasNextPage {
			return ids, nil
		}
		next, err := nextCursor(page.PageInfo, seen)
		if err != nil {
			return nil, err
		}
		after = next
	}
}
func cursor(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nextCursor(info PageInfo, seen map[string]bool) (string, error) {
	if info.EndCursor == "" || seen[info.EndCursor] {
		return "", protocolError()
	}
	seen[info.EndCursor] = true
	return info.EndCursor, nil
}
func protocolError() error { return infra.Classify(errors.New("invalid Linear response")) }
func resolve(names []string, cache map[string]string) ([]string, error) {
	ids := make([]string, 0, len(names))
	for _, name := range names {
		id, ok := cache[name]
		if !ok {
			return nil, infra.Classify(errors.New("unknown Linear metadata name"))
		}
		ids = append(ids, id)
	}
	return ids, nil
}

const issueFields = `id identifier priority createdAt project { id name } updatedAt state { id name } labels(first: 20) { nodes { id name } pageInfo { hasNextPage endCursor } } relations(first: 20) { nodes { id type relatedIssue { id identifier } } pageInfo { hasNextPage endCursor } } inverseRelations(first: 50) { nodes { id type issue { id identifier state { type } } } pageInfo { hasNextPage endCursor } }`

var projectUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Snapshot queries all requested project names or UUIDs together, paging the issue
// connection. A zero updatedSince omits the updatedAt filter entirely.
func (c *Client) Snapshot(ctx context.Context, projects []string, updatedSince time.Time) ([]Issue, error) {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		if projectUUID.MatchString(project) {
			ids = append(ids, project)
			continue
		}
		resolved, err := resolve([]string{project}, c.projects)
		if err != nil {
			return nil, err
		}
		ids = append(ids, resolved[0])
	}
	issues := []Issue{}
	if len(ids) == 0 {
		return issues, nil
	}
	filter := map[string]any{"project": map[string]any{"id": map[string]any{"in": ids}}}
	if !updatedSince.IsZero() {
		filter["updatedAt"] = map[string]any{"gt": updatedSince.UTC().Format(time.RFC3339Nano)}
	}
	after := ""
	seen := map[string]bool{}
	for {
		var data struct {
			Issues *Connection[Issue] `json:"issues"`
		}
		err := c.do(ctx, `query Snapshot($filter: IssueFilter!, $after: String) { issues(filter: $filter, first: 20, after: $after) { nodes { `+issueFields+` } pageInfo { hasNextPage endCursor } } }`, map[string]any{"filter": filter, "after": cursor(after)}, &data)
		if err != nil {
			return nil, err
		}
		if data.Issues == nil {
			return nil, protocolError()
		}
		for i := range data.Issues.Nodes {
			if err := c.completeConnections(ctx, &data.Issues.Nodes[i]); err != nil {
				return nil, err
			}
		}
		issues = append(issues, data.Issues.Nodes...)
		if !data.Issues.PageInfo.HasNextPage {
			return issues, nil
		}
		after, err = nextCursor(data.Issues.PageInfo, seen)
		if err != nil {
			return nil, err
		}
	}
}

// completeConnections prevents nested label/relation connections from silently
// truncating a snapshot when an issue has more than one page of either.
func (c *Client) completeConnections(ctx context.Context, issue *Issue) error {
	for _, field := range []string{"labels", "relations"} {
		info := issue.Labels.PageInfo
		selection := `id name`
		if field == "relations" {
			info = issue.Relations.PageInfo
			selection = `id type relatedIssue { id identifier }`
		}
		seen := map[string]bool{}
		for info.HasNextPage {
			after, err := nextCursor(info, seen)
			if err != nil {
				return err
			}
			var data struct {
				Issue *struct {
					Labels    Connection[NamedID]  `json:"labels"`
					Relations Connection[Relation] `json:"relations"`
				} `json:"issue"`
			}
			err = c.do(ctx, `query IssueConnection($id: String!, $after: String) { issue(id: $id) { `+field+`(first: 100, after: $after) { nodes { `+selection+` } pageInfo { hasNextPage endCursor } } } }`, map[string]any{"id": issue.ID, "after": after}, &data)
			if err != nil {
				return err
			}
			if data.Issue == nil {
				return protocolError()
			}
			if field == "labels" {
				issue.Labels.Nodes = append(issue.Labels.Nodes, data.Issue.Labels.Nodes...)
				info = data.Issue.Labels.PageInfo
				issue.Labels.PageInfo = info
			} else {
				issue.Relations.Nodes = append(issue.Relations.Nodes, data.Issue.Relations.Nodes...)
				info = data.Issue.Relations.PageInfo
				issue.Relations.PageInfo = info
			}
		}
	}
	return nil
}

// UpdateIssue performs exactly one mutation and returns its issue selection.
// Empty state preserves the current state. Nested connection pageInfo is exposed
// to callers; no read-back request is made even for large label/relation lists.
func (c *Client) UpdateIssue(ctx context.Context, id, state string, addLabels, removeLabels []string) (Issue, error) {
	add, err := resolve(addLabels, c.labels)
	if err != nil {
		return Issue{}, err
	}
	remove, err := resolve(removeLabels, c.labels)
	if err != nil {
		return Issue{}, err
	}
	input := map[string]any{"addedLabelIds": add, "removedLabelIds": remove}
	if state != "" {
		states, err := resolve([]string{state}, c.states)
		if err != nil {
			return Issue{}, err
		}
		input["stateId"] = states[0]
	}
	var data struct {
		IssueUpdate struct {
			Success bool   `json:"success"`
			Issue   *Issue `json:"issue"`
		} `json:"issueUpdate"`
	}
	err = c.do(ctx, `mutation UpdateIssue($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success issue { `+issueFields+` } } }`, map[string]any{"id": id, "input": input}, &data)
	if err != nil {
		return Issue{}, err
	}
	if !data.IssueUpdate.Success || data.IssueUpdate.Issue == nil || data.IssueUpdate.Issue.ID == "" {
		return Issue{}, protocolError()
	}
	return *data.IssueUpdate.Issue, nil
}
func (c *Client) CreateComment(ctx context.Context, id, body string) (Comment, error) {
	var data struct {
		CommentCreate struct {
			Success bool     `json:"success"`
			Comment *Comment `json:"comment"`
		} `json:"commentCreate"`
	}
	err := c.do(ctx, `mutation CreateComment($input: CommentCreateInput!) { commentCreate(input: $input) { success comment { id body createdAt updatedAt } } }`, map[string]any{"input": map[string]any{"issueId": id, "body": body}}, &data)
	if err != nil {
		return Comment{}, err
	}
	if !data.CommentCreate.Success || data.CommentCreate.Comment == nil || data.CommentCreate.Comment.ID == "" {
		return Comment{}, protocolError()
	}
	return *data.CommentCreate.Comment, nil
}
func (c *Client) ListComments(ctx context.Context, id string) ([]Comment, error) {
	comments := []Comment{}
	after := ""
	seen := map[string]bool{}
	for {
		var data struct {
			Issue *struct {
				Comments Connection[Comment] `json:"comments"`
			} `json:"issue"`
		}
		err := c.do(ctx, `query ListComments($id: String!, $after: String) { issue(id: $id) { comments(first: 100, after: $after) { nodes { id body createdAt updatedAt } pageInfo { hasNextPage endCursor } } } }`, map[string]any{"id": id, "after": cursor(after)}, &data)
		if err != nil {
			return nil, err
		}
		if data.Issue == nil {
			return nil, protocolError()
		}
		comments = append(comments, data.Issue.Comments.Nodes...)
		if !data.Issue.Comments.PageInfo.HasNextPage {
			return comments, nil
		}
		after, err = nextCursor(data.Issue.Comments.PageInfo, seen)
		if err != nil {
			return nil, err
		}
	}
}

func (c *Client) do(ctx context.Context, query string, variables map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return protocolError()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return protocolError()
	}
	req.Header.Set("Authorization", c.key)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	c.record(response)
	if err != nil {
		// Classify before discarding transport errors, which can contain URLs or keys.
		classified := infra.Classify(err)
		safe := errors.New("Linear transport failed")
		var transient *infra.Transient
		if errors.As(classified, &transient) {
			return &infra.Transient{Err: safe}
		}
		if ctx.Err() != nil {
			return infra.Classify(ctx.Err())
		}
		return &infra.Ticket{Err: safe}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return &infra.Transient{Err: errors.New("Linear response read failed")}
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	decodeErr := json.Unmarshal(raw, &envelope)
	for _, e := range envelope.Errors {
		if e.Extensions.Code == "RATELIMITED" {
			return &infra.Global{Dep: infra.Linear, Err: errors.New("Linear rate limited")}
		}
	}
	if response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 429 {
		return &infra.Global{Dep: infra.Linear, Err: infra.HTTPError{StatusCode: response.StatusCode}}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return infra.Classify(infra.HTTPError{StatusCode: response.StatusCode})
	}
	if decodeErr != nil || len(envelope.Errors) > 0 || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return protocolError()
	}
	if json.Unmarshal(envelope.Data, out) != nil {
		return protocolError()
	}
	return nil
}
func (c *Client) rotateHour() {
	hour := c.now().UTC().Truncate(time.Hour)
	if !hour.Equal(c.usage.Hour) {
		c.usage.Hour = hour
		c.usage.Requests = 0
		c.usage.Complexity = 0
	}
}
func (c *Client) record(response *http.Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rotateHour()
	c.usage.Requests++
	if response == nil {
		return
	}
	h := response.Header
	if n, err := strconv.ParseInt(h.Get("X-Complexity"), 10, 64); err == nil && n >= 0 {
		c.usage.Complexity += n
	}
	for _, item := range []struct {
		prefix string
		limit  *RateLimit
	}{{"X-RateLimit-Requests-", &c.usage.RequestLimit}, {"X-RateLimit-Complexity-", &c.usage.ComplexityLimit}} {
		if n, err := strconv.ParseInt(h.Get(item.prefix+"Limit"), 10, 64); err == nil && n >= 0 {
			item.limit.Limit = n
		}
		if n, err := strconv.ParseInt(h.Get(item.prefix+"Remaining"), 10, 64); err == nil && n >= 0 {
			item.limit.Remaining = n
		}
		if n, err := strconv.ParseInt(h.Get(item.prefix+"Reset"), 10, 64); err == nil && n >= 0 {
			item.limit.Reset = time.UnixMilli(n).UTC()
		}
	}
}
func (c *Client) Usage() Usage { c.mu.Lock(); defer c.mu.Unlock(); c.rotateHour(); return c.usage }
