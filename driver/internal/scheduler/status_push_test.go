package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

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
	f := &fakeBackend{tickets: []Ticket{{ID: "one", Identifier: "ZII-891", Repo: "a", Candidate: true}}}
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
	if json.Unmarshal(data, &view) != nil || len(view.Tickets) != 1 || !view.GeneratedAt.Equal(s.Now()) {
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
