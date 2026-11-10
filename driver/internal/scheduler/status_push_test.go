package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func TestLiveStatusCarriesDeploymentAndQueuePosition(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(t.TempDir(), "status.json"))
	enqueued := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	f := &fakeBackend{tickets: []Ticket{
		{ID: "second", Identifier: "OF-2", Repo: "a", Owner: machine.OwnerMerge, Phase: machine.PhaseMergeQueue, Handoff: &merge.Handoff{QueueID: "second", Lane: 2, Corrections: 1, EnqueuedAt: enqueued.Add(time.Minute)}, Progress: &merge.Progress{QueueID: "second", Corrections: 2}},
		{ID: "first", Identifier: "OF-1", Repo: "a", Owner: machine.OwnerMerge, Phase: machine.PhaseMergeQueue, Handoff: &merge.Handoff{QueueID: "first", Lane: 1, EnqueuedAt: enqueued}},
		{ID: "deployed", Identifier: "OF-3", Repo: "a", Owner: machine.OwnerNone, Phase: machine.PhaseMerged, Progress: &merge.Progress{QueueID: "deploy", MergeSHA: "merged-sha", Phase: "merged", Corrections: 1, Deployment: &merge.DeploymentProgress{Workflow: "deploy.yml", State: "pending", Reason: "waiting for workflow", Smoke: "private-smoke-command"}}},
		{ID: "tie-last", Identifier: "OF-4", Repo: "a", Owner: machine.OwnerMerge, Phase: machine.PhaseMergeQueue, Handoff: &merge.Handoff{QueueID: "z", EnqueuedAt: enqueued.Add(2 * time.Minute)}},
		{ID: "tie-first", Identifier: "OF-5", Repo: "a", Owner: machine.OwnerMerge, Phase: machine.PhaseMergeQueue, Handoff: &merge.Handoff{QueueID: "a", EnqueuedAt: enqueued.Add(2 * time.Minute)}},
		{ID: "other-repo", Identifier: "OF-6", Repo: "b", Owner: machine.OwnerMerge, Phase: machine.PhaseMergeQueue, Handoff: &merge.Handoff{QueueID: "other", EnqueuedAt: enqueued.Add(time.Hour)}},
		{ID: "missing", Identifier: "OF-7", Repo: "a", Owner: machine.OwnerMerge, Phase: machine.PhaseMergeQueue},
		{ID: "conflicting", Identifier: "OF-8", Repo: "a", Owner: machine.OwnerMerge, Phase: machine.PhaseMergeQueue, MergeProblem: "conflicting handoff", Handoff: &merge.Handoff{QueueID: "conflict", EnqueuedAt: enqueued.Add(-time.Hour)}},
		{ID: "merged", Identifier: "OF-9", Repo: "a", Owner: machine.OwnerMerge, Phase: machine.PhaseMerged, Handoff: &merge.Handoff{QueueID: "merged", Lane: 3, Corrections: 2, EnqueuedAt: enqueued.Add(-time.Hour)}, Progress: &merge.Progress{QueueID: "earlier", Corrections: 3}},
		{ID: "malformed", Identifier: "OF-10", Repo: "a", Owner: machine.OwnerMerge, Phase: machine.PhaseMerged, Handoff: &merge.Handoff{QueueID: "malformed", Lane: 4, Corrections: -1, EnqueuedAt: enqueued.Add(-time.Hour)}},
	}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(status.PushPath())
	if err != nil {
		t.Fatal(err)
	}
	var live status.View
	if err := json.Unmarshal(data, &live); err != nil {
		t.Fatal(err)
	}
	var legacy status.View
	for _, name := range []string{"live", "snapshot"} {
		t.Run(name, func(t *testing.T) {
			contents := data
			if name == "snapshot" {
				if err := status.Write(status.Snapshot{GeneratedAt: s.Now(), Tickets: SnapshotRows(f.tickets, s.Repositories)}); err != nil {
					t.Fatal(err)
				}
				contents, err = os.ReadFile(status.Path())
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(contents, &legacy); err != nil {
					t.Fatal(err)
				}
			}
			var view struct {
				Tickets []struct {
					Identifier    string             `json:"identifier"`
					QueuePosition *int               `json:"queuePosition"`
					Deployment    *status.Deployment `json:"deployment"`
					Lane          json.RawMessage    `json:"lane"`
					Corrections   map[string]int     `json:"corrections"`
				} `json:"tickets"`
			}
			if err := json.Unmarshal(contents, &view); err != nil {
				t.Fatal(err)
			}
			if len(view.Tickets) != 10 {
				t.Errorf("tickets = %d, want 10 including the unowned deployment barrier", len(view.Tickets))
			}
			positions := map[string]int{"OF-1": 1, "OF-2": 2, "OF-4": 4, "OF-5": 3, "OF-6": 1}
			lanes := map[string]string{"OF-1": "1", "OF-2": "2", "OF-9": "3"}
			unused := map[string]int{"count": 0, "limit": 3}
			corrections := map[string]map[string]int{"OF-1": unused, "OF-2": {"count": 2, "limit": 3}, "OF-4": unused, "OF-5": unused, "OF-6": unused, "OF-8": unused, "OF-9": {"count": 2, "limit": 3}}
			foundDeployment := false
			for _, row := range view.Tickets {
				if want, queued := positions[row.Identifier]; queued {
					if row.QueuePosition == nil || *row.QueuePosition != want {
						t.Errorf("%s queuePosition = %v, want %d", row.Identifier, row.QueuePosition, want)
					}
				} else if row.QueuePosition != nil {
					t.Errorf("%s unexpectedly has queuePosition %d", row.Identifier, *row.QueuePosition)
				}
				if string(row.Lane) != lanes[row.Identifier] {
					t.Errorf("%s lane = %q, want %q", row.Identifier, string(row.Lane), lanes[row.Identifier])
				}
				if want := corrections[row.Identifier]; !reflect.DeepEqual(row.Corrections, want) {
					t.Errorf("%s corrections = %v, want %v", row.Identifier, row.Corrections, want)
				}
				if row.Identifier == "OF-3" {
					foundDeployment = true
					want := status.Deployment{QueueID: "deploy", SHA: "merged-sha", Workflow: "deploy.yml", State: "pending", Reason: "waiting for workflow"}
					if row.Deployment == nil || *row.Deployment != want {
						t.Errorf("deployment = %+v, want %+v", row.Deployment, want)
					}
				} else if row.Deployment != nil {
					t.Errorf("%s unexpectedly has deployment %+v", row.Identifier, row.Deployment)
				}
			}
			if !foundDeployment {
				t.Error("unowned deployment barrier is missing")
			}
			if bytes.Contains(contents, []byte("private-smoke-command")) || bytes.Contains(contents, []byte(`"smoke"`)) {
				t.Error("deployment exposes smoke command")
			}
		})
	}
	liveRows := make(map[string]status.Ticket, len(live.Tickets))
	for _, row := range live.Tickets {
		liveRows[row.Identifier] = row
	}
	legacyRows := make(map[string]status.Ticket, len(legacy.Tickets))
	for _, row := range legacy.Tickets {
		legacyRows[row.Identifier] = row
	}
	if len(liveRows) != len(legacyRows) {
		t.Errorf("live tickets = %d, snapshot tickets = %d", len(liveRows), len(legacyRows))
	}
	for identifier, liveRow := range liveRows {
		legacyRow, found := legacyRows[identifier]
		if !found {
			t.Errorf("%s is missing from snapshot", identifier)
			continue
		}
		if liveRow.QueuePosition != legacyRow.QueuePosition {
			t.Errorf("%s queuePosition differs: live = %d, snapshot = %d", identifier, liveRow.QueuePosition, legacyRow.QueuePosition)
		}
		if !reflect.DeepEqual(liveRow.Deployment, legacyRow.Deployment) {
			t.Errorf("%s deployment differs: live = %+v, snapshot = %+v", identifier, liveRow.Deployment, legacyRow.Deployment)
		}
		if liveRow.Lane != legacyRow.Lane || !reflect.DeepEqual(liveRow.Corrections, legacyRow.Corrections) {
			t.Errorf("%s lane or corrections differ: live = %d %+v, snapshot = %d %+v", identifier, liveRow.Lane, liveRow.Corrections, legacyRow.Lane, legacyRow.Corrections)
		}
	}
}

