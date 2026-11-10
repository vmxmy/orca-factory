package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func TestHumanObservationFields(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(t.TempDir(), "status.json"))
	// resolver-escalate-scope.json is stdout of the real resolver model given
	// core/roles/resolver.md and a written scenario.
	scope := &resolver.Record{Phase: "human", Result: string(recorded(t, "resolver-escalate-scope.json"))}
	f := &fakeBackend{tickets: []Ticket{
		{Identifier: "scope", Repo: "a", Owner: machine.OwnerHuman, Resolution: scope, NotifyReason: "扩大范围\r\n等待批准"},
		{Identifier: "deploy", Repo: "a", Owner: machine.OwnerHuman, NotifyReason: "fallback", Progress: &merge.Progress{Phase: "merged", MergeSHA: "merged-sha", Deployment: &merge.DeploymentProgress{State: "paused", Reason: "smoke failed"}}},
		{Identifier: "takeover", Repo: "a", Owner: machine.OwnerHuman, NotifyReason: "manual review"},
		{Identifier: "long", Repo: "a", Owner: machine.OwnerHuman, NotifyReason: strings.Repeat("原", 119) + "\n因extra"},
		{Identifier: "active", Repo: "a", Owner: machine.OwnerExecutor, NotifyReason: "must be omitted", Resolution: scope},
		{Identifier: "unmerged", Repo: "a", Owner: machine.OwnerHuman, NotifyReason: "not merged", Progress: &merge.Progress{Deployment: &merge.DeploymentProgress{State: "pending"}}},
		{Identifier: "passed", Repo: "a", Owner: machine.OwnerHuman, NotifyReason: "after deploy", Progress: &merge.Progress{MergeSHA: "merged-sha", Deployment: &merge.DeploymentProgress{State: "passed"}}},
	}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := status.Write(status.Snapshot{GeneratedAt: s.Now(), Tickets: SnapshotRows(f.tickets, s.Repositories, s.Now())}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{status.PushPath(), status.Path()} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var view struct {
			Tickets []struct {
				Identifier string  `json:"identifier"`
				Stage      string  `json:"stage"`
				Kind       *string `json:"humanKind"`
				Reason     *string `json:"humanReason"`
			} `json:"tickets"`
		}
		if err := json.Unmarshal(data, &view); err != nil {
			t.Fatal(err)
		}
		if len(view.Tickets) != 7 {
			t.Errorf("%s: got %d tickets, want 7", path, len(view.Tickets))
		}
		kinds := map[string]string{"scope": "scope", "deploy": "deploy", "takeover": "takeover", "long": "takeover", "unmerged": "takeover", "passed": "takeover"}
		reasons := map[string]string{"scope": "扩大范围 等待批准", "deploy": "smoke failed", "takeover": "manual review", "long": strings.Repeat("原", 119) + " ", "unmerged": "not merged", "passed": "after deploy"}
		for _, row := range view.Tickets {
			if row.Identifier == "active" {
				if row.Kind != nil || row.Reason != nil {
					t.Errorf("%s: active ticket has human fields", path)
				}
				continue
			}
			if row.Stage != "human" || row.Kind == nil || *row.Kind != kinds[row.Identifier] || row.Reason == nil || *row.Reason != reasons[row.Identifier] {
				t.Errorf("%s: %s stage=%q kind=%v reason=%v, want human/%s/%q", path, row.Identifier, row.Stage, row.Kind, row.Reason, kinds[row.Identifier], reasons[row.Identifier])
			}
		}
	}
}
