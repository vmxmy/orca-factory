package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