type pushBackend struct {
	*fakeBackend
	t *testing.T
}

func (b *pushBackend) Launch(ctx context.Context, ticket Ticket) error {
	data, err := os.ReadFile(status.PushPath())
	if err != nil {
		b.t.Fatal(err)
	}
	var view status.View
	if json.Unmarshal(data, &view) != nil || len(view.Tickets) != 1 || view.Tickets[0].Owner != string(machine.OwnerExecutor) {
		b.t.Fatalf("before launch: %s", data)
	}
	return b.fakeBackend.Launch(ctx, ticket)
}
func TestStatusFilePush(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := &fakeBackend{tickets: []Ticket{{ID: "one", Identifier: "ZII-891", Repo: "a", Title: "冒烟测试", Candidate: true}}}
	s := setup(f)
	s.Backend = &pushBackend{f, t}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "orca-factory", "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var view status.View
	if json.Unmarshal(data, &view) != nil || len(view.Tickets) != 1 || !view.GeneratedAt.Equal(s.Now()) || view.Tickets[0].Title != "冒烟测试" || view.Tickets[0].Since.IsZero() {
		t.Fatalf("tick end: %s", data)
	}
}

// Candidate (待领) and human (转人) tickets are written to the status push, not only in-flight ones.
func TestStatusFilePushCandidateAndHuman(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	// applyErr keeps the candidate unclaimed so its candidate stage survives to the push.
	f := &fakeBackend{applyErr: errors.New("write outcome unknown"), tickets: []Ticket{
		{ID: "c", Identifier: "ZII-891", Repo: "a", Candidate: true},
		{ID: "h", Identifier: "ZII-892", Repo: "a", Owner: machine.OwnerHuman, Phase: machine.PhaseHuman},
		{ID: "w", Identifier: "ZII-893", Repo: "a", Owner: machine.OwnerExecutor, Phase: machine.PhaseGate},
	}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "orca-factory", "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var view status.View
	if json.Unmarshal(data, &view) != nil || len(view.Tickets) != 3 {
		t.Fatalf("tickets: %s", data)
	}
	stages := map[string]string{}
	for _, tk := range view.Tickets {
		stages[tk.Identifier] = tk.Stage
	}
	if stages["ZII-891"] != "candidate" || stages["ZII-892"] != "human" || stages["ZII-893"] != "" {
		t.Fatalf("stages: %v", stages)
	}
}

type observationBackend struct {
	*fakeBackend
	fail bool
}

func (b *observationBackend) Snapshot(ctx context.Context, repos []config.Repository) ([]Ticket, error) {
	if b.fail {
		return nil, errors.New("snapshot unavailable")
	}
	return b.fakeBackend.Snapshot(ctx, repos)
}

func TestFailedSnapshotPreservesStatusObservation(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	f := &fakeBackend{tickets: []Ticket{{Repo: "a", Identifier: "ZII-891", Owner: machine.OwnerExecutor, Phase: machine.PhaseGate}}}
	s := setup(f)
	backend := &observationBackend{fakeBackend: f}
	s.Backend = backend
	observed := s.Now()
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	backend.fail = true
	now := observed.Add(10 * time.Minute)
	s.Now = func() time.Time { return now }
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("expected failed snapshot")
	}
	view := status.Read(now, 0)
	if view.State != "stale" || !view.GeneratedAt.Equal(observed) || len(view.Tickets) != 1 {
		t.Fatalf("failed snapshot lost or renewed prior observation: %+v", view)
	}
	backend.fail = false
	f.tickets = nil
	now = now.Add(time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	view = status.Read(now, 0)
	if view.State != "ok" || !view.GeneratedAt.Equal(now) || len(view.Tickets) != 0 {
		t.Fatalf("successful empty snapshot did not renew observation: %+v", view)
	}
}

func TestStatusPushShowsBackendURLHeldAndBlockReason(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := &fakeBackend{applyErr: errors.New("write outcome unknown"), tickets: []Ticket{
		{ID: "b", Identifier: "ZII-891", Repo: "a", Candidate: true, Blocked: true, Blockers: []string{"ZII-1"}, URL: "https://github.com/o/r/issues/891"},
		{ID: "h", Identifier: "ZII-892", Repo: "a", Candidate: true, Blocked: true, BlockersIncomplete: true, Held: true},
	}}
	s := setup(f)
	s.Repositories[0].Env = map[string]string{"AGENT_ISSUE_BACKEND": "github"}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "orca-factory", "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var view status.View
	if err := json.Unmarshal(data, &view); err != nil || len(view.Tickets) != 2 {
		t.Fatalf("tickets: %s", data)
	}
	rows := map[string]status.Ticket{}
	for _, tk := range view.Tickets {
		rows[tk.Identifier] = tk
	}
	blocked, held := rows["ZII-891"], rows["ZII-892"]
	if blocked.Backend != "github" || blocked.URL != "https://github.com/o/r/issues/891" || blocked.Held || blocked.BlockedReason != "blocked by ZII-1" {
		t.Fatalf("blocked row: %+v", blocked)
	}
	if !held.Held || held.BlockedReason != "import held" {
		t.Fatalf("held row: %+v", held)
	}
}
