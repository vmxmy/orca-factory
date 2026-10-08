package linear

import (
	"context"
	"encoding/json"
	"errors"
)

type ExportIssue struct {
	Issue
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Estimate    *float64 `json:"estimate"`
	Parent      *struct {
		ID string `json:"id"`
	} `json:"parent"`
}

func (c *Client) ExportIssues(ctx context.Context, project string) ([]ExportIssue, error) {
	if project == "" {
		return nil, errors.New("source project required")
	}
	if !projectUUID.MatchString(project) {
		ids, err := resolve([]string{project}, c.projects)
		if err != nil {
			return nil, err
		}
		project = ids[0]
	}
	ctx = context.WithValue(ctx, discoveryReadKey{}, true)
	filter := map[string]any{"project": map[string]any{"id": map[string]any{"eq": project}}}
	result := []ExportIssue{}
	after := ""
	seen, identities := map[string]bool{}, map[string]bool{}
	for {
		var data struct {
			Issues *completeConnection[json.RawMessage] `json:"issues"`
		}
		if err := c.do(ctx, `query ExportIssues($filter: IssueFilter!, $after: String) { issues(filter: $filter, first: 20, after: $after, includeArchived: true) { nodes { `+issueFields+` title description estimate parent { id } } pageInfo { hasNextPage endCursor } }`, map[string]any{"filter": filter, "after": cursor(after)}, &data); err != nil {
			return nil, err
		}
		if data.Issues == nil {
			return nil, protocolError()
		}
		for _, raw := range data.Issues.Nodes {
			row, err := c.exportIssue(ctx, raw, project)
			if err != nil {
				return nil, err
			}
			if identities[row.ID] {
				return nil, protocolError()
			}
			identities[row.ID] = true
			result = append(result, row)
		}
		if !data.Issues.PageInfo.HasNextPage {
			return result, nil
		}
		var err error
		after, err = nextCursor(data.Issues.PageInfo, seen)
		if err != nil {
			return nil, err
		}
	}
}

func (c *Client) exportIssue(ctx context.Context, raw json.RawMessage, project string) (ExportIssue, error) {
	var row ExportIssue
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &row) != nil || json.Unmarshal(raw, &fields) != nil || row.ID == "" || row.Project.ID != project {
		return ExportIssue{}, protocolError()
	}
	for _, name := range []string{"title", "description", "estimate", "parent"} {
		if len(fields[name]) == 0 {
			return ExportIssue{}, protocolError()
		}
	}
	if row.Title == "" || row.Parent != nil && row.Parent.ID == "" {
		return ExportIssue{}, protocolError()
	}
	complete, err := c.completeIssue(ctx, row.ID, raw)
	if err != nil {
		return ExportIssue{}, err
	}
	row.Issue = complete
	return row, nil
}
