package linear

import (
	"context"
	"encoding/json"
)

// FreshIssue reads one issue and all of its scheduling connections directly
// from Linear. No discovery result can satisfy this read.
func (c *Client) FreshIssue(ctx context.Context, id string) (Issue, error) {
	var data struct {
		Issue json.RawMessage `json:"issue"`
	}
	if err := c.do(ctx, `query FreshIssue($id: String!) { issue(id: $id) { `+issueFields+` } }`, map[string]any{"id": id}, &data); err != nil {
		return Issue{}, err
	}
	return c.completeIssue(ctx, id, data.Issue)
}

func (c *Client) completeIssue(ctx context.Context, id string, raw json.RawMessage) (Issue, error) {
	var issue Issue
	if json.Unmarshal(raw, &issue) != nil || issue.ID != id || issue.Identifier == "" || issue.State.ID == "" || issue.State.Name == "" || issue.Project.ID == "" {
		return Issue{}, protocolError()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return Issue{}, protocolError()
	}
	for _, field := range []string{"labels", "relations", "inverseRelations"} {
		var page completeConnection[json.RawMessage]
		if json.Unmarshal(fields[field], &page) != nil {
			return Issue{}, protocolError()
		}
		seen := map[string]bool{}
		for page.PageInfo.HasNextPage {
			after, err := nextCursor(page.PageInfo, seen)
			if err != nil {
				return Issue{}, err
			}
			selection := "id name"
			switch field {
			case "relations":
				selection = "id type relatedIssue { id identifier }"
			case "inverseRelations":
				selection = "id type issue { id identifier state { type } }"
			}
			var next struct {
				Issue map[string]json.RawMessage `json:"issue"`
			}
			err = c.do(ctx, `query FreshIssueConnection($id: String!, $after: String) { issue(id: $id) { id `+field+`(first: 100, after: $after) { nodes { `+selection+` } pageInfo { hasNextPage endCursor } } } }`, map[string]any{"id": id, "after": after}, &next)
			if err != nil {
				return Issue{}, err
			}
			var returnedID string
			if json.Unmarshal(next.Issue["id"], &returnedID) != nil || returnedID != id {
				return Issue{}, protocolError()
			}
			var tail completeConnection[json.RawMessage]
			if json.Unmarshal(next.Issue[field], &tail) != nil {
				return Issue{}, protocolError()
			}
			page.Nodes = append(page.Nodes, tail.Nodes...)
			page.PageInfo = tail.PageInfo
		}
		raw, err := json.Marshal(page.Connection)
		if err != nil {
			return Issue{}, protocolError()
		}
		switch field {
		case "labels":
			err = json.Unmarshal(raw, &issue.Labels)
		case "relations":
			err = json.Unmarshal(raw, &issue.Relations)
		case "inverseRelations":
			err = json.Unmarshal(raw, &issue.InverseRelations)
		}
		if err != nil {
			return Issue{}, protocolError()
		}
	}
	for _, label := range issue.Labels.Nodes {
		if label.ID == "" || label.Name == "" {
			return Issue{}, protocolError()
		}
	}
	for _, relation := range issue.Relations.Nodes {
		if relation.ID == "" || relation.Type == "" || relation.RelatedIssue.ID == "" || relation.RelatedIssue.Identifier == "" {
			return Issue{}, protocolError()
		}
	}
	for _, relation := range issue.InverseRelations.Nodes {
		if relation.ID == "" || relation.Type == "" || relation.Issue.ID == "" || relation.Issue.Identifier == "" || relation.Issue.State.Type == "" {
			return Issue{}, protocolError()
		}
	}
	return issue, nil
}
