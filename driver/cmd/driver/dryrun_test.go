package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
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

func TestDryRunPrintsPlanWithoutWrites(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	trap := &writeTrapBackend{tickets: []scheduler.Ticket{
		{ID: "a", Identifier: "ZII-1", Repo: "r", Candidate: true, CreatedAt: now},
		{ID: "b", Identifier: "ZII-2", Repo: "r", Candidate: true, Blocked: true, CreatedAt: now},
	}}
	var out bytes.Buffer
	cfg := config.Config{MaxInFlight: 4, TokenBucket: config.TokenBucket{Capacity: 4, RefillPerSecond: 1}, Repositories: []config.Repository{{Path: "r", MaxInFlight: 4, StaleHours: 8}}}
	if err := dryRunTick(context.Background(), cfg, trap, &out, func() time.Time { return now }); err != nil {
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
