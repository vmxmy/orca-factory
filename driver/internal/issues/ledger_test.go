package issues

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/ledger"
)

func TestGitHubLedgerRotatesAt400Comments(t *testing.T) {
	c, f := newImportServer(t)
	ctx := context.Background()
	store := &LedgerStore{Client: c}
	book := ledger.New(store, ledger.Options{Ticks: make(chan time.Time)})
	t.Cleanup(func() {
		if err := book.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := book.Append(`{"ticket":"GH-1","minutes":1}`); err != nil {
		t.Fatal(err)
	}
	if err := book.Flush(ctx); err != nil {
		t.Fatalf("first ledger append: %v", err)
	}
	first, err := c.Read(ctx, "GH-1")
	if err != nil || first.State.Name != "Backlog" || !first.Metadata.Complete || len(first.Labels.Nodes) != 0 {
		t.Fatalf("ledger creation lost Backlog or became a labeled task: %+v %v", first, err)
	}
	for i := 2; i <= 400; i++ {
		f.comments[1] = append(f.comments[1], map[string]any{"id": i, "body": fmt.Sprintf("ledger {\"ticket\":\"GH-%d\"}", i)})
	}
	if err := book.Append(`{"ticket":"GH-401","minutes":2}`); err != nil {
		t.Fatal(err)
	}
	if err := book.Flush(ctx); err != nil {
		t.Fatalf("ledger rotation at 400 comments: %v", err)
	}
	rows, err := store.ListIssues(ctx)
	if err != nil || len(rows) != 2 || rows[0].State != "Done" || rows[1].Title != ledger.Title+" #2" || rows[1].State != "Backlog" || rows[0].ID != first.ID {
		t.Fatalf("ledger rotation changed identity or state: %+v %v", rows, err)
	}
	old, err := c.Read(ctx, first.ID)
	if err != nil || old.NativeState != "closed" || len(f.comments[1]) != 400 || len(f.comments[2]) != 1 || f.comments[2][0]["body"] != `ledger {"ticket":"GH-401","minutes":2}` {
		t.Fatalf("ledger rotation lost rows or native closure: %+v %v", old, err)
	}
	for _, row := range f.rows {
		body := row["body"].(string)
		if strings.Contains(body, "CHECK:") || strings.Contains(body, "SCOPE:") {
			t.Fatalf("ledger fabricated task brief: %s", body)
		}
	}
}
