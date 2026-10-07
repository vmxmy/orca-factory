package linear

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectionClientSnapshotUsesCompleteProjection(t *testing.T) {
	p, err := OpenProjection(filepath.Join(t.TempDir(), "linear.db"), "team/project")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	issue := Issue{ID: "i1", Identifier: "ENG-1", Project: NamedID{ID: "project"}, UpdatedAt: time.Now().UTC()}
	if err := p.Publish(context.Background(), ProjectionSnapshot{Issues: []Issue{issue}, Complete: true, Watermark: issue.UpdatedAt.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	client := NewProjectionClient(&Client{}, p, time.Hour)
	got, err := client.Snapshot(context.Background(), []string{"project"}, time.Time{})
	if err != nil || len(got) != 1 || got[0].Identifier != "ENG-1" {
		t.Fatalf("snapshot=%+v err=%v", got, err)
	}
}
