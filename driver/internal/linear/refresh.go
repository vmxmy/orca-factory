package linear

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// RefreshReader is the existing authenticated Linear read seam. Keeping the
// coordinator on this interface makes scheduling independent of HTTP details.
type RefreshReader interface {
	Snapshot(context.Context, []string, time.Time) ([]Issue, error)
	ListComments(context.Context, string) ([]Comment, error)
}

var ErrRefreshBound = errors.New("linear reconciliation exceeds configured bound")

type RefreshMetrics struct {
	Coalesced int64 `json:"coalesced"`
}

type RefreshOptions struct {
	Projects           []string
	OverlapWindow      time.Duration
	ReconcileInterval  time.Duration
	MaxReconcileIssues int
	Now                func() time.Time
	OnCoalesced        func()
}

// RefreshCoordinator serializes refresh ownership. Concurrent requests join
// the in-flight operation, so a webhook burst cannot start overlapping reads.
type RefreshCoordinator struct {
	projection *Projection
	reader     RefreshReader
	options    RefreshOptions
	now        func() time.Time

	mu        sync.Mutex
	active    *refreshCall
	coalesced atomic.Int64
}

type refreshCall struct {
	done chan struct{}
	err  error
}

func NewRefreshCoordinator(p *Projection, reader RefreshReader, options RefreshOptions) *RefreshCoordinator {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	if options.OverlapWindow <= 0 {
		options.OverlapWindow = 2 * time.Minute
	}
	return &RefreshCoordinator{projection: p, reader: reader, options: options, now: now}
}

// Refresh runs one coalesced refresh and returns the shared result to callers
// that arrive while another refresh owns the projection.
func (c *RefreshCoordinator) Refresh(ctx context.Context) error {
	c.mu.Lock()
	if call := c.active; call != nil {
		onCoalesced := c.options.OnCoalesced
		c.mu.Unlock()
		c.coalesced.Add(1)
		if onCoalesced != nil {
			onCoalesced()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-call.done:
			return call.err
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	c.active = call
	c.mu.Unlock()

	err := c.refresh(ctx)
	c.mu.Lock()
	call.err = err
	close(call.done)
	c.active = nil
	c.mu.Unlock()
	return err
}

// Metrics reports refresh coordination without exposing ticket data.
func (c *RefreshCoordinator) Metrics() RefreshMetrics {
	return RefreshMetrics{Coalesced: c.coalesced.Load()}
}

// Schedule is the event-driven entry point. The returned channel is closed
// after the owned refresh completes; callers may ignore it when webhook intake
// only needs to trigger work.
func (c *RefreshCoordinator) Schedule(ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.Refresh(ctx); close(done) }()
	return done
}

func (c *RefreshCoordinator) refresh(ctx context.Context) error {
	dirtyAtStart, err := c.projection.Dirty(ctx)
	if err != nil {
		return err
	}
	state, err := c.projection.Meta(ctx)
	if err != nil {
		return err
	}
	full := !state.Complete || state.Watermark == ""
	if !full && c.options.ReconcileInterval > 0 && !state.RefreshedAt.IsZero() && c.now().Sub(state.RefreshedAt) >= c.options.ReconcileInterval {
		full = true
	}
	since := time.Time{}
	if !full {
		since, err = time.Parse(time.RFC3339Nano, state.Watermark)
		if err != nil {
			return err
		}
		since = since.Add(-c.options.OverlapWindow)
	}
	issues, err := c.reader.Snapshot(ctx, c.options.Projects, since)
	if err != nil {
		return err
	}
	// A webhook can mark an issue or comment dirty without changing the issue
	// updatedAt value used by the incremental filter. Reconcile a bounded full
	// page set in that case so the dirty object is actually consumed.
	if !full && len(dirtyAtStart) > 0 {
		issues, err = c.reader.Snapshot(ctx, c.options.Projects, time.Time{})
		if err != nil {
			return err
		}
		full = true
	}
	if full && c.options.MaxReconcileIssues > 0 && len(issues) > c.options.MaxReconcileIssues {
		return ErrRefreshBound
	}
	comments := make(map[string][]Comment, len(issues))
	for _, issue := range issues {
		rows, err := c.reader.ListComments(ctx, issue.ID)
		if err != nil {
			return err
		}
		comments[issue.ID] = rows
	}
	watermark := state.Watermark
	for _, issue := range issues {
		if issue.UpdatedAt.After(parseWatermark(watermark)) {
			watermark = issue.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
	}
	snapshot := ProjectionSnapshot{Issues: issues, Comments: comments, Complete: true, Watermark: watermark}
	if full {
		err = c.projection.Publish(ctx, snapshot)
	} else {
		err = c.projection.ApplyIncremental(ctx, snapshot)
	}
	if err != nil {
		return err
	}
	return c.projection.AcknowledgeDirty(ctx, dirtyAtStart)
}

func parseWatermark(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, value)
	return t
}
