package linear

import (
	"context"
	"errors"
	"time"
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
	rows, err := c.Snapshot(ctx, []string{project}, time.Time{})
	if err != nil {
		return nil, err
	}
	result := make([]ExportIssue, 0, len(rows))
	for _, row := range rows {
		fresh, err := c.FreshIssue(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		var data struct {
			Issue *ExportIssue `json:"issue"`
		}
		if err = c.do(ctx, `query ExportIssue($id: String!) { issue(id: $id) { id identifier title description estimate parent { id } project { id } } }`, map[string]any{"id": row.ID}, &data); err != nil {
			return nil, err
		}
		if data.Issue == nil || data.Issue.ID != row.ID || data.Issue.Project.ID != project {
			return nil, protocolError()
		}
		copy := *data.Issue
		copy.Issue = fresh
		result = append(result, copy)
	}
	return result, nil
}
