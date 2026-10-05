package executor

import (
	"context"
	"errors"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// One watch per dispatch, retained across read retries; restart begins a fresh
// observation period. An uncertain stop is never issued a second time.
type workerWatch struct {
	unobservedSince time.Time
	stopAttempted   bool
	stopErr         error
	observe         func(orca.Worker) error
}

func (e *Executor) currentWorker(ctx context.Context, run, dispatch string) (*orca.Worker, error) {
	cursor := ""
	seen := map[string]bool{}
	for {
		receipt, err := e.Orca.WorkerList(ctx, orca.ListOptions{Run: run, Cursor: cursor, Limit: 100, IncludeRemote: true})
		if err != nil {
			return nil, err
		}
		page, err := orca.ParseWorkerList(receipt)
		if err != nil {
			return nil, err
		}
		for _, w := range page.Workers {
			if w.DispatchID == dispatch {
				return &w, nil
			}
		}
		if !page.Page.HasMore {
			return nil, nil
		}
		cursor = page.Page.NextCursor
		if cursor == "" || seen[cursor] {
			return nil, errors.New("invalid worker pagination")
		}
		seen[cursor] = true
	}
}

func (e *Executor) wait(ctx context.Context, run, task, dispatch string, interval time.Duration, watch *workerWatch, observeIdle func() error) (*orca.WorkerReport, error) {
	for {
		tasks, err := e.Orca.TaskList(ctx, run)
		if err != nil {
			watch.unobservedSince = time.Time{}
			return nil, errors.New("task polling failed")
		}
		completed := false
		var report *orca.WorkerReport
		for _, t := range tasks {
			// task-list rows carry no dispatch_id; match it only when supplied.
			if t.ID != task || (t.DispatchID != "" && t.DispatchID != dispatch) || (t.RunID != "" && t.RunID != run) {
				continue
			}
			completed = completed || t.Status == "completed"
			report, err = t.WorkerReport()
			if err != nil {
				return nil, errors.New("invalid worker report")
			}
			if report != nil {
				if report.Outcome != "succeeded" && report.Outcome != "failed" {
					return nil, errors.New("invalid worker outcome")
				}
				break
			}
		}
		worker, readErr := e.currentWorker(ctx, run, dispatch)
		if readErr == nil && worker != nil && watch.observe != nil {
			if err := watch.observe(*worker); err != nil {
				return nil, err
			}
		}
		// A report is authoritative even if the fleet read fails or says exited.
		if report != nil {
			return report, nil
		}
		if readErr != nil {
			watch.unobservedSince = time.Time{}
			return nil, readErr
		}
		unverifiable := worker != nil && worker.Projection.Liveness.Verdict == "unverifiable"
		if worker != nil && worker.Projection.Liveness.Verdict == "exited" {
			return livenessFailure("worker exited without worker_done"), nil
		}
		if worker != nil && !unverifiable && worker.Projection.Stage.Detail == "turn_start_unobserved" {
			now := time.Now()
			if e.Now != nil {
				now = e.Now()
			}
			if watch.unobservedSince.IsZero() {
				watch.unobservedSince = now
			}
			if now.Sub(watch.unobservedSince) >= 5*time.Minute {
				if !watch.stopAttempted {
					watch.stopAttempted = true
					_, watch.stopErr = e.Orca.WorkerStop(ctx, orca.RequestID(run, dispatch, "turn-start-stop", 1), dispatch)
				}
				if watch.stopErr != nil {
					return nil, watch.stopErr
				}
				return livenessFailure("worker never started its turn"), nil
			}
		} else {
			watch.unobservedSince = time.Time{}
		}
		// Contact loss must not trigger idle failure, nudging or recovery either.
		if !completed && !unverifiable {
			if err := observeIdle(); err != nil {
				return nil, err
			}
		}
		sleep := e.Sleep
		if sleep == nil {
			sleep = pause
		}
		if err := sleep(ctx, interval); err != nil {
			return nil, err
		}
	}
}

func livenessFailure(reason string) *orca.WorkerReport {
	return &orca.WorkerReport{Provenance: "executor_liveness", Outcome: "failed", Body: reason}
}
