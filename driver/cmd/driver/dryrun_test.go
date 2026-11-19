package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/hostmem"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

type writeTrapBackend struct {
	tickets []scheduler.Ticket
	writes  int
}

func (b *writeTrapBackend) Snapshot(context.Context, []config.Repository) ([]scheduler.Ticket, error) {
	return b.tickets, nil
}
func (b *writeTrapBackend) AutomationDisabled(context.Context, config.Repository) (bool, error) {
	return true, nil
}
func (b *writeTrapBackend) TerminalAbsent(context.Context, scheduler.Ticket) (bool, error) {
	return true, nil
}
func (b *writeTrapBackend) NoActiveDispatch(context.Context, scheduler.Ticket) (bool, error) {
	return true, nil
}
func (b *writeTrapBackend) InspectLegacy(context.Context, scheduler.Ticket) (scheduler.Legacy, error) {
	return scheduler.Legacy{}, nil
}
func (b *writeTrapBackend) Apply(context.Context, scheduler.Ticket, scheduler.Action) error {
	b.writes++
	return nil
}
func (b *writeTrapBackend) Launch(context.Context, scheduler.Ticket) error { b.writes++; return nil }

func stubHostMemory(t *testing.T, reading hostmem.Reading) {
	t.Helper()
	previous := readHostMemory
	readHostMemory = func() (hostmem.Reading, error) { return reading, nil }
	t.Cleanup(func() { readHostMemory = previous })
}

func TestDryRunPrintsPlanWithoutWrites(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	stubHostMemory(t, hostmem.Reading{Source: "host", Available: 30 << 30})
	trap := &writeTrapBackend{tickets: []scheduler.Ticket{
		{ID: "a", Identifier: "ZII-1", Repo: "r", Candidate: true, CreatedAt: now},
		{ID: "b", Identifier: "ZII-2", Repo: "r", Candidate: true, Blocked: true, CreatedAt: now},
	}}
	var out bytes.Buffer
	cfg, repos := config.Config{MaxInFlight: 4, TokenBucket: config.TokenBucket{Capacity: 4, RefillPerSecond: 1}}, []config.Repository{{Path: "r", MaxInFlight: 4, StaleHours: 8}}
	if err := dryRunTick(context.Background(), cfg, repos, trap, &out, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	if trap.writes != 0 {
		t.Fatalf("dry run wrote %d times", trap.writes)
	}
	got := out.String()
	for _, want := range []string{"would claim ZII-1", "would launch ZII-1", "ZII-2 blocked"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestDryRunSkipsLaunchesWhileHostMemoryHeld(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	cfg, repos := config.Config{MaxInFlight: 4, TokenBucket: config.TokenBucket{Capacity: 4, RefillPerSecond: 1}}, []config.Repository{{Path: "r", MaxInFlight: 4, StaleHours: 8}}
	for _, tc := range []struct {
		name      string
		published *status.HostMemory
		own       uint64
		source    string
	}{
		{"no running serve, own reading below hold line", nil, 5 << 30, "host"},
		{"serve held in its slice, this shell has room", &status.HostMemory{Paused: true, Reading: hostmem.Reading{Source: "/orca.slice", Available: 8 << 30}}, 20 << 30, "/orca.slice"},
		{"serve published room, this shell is below hold line", &status.HostMemory{Reading: hostmem.Reading{Source: "host", Available: 20 << 30}}, 3 << 30, "host"},
		{"serve without reading source held", &status.HostMemory{Paused: true, Reading: hostmem.Reading{Available: 5 << 30}}, 20 << 30, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			t.Setenv("DRIVER_STATUS_FILE", "")
			if tc.published != nil {
				status.PushObservation(now, now, func(view *status.View) { view.HostMemory = *tc.published })
			}
			stubHostMemory(t, hostmem.Reading{Source: "host", Available: tc.own})
			trap := &writeTrapBackend{tickets: []scheduler.Ticket{{ID: "a", Identifier: "ZII-1", Repo: "r", Candidate: true, CreatedAt: now}}}
			var out bytes.Buffer
			if err := dryRunTick(context.Background(), cfg, repos, trap, &out, func() time.Time { return now }); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if strings.Contains(got, "would claim ZII-1") || strings.Contains(got, "would launch ZII-1") || !strings.Contains(got, `skip launches reason="memory" source="`+tc.source+`"`) {
				t.Fatalf("dry run under a memory hold:\n%s", got)
			}
		})
	}
}

// A reported resolver result is applied without a terminal, also under a hold.
func TestDryRunResolverResultIsNotALaunch(t *testing.T) {
	var out bytes.Buffer
	ticket := scheduler.Ticket{Identifier: "ZII-1", Owner: machine.OwnerResolver, Resolution: &resolver.Record{Phase: "result"}}
	if err := (&dryBackend{out: &out}).Launch(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "would launch") || !strings.Contains(got, "would apply resolver result ZII-1") {
		t.Fatalf("dry-run resolver result output: %q", got)
	}
}

// ActualChanges refreshes local git refs in the production Runtime.
func (b *writeTrapBackend) ActualChanges(context.Context, scheduler.Ticket) ([]string, bool, error) {
	b.writes++
	return []string{"file"}, true, nil
}
func (b *writeTrapBackend) AdvanceMerges(context.Context, []scheduler.Ticket) error {
	b.writes++
	return nil
}
func TestDryRunMergeOwnershipDoesNotFetchOrWrite(t *testing.T) {
	trap := &writeTrapBackend{tickets: []scheduler.Ticket{{ID: "a", Identifier: "ENG-1", Repo: "r", Owner: machine.OwnerMerge}}}
	cfg, repos := config.Config{MaxInFlight: 2, TokenBucket: config.TokenBucket{Capacity: 2, RefillPerSecond: 1}}, []config.Repository{{Path: "r", MaxInFlight: 2, StaleHours: 8}}
	var out bytes.Buffer
	if err := dryRunTick(context.Background(), cfg, repos, trap, &out, time.Now); err != nil {
		t.Fatal(err)
	}
	if trap.writes != 0 {
		t.Fatalf("dry-run performed %d mutating operations", trap.writes)
	}
}

type legacyWriteTrap struct{ *writeTrapBackend }

func (b *legacyWriteTrap) InspectLegacy(context.Context, scheduler.Ticket) (scheduler.Legacy, error) {
	b.writes++
	return scheduler.Legacy{Complete: true}, nil
}
func TestDryRunLegacyDoesNotFetch(t *testing.T) {
	trap := &legacyWriteTrap{&writeTrapBackend{tickets: []scheduler.Ticket{{ID: "a", Identifier: "ENG-1", Repo: "r", Legacy: true}}}}
	cfg, repos := config.Config{MaxInFlight: 2, TokenBucket: config.TokenBucket{Capacity: 2, RefillPerSecond: 1}}, []config.Repository{{Path: "r", MaxInFlight: 2, StaleHours: 8}}
	var out bytes.Buffer
	_ = dryRunTick(context.Background(), cfg, repos, trap, &out, time.Now)
	if trap.writes != 0 {
		t.Fatalf("dry-run legacy reconciliation mutated git: %d", trap.writes)
	}
}
