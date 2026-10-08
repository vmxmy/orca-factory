package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ListCommentsBatch reads complete live histories, with at most five issues per
// request. Results belong only to this call; any failed page discards all rows.
func (c *Client) ListCommentsBatch(ctx context.Context, ids []string) (map[string][]Comment, error) {
	ctx = context.WithValue(ctx, discoveryReadKey{}, true)
	result := map[string][]Comment{}
	unique := []string{}
	for _, id := range ids {
		if id == "" {
			return nil, protocolError()
		}
		if _, ok := result[id]; !ok {
			result[id] = []Comment{}
			unique = append(unique, id)
		}
	}
	for start := 0; start < len(unique); start += 5 {
		group := unique[start:min(start+5, len(unique))]
		after := make([]string, len(group))
		done := make([]bool, len(group))
		seen := make([]map[string]bool, len(group))
		for i := range group {
			seen[i] = map[string]bool{}
		}
		for {
			declarations, fields := []string{}, []string{}
			variables := map[string]any{}
			for i, id := range group {
				if done[i] {
					continue
				}
				declarations = append(declarations, fmt.Sprintf("$id%d: String!, $after%d: String", i, i))
				fields = append(fields, fmt.Sprintf("i%d: issue(id: $id%d) { id comments(first: 100, after: $after%d) { nodes { id body createdAt updatedAt } pageInfo { hasNextPage endCursor } } }", i, i, i))
				variables[fmt.Sprintf("id%d", i)] = id
				variables[fmt.Sprintf("after%d", i)] = cursor(after[i])
			}
			if len(fields) == 0 {
				break
			}
			var data map[string]*struct {
				ID       string       `json:"id"`
				Comments *commentPage `json:"comments"`
			}
			if err := c.do(ctx, "query ListCommentsBatch("+strings.Join(declarations, ", ")+") { "+strings.Join(fields, " ")+" }", variables, &data); err != nil {
				return nil, err
			}
			for i, id := range group {
				if done[i] {
					continue
				}
				page := data[fmt.Sprintf("i%d", i)]
				if page == nil || page.ID != id || page.Comments == nil {
					return nil, protocolError()
				}
				result[id] = append(result[id], page.Comments.Nodes...)
				if !page.Comments.PageInfo.HasNextPage {
					done[i] = true
					continue
				}
				next, err := nextCursor(page.Comments.PageInfo, seen[i])
				if err != nil {
					return nil, err
				}
				after[i] = next
			}
		}
	}
	return result, nil
}

// completeConnection distinguishes explicit empty connections from omitted or
// null fields. Missing pagination evidence must never look like a final page.
type completeConnection[T any] struct{ Connection[T] }

func (c *completeConnection[T]) UnmarshalJSON(raw []byte) error {
	var shape struct {
		Nodes    json.RawMessage `json:"nodes"`
		PageInfo *struct {
			HasNextPage *bool  `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		return protocolError()
	}
	if len(shape.Nodes) == 0 || string(shape.Nodes) == "null" || shape.PageInfo == nil || shape.PageInfo.HasNextPage == nil {
		return protocolError()
	}
	if err := json.Unmarshal(shape.Nodes, &c.Nodes); err != nil {
		return protocolError()
	}
	c.PageInfo = PageInfo{HasNextPage: *shape.PageInfo.HasNextPage, EndCursor: shape.PageInfo.EndCursor}
	if c.PageInfo.HasNextPage && c.PageInfo.EndCursor == "" {
		return protocolError()
	}
	return nil
}

type commentPage struct{ Connection[Comment] }

func (p *commentPage) UnmarshalJSON(raw []byte) error {
	var page completeConnection[json.RawMessage]
	if err := json.Unmarshal(raw, &page); err != nil {
		return err
	}
	p.Nodes = []Comment{}
	p.PageInfo = page.PageInfo
	for _, node := range page.Nodes {
		var fields struct {
			ID        string     `json:"id"`
			User      *NamedID   `json:"user"`
			Body      *string    `json:"body"`
			CreatedAt *time.Time `json:"createdAt"`
			UpdatedAt *time.Time `json:"updatedAt"`
		}
		if json.Unmarshal(node, &fields) != nil || fields.ID == "" || fields.Body == nil || fields.CreatedAt == nil || fields.UpdatedAt == nil {
			return protocolError()
		}
		p.Nodes = append(p.Nodes, Comment{ID: fields.ID, User: fields.User, Body: *fields.Body, CreatedAt: *fields.CreatedAt, UpdatedAt: *fields.UpdatedAt})
	}
	return nil
}
