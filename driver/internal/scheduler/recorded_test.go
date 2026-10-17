package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// Contract tests read payloads recorded from the real systems (testdata/recorded).
// Hand-written fakes encode our assumptions; recorded payloads encode the systems.
func recorded(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/recorded/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecordedOwnershipUnregistered(t *testing.T) {
	var resp struct {
		Data struct {
			Issue struct {
				Comments struct{ Nodes []task.Comment } `json:"comments"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorded(t, "linear-comment-ownership-unregistered.json"), &resp); err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{}
	if err := readOwnership(&ticket, resp.Data.Issue.Comments.Nodes); err != nil {
		t.Fatal(err)
	}
	if !unregistered(ticket) {
		t.Fatalf("scheduler claim with term=- must read as unregistered, got terminal %q", ticket.Terminal)
	}
}

func TestRecordedTerminalList(t *testing.T) {
	raw := recorded(t, "orca-terminal-list.json")
	r := &Runtime{OrcaPath: "/usr/bin/orca", Run: func(context.Context, runx.Spec) ([]byte, error) { return raw, nil }}
	if absent, err := r.TerminalAbsent(context.Background(), Ticket{Terminal: "term_640d9460-b9fd-46a0-bedb-09763c8c20ea"}); err != nil || absent {
		t.Fatalf("listed terminal: absent=%v err=%v", absent, err)
	}
	if absent, err := r.TerminalAbsent(context.Background(), Ticket{Terminal: "term_gone"}); err != nil || !absent {
		t.Fatalf("missing terminal: absent=%v err=%v", absent, err)
	}
}

func TestRecordedExecutorHandoff(t *testing.T) {
	var resp struct {
		Data struct {
			Issue struct {
				Comments struct{ Nodes []task.Comment } `json:"comments"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorded(t, "linear-comments-executor-handoff.json"), &resp); err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{}
	if err := readOwnership(&ticket, resp.Data.Issue.Comments.Nodes); err != nil {
		t.Fatal(err)
	}
	if ticket.Owner != machine.OwnerHuman {
		t.Fatalf("a 转人 comment hands the ticket to a human, got owner %q", ticket.Owner)
	}
}

func TestRecordedSchedulerBlockerIdentifiers(t *testing.T) {
	var response struct{ Data struct{ Issue task.Issue } }
	if err := json.Unmarshal(recorded(t, "linear-inverse-relations.json"), &response); err != nil {
		t.Fatal(err)
	}
	issue := response.Data.Issue
	issue.Project.ID = "project"
	issue.State.Name = "Todo"
	issue.Labels.Nodes = []task.NamedID{{Name: "ready-for-agent"}}
	client := &fakeIssues{issues: []task.Issue{issue}}
	repo := config.Repository{Path: "repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "project", "AGENT_SCOPE_AVOID": "0"}}
	runtime := &Runtime{Clients: map[string]issues.Store{"repo": client}}
	tickets, err := runtime.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 || !tickets[0].Blocked || tickets[0].BlockersIncomplete || !reflect.DeepEqual(tickets[0].Blockers, []string{"ZII-893"}) {
		t.Fatalf("lost recorded blocker identifiers: %+v", tickets)
	}
}

// The fixture is the real worker-list of Run run_9714ea9b8576: dispatch IDs
// and statuses must parse as recorded, and no settled row may be abandoned.
func TestRecordedDispatchesWorkerListLiveness(t *testing.T) {
	var envelope struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(recorded(t, "orca-worker-list-liveness.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{Orca: &reapOrca{fakeOrca: fakeOrca{pages: []orca.Receipt{envelope.Result, envelope.Result}}}}
	rows, err := r.dispatches(context.Background(), "run_9714ea9b8576")
	if err != nil {
		t.Fatal(err)
	}
	want := []dispatchRow{{"ctx_f9e3c9ac102a", "failed", ""}, {"ctx_6d1316722a7c", "failed", ""}, {"ctx_a93f6b74d60d", "failed", ""}, {"ctx_2ec61f77c499", "completed", ""}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows=%+v", rows)
	}
	for _, row := range rows {
		if !settledDispatch(row.Status) {
			t.Fatalf("recorded row %+v counted as unsettled", row)
		}
	}
	r.StateRoot = t.TempDir()
	r.PIDAlive = func(int) bool { return false }
	r.Repositories = map[string]config.Repository{"/repo": {Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "o/proj"}}}
	ticket := Ticket{Identifier: "OF-1", Repo: "/repo", Owner: machine.OwnerExecutor, Terminal: "-", Run: "run_9714ea9b8576"}
	if err := orca.WriteBeat(orca.HeartbeatPath(r.StateRoot, "proj", "OF-1", "executor"), orca.Beat{PID: 7, Run: "run_9714ea9b8576", At: time.Unix(0, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReapDead(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if abandoned := r.Orca.(*reapOrca).abandoned; len(abandoned) != 0 {
		t.Fatalf("settled dispatches abandoned: %v", abandoned)
	}
}

// An abandoned dispatch reads as dispatchStatus failed; only workerState names
// the abandon a failed terminal close must retry (orca-worker-list-abandoned.json).
func TestRecordedDispatchesWorkerListAbandoned(t *testing.T) {
	var envelope struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(recorded(t, "orca-worker-list-abandoned.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{Orca: &reapOrca{fakeOrca: fakeOrca{pages: []orca.Receipt{envelope.Result}}}}
	rows, err := r.dispatches(context.Background(), "run_4eb9fdf9ecdc")
	if err != nil {
		t.Fatal(err)
	}
	if want := []dispatchRow{{"ctx_ee6da2ada658", "failed", "abandoned"}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows=%+v", rows)
	}
}
