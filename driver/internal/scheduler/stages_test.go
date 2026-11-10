package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestStageDurationsLiveAndSnapshot(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(t.TempDir(), "status.json"))
	now := setup(&fakeBackend{}).Now()
	ticket, err := InspectOwnership([]task.Comment{
		{Body: "阶段: gate\n归属: 执行器", CreatedAt: now.Add(-80 * time.Second)},
		{Body: "阶段: worker 运行中\n归属: 执行器", CreatedAt: now.Add(-200 * time.Second)},
		{Body: "阶段: worker 运行中\n归属: 执行器", CreatedAt: now.Add(-150 * time.Second)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ticket.Repo, ticket.Identifier = "a", "OF-459"
	f := &fakeBackend{tickets: []Ticket{ticket}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(status.PushPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := status.Write(status.Snapshot{GeneratedAt: now, Tickets: SnapshotRows(f.tickets, s.Repositories, s.Now())}); err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(status.Read(now, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"live": live, "snapshot": legacy} {
		t.Run(name, func(t *testing.T) {
			var view struct {
				Tickets []struct {
					Stages []struct {
						Name    string `json:"name"`
						Seconds int    `json:"seconds"`
					} `json:"stages"`
				} `json:"tickets"`
			}
			if err := json.Unmarshal(data, &view); err != nil {
				t.Fatal(err)
			}
			if len(view.Tickets) != 1 {
				t.Fatalf("tickets = %s", data)
			}
			got, _ := json.Marshal(view.Tickets[0].Stages)
			want := `[{"name":"worker 运行中","seconds":120},{"name":"gate","seconds":80}]`
			if string(got) != want {
				t.Fatalf("stages = %s, want %s", got, want)
			}
		})
	}
}

func TestStageDurationsBoundedAndWithoutEvidence(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"absent", "untimed", "future", "phase changed", "over limit"} {
		t.Run(name, func(t *testing.T) {
			var comments []task.Comment
			switch name {
			case "untimed":
				comments = []task.Comment{{Body: "阶段: gate\n归属: 执行器"}}
			case "future":
				comments = []task.Comment{{Body: "阶段: gate\n归属: 执行器", CreatedAt: now.Add(time.Second)}}
			case "phase changed":
				comments = []task.Comment{{Body: "阶段: gate\n归属: 执行器", CreatedAt: now.Add(-time.Second)}}
			case "over limit":
				for i := 0; i < 23; i++ {
					comments = append(comments, task.Comment{Body: fmt.Sprintf("阶段: stage-%d\n归属: 执行器", i), CreatedAt: now.Add(time.Duration(i-23) * 10 * time.Second)})
				}
			}
			ticket, err := InspectOwnership(comments)
			if err != nil {
				t.Fatal(err)
			}
			if name == "phase changed" {
				ticket.Phase = machine.PhaseReview
			}
			data, err := json.Marshal(ObservationRow(ticket, nil, now))
			if err != nil {
				t.Fatal(err)
			}
			var row map[string]json.RawMessage
			if err := json.Unmarshal(data, &row); err != nil {
				t.Fatal(err)
			}
			if name != "over limit" {
				if _, ok := row["stages"]; ok {
					t.Fatalf("stages must be omitted without timeline evidence: %s", data)
				}
				return
			}
			var stages []status.Stage
			if err := json.Unmarshal(row["stages"], &stages); err != nil {
				t.Fatal(err)
			}
			if len(stages) != 20 || stages[0] != (status.Stage{Name: "stage-3", Seconds: 10}) || stages[19] != (status.Stage{Name: "stage-22", Seconds: 10}) {
				t.Fatalf("want most recent 20 stages including current, got %+v", stages)
			}
		})
	}
}

// The recorded ledger interleaves stage comments with merge-progress-only
// comments and repeats stages, the real shapes the timeline must fold.
func TestRecordedStageDurations(t *testing.T) {
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded(t, "ledger-comments-deployment-retry-workflow.json"), &recording); err != nil {
		t.Fatal(err)
	}
	ticket, err := InspectOwnership(recording.Comments)
	if err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339, "2026-11-04T15:53:00+08:00")
	if err != nil {
		t.Fatal(err)
	}
	want := []status.Stage{{Name: "排队合并", Seconds: 427}, {Name: "Agent处置", Seconds: 434}, {Name: "排队合并", Seconds: 141}, {Name: "Agent处置", Seconds: 50}}
	got := ObservationRow(ticket, nil, now).Stages
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stages = %+v, want %+v", got, want)
	}
	var sample status.View
	if err := json.Unmarshal(recorded(t, "driver-status-json-stages.json"), &sample); err != nil {
		t.Fatal(err)
	}
	if len(sample.Tickets) != 1 || sample.Tickets[0].Identifier != "ZY-365" || !reflect.DeepEqual(sample.Tickets[0].Stages, got) {
		t.Fatalf("driver-status-json-stages.json differs from driver output %+v: %+v", got, sample.Tickets)
	}
}
