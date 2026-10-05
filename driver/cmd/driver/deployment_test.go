package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func TestDeploymentStatusAndDryRunObserveHumanBarrier(t *testing.T) {
	ticket := scheduler.Ticket{ID: "id", Identifier: "ENG-1", Repo: "r", Owner: machine.OwnerHuman, Progress: &merge.Progress{QueueID: "q1", MergeSHA: strings.Repeat("a", 40), Deployment: &merge.DeploymentProgress{State: "paused", Reason: "smoke uncertain", Workflow: "Deploy", Smoke: "secret-command"}}}
	trap := &writeTrapBackend{tickets: []scheduler.Ticket{ticket}}
	cfg := config.Config{MaxInFlight: 2, TokenBucket: config.TokenBucket{Capacity: 2, RefillPerSecond: 1}, Repositories: []config.Repository{{Path: "r", MaxInFlight: 2, StaleHours: 8}}}
	var out bytes.Buffer
	if err := dryRunTick(context.Background(), cfg, trap, &out, time.Now); err != nil {
		t.Fatal(err)
	}
	if trap.writes != 0 || !strings.Contains(out.String(), "smoke uncertain") || !strings.Contains(out.String(), "q1") || strings.Contains(out.String(), "secret-command") {
		t.Fatalf("dryrun invisible or mutating: %d %s", trap.writes, out.String())
	}
	snapshot := collectStatus(&scheduler.Scheduler{}, &statusBackend{tickets: []scheduler.Ticket{ticket}}, nil, time.Now())
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(t.TempDir(), "status.json"))
	if err := status.Write(snapshot); err != nil {
		t.Fatal(err)
	}
	raw, rawMarshalErr := json.Marshal(status.Read(time.Now()))
	if rawMarshalErr != nil {
		t.Fatal(rawMarshalErr)
	}
	if !strings.Contains(string(raw), "smoke uncertain") || !strings.Contains(string(raw), "q1") || strings.Contains(string(raw), "secret-command") {
		t.Fatalf("status invisible or leaking: %s", raw)
	}
}
func TestServeDryRunDoesNotWriteLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_DRIVER_STATE_ROOT", filepath.Join(home, "state"))
	if code := serve([]string{"--dry-run"}); code != 0 {
		t.Fatalf("dryrun failed: %d", code)
	}
	if _, err := os.Stat(filepath.Join(home, "state")); !os.IsNotExist(err) {
		t.Fatal("dry-run created logs")
	}
}

func TestResumeDeploymentCommandRequiresExplicitIdentity(t *testing.T) {
	command := commands["resume-deployment"]
	if command == nil {
		t.Fatal("manual resume command is not registered")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, args := range [][]string{nil, {"--issue", "ENG-1"}, {"--issue", "ENG-1", "--queue", "q1", "--sha", strings.Repeat("a", 40), "--action", "confirm-passed"}} {
		if code := command(args); code != 2 {
			t.Fatalf("incomplete confirmation accepted: %v = %d", args, code)
		}
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatal("invalid resume created local state")
	}
}

func TestResumeDeploymentDryRunPropagatesDecisionWithoutExecution(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	code := runDeploymentRecovery([]string{"--issue", "ENG-1", "--queue", "q1", "--sha", strings.Repeat("a", 40), "--action", "confirm-passed", "--operator", "Alice", "--reason", "checked production", "--dry-run"}, &out, func(ctx context.Context, issue, queue, sha, action, operator, reason string, dry bool) error {
		calls++
		if issue != "ENG-1" || queue != "q1" || sha != strings.Repeat("a", 40) || action != "confirm-passed" || operator != "Alice" || reason != "checked production" || !dry {
			t.Fatal("recovery decision changed at command boundary")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded recovery")
		}
		return nil
	})
	if code != 0 || calls != 1 || !strings.Contains(out.String(), "no changes") {
		t.Fatalf("dryrun adapter failed: code=%d calls=%d %s", code, calls, out.String())
	}
}
