package resolver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type workerIdentity struct {
	Task     string `json:"taskId"`
	Run      string `json:"runId"`
	Dispatch string `json:"dispatchId"`
	Status   string `json:"dispatchStatus"`
	Resource struct {
		ReleaseState string `json:"releaseState"`
	} `json:"resource"`
}

// Complete worker pages prove association; missing/ambiguous rows never grant a
// replacement controller permission to repeat an uncertain dispatch.
func (c *Controller) worker(ctx context.Context, p *Record) (*workerIdentity, error) {
	rows, err := c.workers(ctx, p.Run)
	if err != nil {
		return nil, err
	}
	var found *workerIdentity
	for _, row := range rows {
		if row.Task != p.Task {
			continue
		}
		if found != nil || row.Run != p.Run || row.Dispatch == "" || (p.Dispatch != "" && row.Dispatch != p.Dispatch) {
			return nil, errors.New("conflicting resolver worker association")
		}
		copy := row
		found = &copy
	}
	if found == nil {
		return nil, errors.New("resolver worker association not yet observable")
	}
	return found, nil
}

// dispatchStatus finds the recorded dispatch in a complete worker read.
func (c *Controller) dispatchStatus(ctx context.Context, p *Record) (listed bool, status string, err error) {
	rows, err := c.workers(ctx, p.Run)
	if err != nil {
		return false, "", err
	}
	for _, row := range rows {
		if row.Dispatch == p.Dispatch {
			return true, row.Status, nil
		}
	}
	return false, "", nil
}

func (c *Controller) workers(ctx context.Context, run string) ([]workerIdentity, error) {
	cursor := ""
	seen := map[string]bool{}
	var all []workerIdentity
	for {
		receipt, err := c.Orca.WorkerList(ctx, orca.ListOptions{Run: run, Cursor: cursor, Limit: 100, IncludeRemote: true})
		if err != nil {
			return nil, err
		}
		var scope struct {
			Run string `json:"run"`
		}
		var page struct {
			HasMore *bool  `json:"hasMore"`
			Next    string `json:"nextCursor"`
		}
		var rows []workerIdentity
		if json.Unmarshal(receipt["scope"], &scope) != nil || scope.Run != run || json.Unmarshal(receipt["page"], &page) != nil || page.HasMore == nil || json.Unmarshal(receipt["workers"], &rows) != nil || rows == nil {
			return nil, errors.New("incomplete resolver worker observation")
		}
		all = append(all, rows...)
		if !*page.HasMore {
			return all, nil
		}
		if page.Next == "" || seen[page.Next] {
			return nil, errors.New("invalid resolver worker cursor")
		}
		cursor = page.Next
		seen[cursor] = true
	}
}
func (c *Controller) recoverStart(ctx context.Context, p *Record) error {
	tasks, err := c.Orca.TaskList(ctx, p.Run)
	if err != nil {
		return err
	}
	var found *orca.Task
	for _, task := range tasks {
		if task.RunID != p.Run || task.Spec != p.Spec {
			continue
		}
		if found != nil || task.ID == "" {
			return errors.New("ambiguous resolver task recovery")
		}
		copy := task
		found = &copy
	}
	if found == nil {
		return errors.New("uncertain resolver dispatch; awaiting original task evidence")
	}
	p.Task = found.ID
	worker, err := c.worker(ctx, p)
	if err != nil {
		return err
	}
	if found.DispatchID != "" && found.DispatchID != worker.Dispatch {
		return errors.New("conflicting resolver task dispatch")
	}
	p.Dispatch = worker.Dispatch
	p.Phase = "running"
	return c.Store.Save(ctx, *p)
}
