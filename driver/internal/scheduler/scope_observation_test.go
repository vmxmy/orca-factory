package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
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

func TestScopeWaitObservationUsesAdmissionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name             string
		files            []string
		found            bool
		err              error
		shared, disabled string
		want             int
		source           string
	}{
		{name: "actual disjoint", files: []string{"docs/only.md"}, found: true, want: 0},
		{name: "empty actual", files: []string{}, found: true, want: 0},
		{name: "declared fallback", err: errors.New("fetch failed"), want: 1, source: "declared"},
		{name: "missing branches", want: 1, source: "declared"},
		{name: "shared excluded", files: []string{"src/shared.ts"}, found: true, shared: "src/shared.ts", want: 0},
		{name: "disabled", files: []string{"src/main.ts"}, found: true, disabled: "0", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &scopeBackend{fakeBackend: &fakeBackend{}, files: tc.files, found: tc.found, err: tc.err}
			tickets := []Ticket{{Identifier: "WAW-530", Repo: "b", Owner: machine.OwnerResolver, Scope: []string{"src/*"}}, {Identifier: "WAW-532", Repo: "b", Candidate: true, Scope: []string{"src/*"}}}
			repo := config.Repository{Path: "b", Env: map[string]string{"AGENT_SCOPE_SHARED": tc.shared, "AGENT_SCOPE_AVOID": tc.disabled}}
			ObserveScopeWaits(context.Background(), tickets, []config.Repository{repo}, f)
			waits := tickets[1].ScopeWaits
			if len(waits) != tc.want {
				t.Fatalf("waits=%+v", waits)
			}
			if tc.want > 0 && waits[0].Source != tc.source {
				t.Fatalf("source=%s", waits[0].Source)
			}
			if tc.disabled == "0" && f.reads != 0 {
				t.Fatal("disabled scope read branches")
			}
		})
	}
	t.Run("candidate order is not predicted", func(t *testing.T) {
		tickets := []Ticket{{Identifier: "WAW-532", Repo: "b", Candidate: true, Scope: []string{"src/*"}}, {Identifier: "WAW-533", Repo: "b", Candidate: true, Scope: []string{"src/*"}}}
		ObserveScopeWaits(context.Background(), tickets, []config.Repository{{Path: "b"}}, nil)
		if len(tickets[0].ScopeWaits) != 0 || len(tickets[1].ScopeWaits) != 0 {
			t.Fatal("fresh observation predicted same-tick claims")
		}
		tickets[0].Candidate = false
		tickets[0].Owner = machine.OwnerResolver
		ObserveScopeWaits(context.Background(), tickets, []config.Repository{{Path: "b"}}, nil)
		if len(tickets[1].ScopeWaits) != 1 {
			t.Fatal("newly observed ownership did not reserve scope")
		}
	})
}

func TestScopeWaitObservationDoesNotCrossRepositories(t *testing.T) {
	tickets := []Ticket{{Identifier: "WAW-530", Repo: "a", Owner: machine.OwnerResolver, Scope: []string{"src/*"}}, {Identifier: "WAW-532", Repo: "b", Candidate: true, Scope: []string{"src/*"}}}
	ObserveScopeWaits(context.Background(), tickets, []config.Repository{{Path: "a"}, {Path: "b"}}, nil)
	if len(tickets[1].ScopeWaits) != 0 {
		t.Fatalf("cross-repository scope wait: %+v", tickets[1].ScopeWaits)
	}
}

func TestScopeWaitGeneratedFixtureMatchesObservation(t *testing.T) {
	repo := config.Repository{Path: "/fixture/wecom-agent-workbench"}
	tickets := []Ticket{{Identifier: "WAW-530", Repo: repo.Path, Owner: machine.OwnerResolver, Phase: machine.PhaseResolver, Terminal: "active", Scope: []string{"src/*", "docs/*"}}, {Identifier: "WAW-532", Repo: repo.Path, Candidate: true, Scope: []string{"src/callback/*"}}, {Identifier: "WAW-533", Repo: repo.Path, Candidate: true, Scope: []string{"docs/driver.md"}}}
	source := &scopeBackend{fakeBackend: &fakeBackend{}, found: true, files: []string{"src/callback/payload.ts", "docs/driver.md"}}
	ObserveScopeWaits(context.Background(), tickets, []config.Repository{repo}, source)
	var generated status.View
	if err := json.Unmarshal(recorded(t, "driver-status-json-scope-wait.json"), &generated); err != nil {
		t.Fatal(err)
	}
	want := ObservationRows(tickets, []config.Repository{repo}, generated.GeneratedAt)
	if !reflect.DeepEqual(generated.Tickets, want) {
		t.Fatalf("fixture differs from actual status: %s", mustDependencyJSON(want))
	}
}
