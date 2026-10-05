package orca

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
)

// Worker is the fleet projection, not the terminal's PTY observation.
// Unverifiable means unknown, never proof of process exit.
type Worker struct {
	DispatchID          string `json:"dispatchId"`
	DispatchStatus      string `json:"dispatchStatus"`
	AgentTerminalHandle string `json:"agentTerminalHandle"`
	Resource            struct {
		WorktreeID string `json:"worktreeId"`
	} `json:"resource"`
	Projection struct {
		Liveness struct {
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
		} `json:"liveness"`
		Stage struct {
			Worker string `json:"worker"`
			Detail string `json:"detail"`
		} `json:"stage"`
	} `json:"projection"`
}

func (w Worker) WorktreePath() string {
	repo, path, ok := strings.Cut(w.Resource.WorktreeID, "::")
	if !ok || repo == "" || !filepath.IsAbs(path) {
		return ""
	}
	return path
}

type WorkerPage struct {
	Workers []Worker `json:"workers"`
	Page    struct {
		HasMore    bool   `json:"hasMore"`
		NextCursor string `json:"nextCursor"`
	} `json:"page"`
}

// ParseWorkerList keeps WorkerList's receipt contract for other consumers.
func ParseWorkerList(r Receipt) (WorkerPage, error) {
	var page WorkerPage
	if json.Unmarshal(r["workers"], &page.Workers) != nil {
		return page, invalid("invalid worker list")
	}
	if raw := r["page"]; len(raw) > 0 && json.Unmarshal(raw, &page.Page) != nil {
		return page, invalid("invalid worker list page")
	}
	return page, nil
}

func (c *Client) WorkerStop(ctx context.Context, request, dispatch string) (Receipt, error) {
	return c.mutate(ctx, request, []string{"orchestration", "worker-stop", "--dispatch", dispatch})
}
