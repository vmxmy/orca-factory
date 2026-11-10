package issues

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestRecordedFeishuIdleInspectionReadsLess(t *testing.T) {
	f := newRecordedFeishuBase(t)
	a := loadFeishuRecording(t, "bot-events-list.json")
	b := loadFeishuRecording(t, "bot-events-list-ticket-b.json")
	events := recBasePath + "/tables/" + recEventTable + "/records"
	filter := `{"logic":"and","conditions":[["票据","intersects",[{"id":"` + recTicketA + `"},{"id":"` + recTicketB + `"}]]]}`
	f.on(http.MethodGet, events, filter, nil, feishuReply{data: joinedPages(t, a.Response.Data, uneditedPage(t, b.Response.Data))})
	c := f.backend(t)
	requests := func() int {
		return len(f.sent(http.MethodGet, recBasePath+"/tables/"+recProjectTable+"/records")) +
			len(f.sent(http.MethodGet, recBasePath+"/tables/"+recTicketTable+"/records")) +
			len(f.sent(http.MethodPost, recBasePath+"/tables/"+recTicketTable+"/records/batch_get")) +
			len(f.sent(http.MethodGet, events))
	}
	read := func(since time.Time) ([]task.Issue, map[string][]task.Comment, int) {
		t.Helper()
		before := requests()
		rows, err := c.Snapshot(context.Background(), []string{feishuRepo}, since)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		comments, err := c.SnapshotComments(context.Background(), ids)
		if err != nil {
			t.Fatal(err)
		}
		return rows, comments, requests() - before
	}
	started := time.Now()
	full, fullComments, first := read(time.Time{})
	if len(full) != 2 || len(fullComments[feishuUUIDA]) != 3 {
		t.Fatalf("recording did not exercise both tickets and their events: tickets=%d histories=%v", len(full), fullComments)
	}
	incremental, comments, second := read(started)
	if !reflect.DeepEqual(incremental, full) || !reflect.DeepEqual(comments, fullComments) {
		t.Fatal("unchanged incremental inspection differs from full ticket and event sets")
	}
	if second >= first {
		t.Fatalf("idle inspection reread unchanged records: first=%d second=%d; want fewer requests on second round", first, second)
	}
}
