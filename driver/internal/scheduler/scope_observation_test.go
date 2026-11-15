package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func TestScopeWaitObservationNamesOccupierAndMatchedPaths(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	f := &scopeBackend{fakeBackend: &fakeBackend{}, found: true, files: []string{"src/callback/payload.ts", "docs/driver.md"}}
	s := setup(f.fakeBackend)
	s.Backend = f
	f.tickets = []Ticket{
		{ID: "530", Identifier: "WAW-530", Repo: "b", Owner: machine.OwnerResolver, Terminal: "active", Scope: []string{"src/*", "docs/*"}},
		{ID: "532", Identifier: "WAW-532", Repo: "b", Candidate: true, Scope: []string{"src/callback/*"}},
		{ID: "533", Identifier: "WAW-533", Repo: "b", Candidate: true, Scope: []string{"docs/driver.md"}},
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 0 || len(f.launched) != 0 {
		t.Fatal("scope safety changed")
	}
	raw, err := os.ReadFile(status.PushPath())
	if err != nil {
		t.Fatal(err)
	}
	var view struct{ Tickets []map[string]any }
	if err = json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	for _, row := range view.Tickets {
		if row["identifier"] == "WAW-530" {
			continue
		}
		reason, _ := row["blockedReason"].(string)
		if !strings.Contains(reason, "scope") || !strings.Contains(reason, "WAW-530") {
			t.Fatalf("scope wait remains unexplained: %s", mustDependencyJSON(row))
		}
		waits, ok := row["scopeWaits"].([]any)
		if !ok || len(waits) != 1 {
			t.Fatalf("missing matched scope evidence: %s", mustDependencyJSON(row))
		}
		match := waits[0].(map[string]any)
		wantPath, wantOccupied, wantRule := "src/callback/*", "src/callback/payload.ts", "candidate-pattern"
		if row["identifier"] == "WAW-533" {
			wantPath, wantOccupied, wantRule = "docs/driver.md", "docs/driver.md", "equal"
		}
		if match["identifier"] != "WAW-530" || match["candidatePath"] != wantPath || match["occupiedPath"] != wantOccupied || match["rule"] != wantRule || match["source"] != "actual" {
			t.Fatalf("match=%s", mustDependencyJSON(match))
		}
		if _, has := row["dependencies"]; has {
			t.Fatal("scope conflict became a dependency")
		}
	}
	if f.reads != 1 {
		t.Fatalf("scope evidence read twice: %d", f.reads)
	}
}
