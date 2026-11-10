package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type todayIssues struct {
	fakeIssues
	reads int
}

func (f *todayIssues) SnapshotComments(_ context.Context, ids []string) (map[string][]task.Comment, error) {
	f.reads++
	return map[string][]task.Comment{ids[0]: f.issueComments}, nil
}

func TestTodayOutputFromExistingComments(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.FixedZone("local", 8*3600))
	row := func(id, end, outcome string, minutes any) string {
		return fmt.Sprintf(`ledger {"ticket":%q,"started_at":"2026-11-10T15:00:00Z","finished_at":%q,"outcome":%q,"minutes":%v}`, id, end, outcome, minutes)
	}
	for _, tc := range []struct {
		name string
		rows []string
		want map[string]any
	}{
		{"empty", nil, nil},
		{"local midnight and future", []string{
			row("old", "2026-11-10T15:59:59Z", "merged", 100),
			row("midnight", "2026-11-10T16:00:00Z", "merged", 10),
			row("crossing", "2026-11-10T16:01:00Z", "escalated", 20),
			row("future", "2026-11-11T16:00:00Z", "merged", 200),
		}, map[string]any{"merged": float64(1), "escalated": float64(1), "medianMinutes": float64(15)}},
		{"odd median and duplicate", []string{
			row("a", "2026-11-11T01:00:00Z", "merged", 90),
			row("b", "2026-11-11T02:00:00Z", "merged", 3),
			row("c", "2026-11-11T03:00:00Z", "escalated", 12),
			row("c", "2026-11-11T03:00:00Z", "escalated", 12),
		}, map[string]any{"merged": float64(2), "escalated": float64(1), "medianMinutes": float64(12)}},
		{"missing duration", []string{row("a", "2026-11-11T01:00:00Z", "merged", "null")}, map[string]any{"merged": float64(1), "escalated": float64(0)}},
		{"no completed work", []string{
			row("old", "2026-11-10T15:59:59Z", "merged", 100),
			row("empty", "2026-11-11T01:00:00Z", "no_ticket", 0),
			row("broken", "bad", "merged", 12),
			"ledger not-json",
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DRIVER_STATUS_FILE", "")
			f := &todayIssues{fakeIssues: fakeIssues{issues: []task.Issue{{ID: "closed", Project: task.NamedID{ID: "p1"}, State: task.NamedID{Name: "Done"}}}, issueComments: evidenceComments(tc.rows...)}}
			r := &Runtime{Clients: map[string]issues.Store{"a": f}, Orca: &fakeOrca{automations: receipt(`{"automations":[]}`)}}
			s := setup(&fakeBackend{})
			s.Backend, s.Now = r, func() time.Time { return now }
			s.Repositories = []config.Repository{{Path: "a", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "p1", "AGENT_SCOPE_AVOID": "0"}}}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(status.Read(now, time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			var view struct {
				Today map[string]any `json:"today"`
			}
			if err := json.Unmarshal(data, &view); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(view.Today, tc.want) {
				t.Fatalf("today=%v, want %v; status=%s", view.Today, tc.want, data)
			}
			if f.reads != 1 {
				t.Fatalf("comment reads=%d, want one existing batch", f.reads)
			}
		})
	}
}

func TestRecordedTodayLedgerComments(t *testing.T) {
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
	rows := []task.Issue{}
	comments := map[string][]task.Comment{}
	repos := map[string]string{}
	for _, sample := range samples {
		rows = append(rows, task.Issue{ID: sample.Ticket})
		comments[sample.Ticket] = sample.Comments
		repos[sample.Ticket] = "repo"
	}
	now := time.Date(2026, 11, 9, 13, 0, 0, 0, time.FixedZone("local", 8*3600))
	got := todayOutput(readTodayRuns(rows, comments, repos), now)
	if got == nil || got.Merged != 1 || got.Escalated != 0 || got.MedianMinutes == nil || *got.MedianMinutes != 37.10948458495 {
		t.Fatalf("recorded today=%+v", got)
	}
	rows = append(rows, task.Issue{ID: "book", Title: "[ledger] agent-graph"})
	if got := todayOutput(readTodayRuns(rows, comments, repos), now); got != nil {
		t.Fatalf("ledger ticket must take precedence over task comments: %+v", got)
	}
}
