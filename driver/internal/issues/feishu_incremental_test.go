package issues

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// The test Base keeps one project, the two recorded tickets and their events,
// so each table's full listing stands in for its limit=1 rev probe. The
// production probe in events-list-probe.json carries a later 事件 rev.
func TestRecordedFeishuIdleInspectionReadsLess(t *testing.T) {
	f := newRecordedFeishuBase(t)
	a := loadFeishuRecording(t, "bot-events-list.json")
	b := loadFeishuRecording(t, "bot-events-list-ticket-b.json")
	tickets := loadFeishuRecording(t, "bot-tickets-list-filter.json")
	probe := loadFeishuRecording(t, "events-list-probe.json")
	ticketsPath := recBasePath + "/tables/" + recTicketTable + "/records"
	events := recBasePath + "/tables/" + recEventTable + "/records"
	allEvents := joinedPages(t, a.Response.Data, uneditedPage(t, b.Response.Data))
	filter := `{"logic":"and","conditions":[["票据","intersects",[{"id":"` + recTicketA + `"},{"id":"` + recTicketB + `"}]]]}`
	f.on(http.MethodGet, events, filter, nil, feishuReply{data: allEvents})
	f.on(http.MethodGet, ticketsPath, "", nil, feishuReply{data: tickets.Response.Data})
	f.on(http.MethodGet, events, "", nil, feishuReply{data: allEvents}, feishuReply{data: probe.Response.Data})
	c := f.backend(t)
	requests := func() int {
		return len(f.sent(http.MethodGet, recBasePath+"/tables/"+recProjectTable+"/records")) +
			len(f.sent(http.MethodGet, ticketsPath)) +
			len(f.sent(http.MethodPost, ticketsPath+"/batch_get")) +
			len(f.sent(http.MethodGet, events))
	}
	read := func(ctx context.Context) ([]task.Issue, map[string][]task.Comment, int) {
		t.Helper()
		before := requests()
		rows, err := c.Snapshot(ctx, []string{feishuRepo}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		comments, err := c.SnapshotComments(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		return rows, comments, requests() - before
	}
	full, fullComments, first := read(context.Background())
	if len(full) != 2 || len(fullComments[feishuUUIDA]) != 3 {
		t.Fatalf("recording did not exercise both tickets and their events: tickets=%d histories=%v", len(full), fullComments)
	}
	same := func(round string, rows []task.Issue, comments map[string][]task.Comment) {
		t.Helper()
		if !reflect.DeepEqual(rows, full) || !reflect.DeepEqual(comments, fullComments) {
			t.Fatalf("%s inspection differs from full ticket and event sets", round)
		}
	}
	rows, comments, idle := read(context.Background())
	same("unchanged", rows, comments)
	if idle >= first {
		t.Fatalf("idle inspection reread unchanged records: first=%d second=%d; want fewer requests on second round", first, idle)
	}
	rows, comments, forced := read(WithFullSync(context.Background()))
	same("full sync", rows, comments)
	if forced != first {
		t.Fatalf("full sync sent %d requests, want the first round's %d", forced, first)
	}
	before := len(f.sent(http.MethodGet, events))
	rows, comments, _ = read(context.Background())
	same("events changed", rows, comments)
	if got := len(f.sent(http.MethodGet, events)) - before; got != 2 {
		t.Fatalf("changed 事件 rev sent %d event reads, want the probe and a full reread", got)
	}
}
