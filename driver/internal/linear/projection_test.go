package linear

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectionFailedPublishKeepsCompleteSnapshot(t *testing.T) {
	p, err := OpenProjection(filepath.Join(t.TempDir(), "linear.db"), "team-1/project-1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	want := Issue{ID: "issue-1", Identifier: "ENG-1", UpdatedAt: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
	if err := p.Publish(context.Background(), ProjectionSnapshot{Issues: []Issue{want}, Complete: true, Watermark: "2026-10-07T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), ProjectionSnapshot{Issues: nil, Complete: false, Watermark: "bad"}); err == nil {
		t.Fatal("incomplete snapshot was published")
	}
	got, err := p.Read(context.Background(), ReadOptions{RequireComplete: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Issues) != 1 || got.Issues[0].Identifier != want.Identifier || !got.Meta.Complete || got.Meta.Watermark != "2026-10-07T00:00:00Z" || got.Meta.Source != ProjectionSource {
		t.Fatalf("snapshot=%+v", got)
	}
}
