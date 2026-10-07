package linear

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type refreshFake struct {
	mu       sync.Mutex
	issues   []Issue
	err      error
	calls    int
	gotSince time.Time
	started  chan struct{}
	release  chan struct{}
}

func (f *refreshFake) Snapshot(_ context.Context, _ []string, since time.Time) ([]Issue, error) {
	f.mu.Lock()
	f.calls++
	f.gotSince = since
	if f.started != nil {
		close(f.started)
		f.started = nil
	}
	release := f.release
	err := f.err
	issues := append([]Issue(nil), f.issues...)
	f.mu.Unlock()
	if release != nil {
		<-release
	}
	return issues, err
}

func (*refreshFake) ListComments(context.Context, string) ([]Comment, error) { return nil, nil }

func TestRefreshFailurePreservesCompleteSnapshotAndDirtyWork(t *testing.T) {
	p, err := OpenProjection(filepath.Join(t.TempDir(), "linear.db"), "team-1/project-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if err := p.Publish(context.Background(), ProjectionSnapshot{Issues: []Issue{{ID: "i1"}}, Complete: true, Watermark: "2026-10-07T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkDirty(context.Background(), "issue", "i1"); err != nil {
		t.Fatal(err)
	}
	fake := &refreshFake{err: errors.New("remote read failed")}
	coord := NewRefreshCoordinator(p, fake, RefreshOptions{Projects: []string{"project"}})
	if err := coord.Refresh(context.Background()); err == nil {
		t.Fatal("failed remote read was acknowledged")
	}
	got, err := p.Read(context.Background(), ReadOptions{RequireComplete: true})
	if err != nil || got.Meta.Watermark != "2026-10-07T00:00:00Z" || len(got.Issues) != 1 {
		t.Fatalf("snapshot after failed refresh=%+v err=%v", got, err)
	}
	dirty, err := p.Dirty(context.Background())
	if err != nil || len(dirty["issue"]) != 1 {
		t.Fatalf("dirty after failed refresh=%v err=%v", dirty, err)
	}
}

func TestRefreshCoalescesConcurrentRequests(t *testing.T) {
	p, err := OpenProjection(filepath.Join(t.TempDir(), "linear.db"), "team-1/project-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if err := p.Publish(context.Background(), ProjectionSnapshot{Complete: true, Watermark: "2026-10-07T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	fake := &refreshFake{issues: []Issue{{ID: "i1", UpdatedAt: time.Date(2026, 10, 7, 0, 1, 0, 0, time.UTC)}}, started: make(chan struct{}), release: make(chan struct{})}
	joined := make(chan struct{})
	coord := NewRefreshCoordinator(p, fake, RefreshOptions{Projects: []string{"project"}, OnCoalesced: func() { close(joined) }})
	first := make(chan error, 1)
	started := fake.started
	go func() { first <- coord.Refresh(context.Background()) }()
	<-started
	second := make(chan error, 1)
	go func() { second <- coord.Refresh(context.Background()) }()
	<-joined
	close(fake.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	calls := fake.calls
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("coalesced refresh calls=%d", calls)
	}
}
