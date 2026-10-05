package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type lintFailWriter struct {
	err   error
	allow int
}

func (w *lintFailWriter) Write(p []byte) (int, error) {
	if w.allow > 0 {
		w.allow--
		return len(p), nil
	}
	return 0, w.err
}

type lintSnapshotBackend struct {
	scheduler.Backend
	tickets []scheduler.Ticket
	err     error
}

func (b lintSnapshotBackend) Snapshot(context.Context, []config.Repository) ([]scheduler.Ticket, error) {
	return b.tickets, b.err
}
func TestLintDryOutputErrors(t *testing.T) {
	failure := errors.New("output failed")
	ctx := context.Background()
	for _, deployment := range []bool{false, true} {
		ticket := scheduler.Ticket{Identifier: "ENG-1"}
		if deployment {
			ticket.Progress = &merge.Progress{Deployment: &merge.DeploymentProgress{}}
		}
		b := &dryBackend{Backend: lintSnapshotBackend{tickets: []scheduler.Ticket{ticket}, err: context.Canceled}, out: &lintFailWriter{err: failure}}
		_, err := b.Snapshot(ctx, nil)
		if !errors.Is(err, failure) || !errors.Is(err, context.Canceled) {
			t.Errorf("snapshot lost output or context error: %v", err)
		}
	}
	b := &dryBackend{out: &lintFailWriter{err: failure}}
	if err := b.Apply(ctx, scheduler.Ticket{}, scheduler.Action{}); !errors.Is(err, failure) {
		t.Errorf("apply: %v", err)
	}
	if err := b.Launch(ctx, scheduler.Ticket{}); !errors.Is(err, failure) {
		t.Errorf("launch: %v", err)
	}
	if _, err := b.InspectLegacy(ctx, scheduler.Ticket{}); !errors.Is(err, failure) || !strings.Contains(err.Error(), "legacy verification requires git fetch") {
		t.Errorf("legacy: %v", err)
	}
	trap := &writeTrapBackend{tickets: []scheduler.Ticket{{ID: "a", Identifier: "ENG-1", Repo: "r", Owner: machine.OwnerMerge}}}
	cfg := config.Config{MaxInFlight: 2, TokenBucket: config.TokenBucket{Capacity: 2, RefillPerSecond: 1}, Repositories: []config.Repository{{Path: "r", MaxInFlight: 2, StaleHours: 8}}}
	if err := dryRunTick(ctx, cfg, trap, &lintFailWriter{err: failure, allow: 1}, time.Now); !errors.Is(err, failure) {
		t.Errorf("merge-lane output: %v", err)
	}
}
func TestLintRecoveryOutputFailure(t *testing.T) {
	for _, dry := range []bool{false, true} {
		args := []string{"--issue", "ENG-1", "--queue", "q", "--sha", strings.Repeat("a", 40), "--action", "confirm-passed", "--operator", "Alice", "--reason", "verified"}
		if dry {
			args = append(args, "--dry-run")
		}
		calls := 0
		code := runDeploymentRecovery(args, &lintFailWriter{err: errors.New("broken pipe")}, func(context.Context, string, string, string, string, string, string, bool) error { calls++; return nil })
		if code != 1 || calls != 1 {
			t.Errorf("dry=%v code=%d calls=%d", dry, code, calls)
		}
	}
}
