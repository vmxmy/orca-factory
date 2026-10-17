package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type workerIdentity struct {
	Task     string `json:"taskId"`
	Run      string `json:"runId"`
	Dispatch string `json:"dispatchId"`
	State    string `json:"workerState"`
	Status   string `json:"dispatchStatus"`
	Resource struct {
		ReleaseState string `json:"releaseState"`
	} `json:"resource"`
	Projection struct {
		Stage struct {
			Detail string `json:"detail"`
		} `json:"stage"`
	} `json:"projection"`
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

// dispatchRow finds the recorded dispatch in a complete worker read; nil means
// the complete read does not list it.
func (c *Controller) dispatchRow(ctx context.Context, p *Record) (*workerIdentity, error) {
	rows, err := c.workers(ctx, p.Run)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Dispatch == p.Dispatch {
			return &rows[i], nil
		}
	}
	return nil, nil
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
		if len(tasks) == 0 {
			empty, err := c.runEmpty(ctx, p.Run)
			if err != nil {
				return err
			}
			if empty {
				p.Phase, p.Outcome = "waiting", Undispatched
				p.Result = fmt.Sprintf("resolver run %s has no task or worker; dispatch never started", p.Run)
				return c.Store.Save(ctx, *p)
			}
		}
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

// runEmpty proves the original controller stopped before dispatch (ZCP-141):
// complete worker and task reads are empty twice over, so a dispatch landing
// between the first reads cannot pass as one that never started. A legacy
// read-only run, or an empty list without its count, proves nothing.
func (c *Controller) runEmpty(ctx context.Context, run string) (bool, error) {
	workers, err := c.workers(ctx, run)
	if err != nil || len(workers) > 0 {
		return false, err
	}
	listing, err := c.Orca.TaskListing(ctx, run)
	if err != nil || len(listing.Tasks) > 0 {
		return false, err
	}
	if listing.LegacyReadOnly || !listing.Counted {
		return false, errors.New("incomplete resolver task list; awaiting original task evidence")
	}
	workers, err = c.workers(ctx, run)
	return len(workers) == 0, err
}
