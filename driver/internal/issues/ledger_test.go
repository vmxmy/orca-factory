package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/ledger"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestLedgerStatsReadsTicketComments(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/ledger-ticket-comments.json")
	if err != nil {
		t.Fatal(err)
	}
	var samples []struct {
		Ticket   string
		Comments []task.Comment
	}
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("recorded tickets=%d", len(samples))
	}
	for _, tc := range []struct {
		name                   string
		ledgerTicket, old, all bool
		want                   string
	}{
		{name: "ticket comments", want: "runs=2 merged=2 escalated=0 no_ticket=0\n"},
		{name: "ledger preferred", ledgerTicket: true, want: "runs=1 merged=0 escalated=1 no_ticket=0\n"},
		{name: "old comments excluded", old: true, want: "runs=0\n"},
		{name: "all includes old comments", old: true, all: true, want: "runs=2 merged=2 escalated=0 no_ticket=0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFeishuFake(t)
			for i, sample := range samples {
				for j, comment := range sample.Comments {
					at := comment.CreatedAt.Format(time.RFC3339Nano)
					f.rows["tblE"] = append(f.rows["tblE"], feishuRow{
						"_id": fmt.Sprintf("recE%d%d", i, j), "票据": []feishu.LinkRef{{ID: []string{"recA", "recB"}[i]}},
						"创建时间": at, "修改时间": at, "正文": comment.Body,
					})
				}
			}
			// Repeat a recorded run on another ticket to exercise the global dedup key.
			f.rows["tblE"] = append(f.rows["tblE"], feishuRow{"_id": "recDuplicate", "票据": []feishu.LinkRef{{ID: "recB"}}, "创建时间": "2026-11-09T04:40:00Z", "修改时间": "2026-11-09T04:40:00Z", "正文": samples[0].Comments[0].Body})
			if tc.ledgerTicket {
				f.rows["tblT"][1]["标题"] = ledger.Title
				f.rows["tblE"] = f.rows["tblE"][:1]
				f.rows["tblE"] = append(f.rows["tblE"], feishuRow{"_id": "recLedger", "票据": []feishu.LinkRef{{ID: "recB"}}, "创建时间": "2026-11-09T04:40:00Z", "修改时间": "2026-11-09T04:40:00Z", "正文": `ledger {"ticket":"ledger-only","started_at":"2026-11-09T00:00:00Z","outcome":"escalated"}`})
			}
			now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
			if tc.old {
				now = now.Add(30 * 24 * time.Hour)
			}
			book := ledger.New(&LedgerStore{Client: newFeishuTest(t, f)}, ledger.Options{Now: func() time.Time { return now }, Ticks: make(chan time.Time)})
			t.Cleanup(func() {
				if err := book.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			var out, warnings bytes.Buffer
			if err := book.Stats(context.Background(), tc.all, &out, &warnings); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("stats=%q, want %q", out.String(), tc.want)
			}
			// The ledger-ticket output stays byte-for-byte unchanged; only the fallback names its source.
			if tc.ledgerTicket == strings.HasSuffix(out.String(), "source=ticket-comments\n") || tc.ledgerTicket && strings.Contains(out.String(), "source=") {
				t.Fatalf("stats=%q, ledger ticket=%v", out.String(), tc.ledgerTicket)
			}
			if len(f.writes) != 0 {
				t.Fatalf("stats wrote records: %+v", f.writes)
			}
		})
	}
}

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
