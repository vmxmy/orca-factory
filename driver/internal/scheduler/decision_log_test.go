package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/pause"
)

func captureDecisions(t *testing.T, s *Scheduler) []map[string]any {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "decisions")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = file
	defer func() {
		os.Stderr = old
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("log %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestSchedulerDecisionLog(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{
		{ID: "blocked", Identifier: "ZII-2", Repo: "b", Candidate: true, Blocked: true, Blockers: []string{"ZII-1"}},
		{ID: "free", Identifier: "ZII-3", Repo: "b", Candidate: true},
		{ID: "existing", Identifier: "ZII-1", Repo: "a", Owner: machine.OwnerMerge},
	}}
	rows := captureDecisions(t, setup(f))
	var skipped, claimed, summary, launched int
	for _, row := range rows {
		switch row["event"] {
		case "skip":
			if row["ticket"] == "ZII-2" && row["reason"] == "blocked" {
				skipped++
				if !reflect.DeepEqual(row["blockers"], []any{"ZII-1"}) {
					t.Fatalf("missing blockers: %v", row)
				}
			}
		case "claim":
			if row["ticket"] == "ZII-3" {
				claimed++
			}
		case "launch":
			if row["ticket"] == "ZII-3" && row["result"] == "success" {
				launched++
			}
		case "summary":
			summary++
			limits, ok := row["repository_limits"].(map[string]any)
			if !ok || limits["a"] != float64(1) || limits["b"] != float64(3) || row["in_flight"] != float64(1) || row["candidates"] != float64(2) || row["paused"] != false || row["duration_ms"] == nil {
				t.Fatalf("incomplete summary: %v", row)
			}
		}
	}
	if skipped != 1 || claimed != 1 || summary != 1 || launched != 1 {
		t.Fatalf("skip=%d claim=%d summary=%d launch=%d logs=%v", skipped, claimed, summary, launched, rows)
	}
}

func TestSchedulerDecisionLogSkipsEveryCandidate(t *testing.T) {
	for _, reason := range []string{"capacity", "scope", "paused", "legacy", "token"} {
		t.Run(reason, func(t *testing.T) {
			f := &fakeBackend{tickets: []Ticket{
				{ID: "one", Identifier: "ZII-1", Repo: "b", Candidate: true, Scope: []string{"shared/*"}},
				{ID: "two", Identifier: "ZII-2", Repo: "b", Candidate: true, Scope: []string{"shared/a"}},
				{ID: "three", Identifier: "ZII-3", Repo: "b", Candidate: true, Scope: []string{"shared/b"}},
			}}
			s := setup(f)
			want := 2
			switch reason {
			case "capacity":
				s.Config.MaxInFlight = 1
			case "token":
				s.Config.TokenBucket.Capacity = 1
				s.Repositories[1].Env = map[string]string{"AGENT_SCOPE_AVOID": "0"}
			case "paused":
				s.Pauses = pause.New(s.clock, nil, nil)
				s.Pauses.Pause(pause.Key{Repo: "b", Dependency: pause.GitHub}, time.Time{})
				want = 3
			case "legacy":
				for i := range f.tickets {
					f.tickets[i].Legacy = true
				}
				want = 3
			}
			rows := captureDecisions(t, s)
			skipped := 0
			for _, row := range rows {
				if row["event"] == "skip" && row["reason"] == reason {
					skipped++
				}
			}
			if skipped != want {
				t.Fatalf("want %d %s skips, got %d: %v", want, reason, skipped, rows)
			}
		})
	}
}

func TestSchedulerOwnershipDecisionLog(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		absent, inactive         bool
		terminalErr, dispatchErr error
		terminal, dispatch       any
		result                   string
	}{
		{"dead", true, true, nil, nil, false, false, "dead"},
		{"terminal present", false, true, nil, nil, true, false, "alive"},
		{"dispatch active", true, false, nil, nil, false, true, "alive"},
		{"terminal unreadable", true, true, errors.New("offline"), nil, nil, false, "uncertain"},
		{"dispatch unreadable", true, true, nil, errors.New("offline"), false, nil, "uncertain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBackend{absent: tc.absent, inactive: tc.inactive, terminalErr: tc.terminalErr, dispatchErr: tc.dispatchErr,
				tickets: []Ticket{{ID: "owned", Identifier: "ZII-1", Repo: "b", Owner: machine.OwnerExecutor, Attempt: 1, Terminal: "term-one", Run: "run-one"}}}
			rows := captureDecisions(t, setup(f))
			found := 0
			for _, row := range rows {
				if row["event"] != "ownership" {
					continue
				}
				found++
				if row["result"] != tc.result || row["terminal_exists"] != tc.terminal || row["active_dispatch"] != tc.dispatch || row["terminal"] != "term-one" || row["run"] != "run-one" {
					t.Fatalf("wrong ownership evidence: %v", row)
				}
			}
			if found != 1 {
				t.Fatalf("ownership logs=%v", rows)
			}
		})
	}
}

func TestSchedulerPausedSummary(t *testing.T) {
	f := &fakeBackend{}
	s := setup(f)
	s.Pauses = pause.New(s.clock, nil, nil)
	s.Pauses.Pause(pause.Key{Dependency: pause.Orca}, time.Time{})
	rows := captureDecisions(t, s)
	if f.snapshots != 0 || len(rows) != 1 || rows[0]["event"] != "summary" || rows[0]["paused"] != true || rows[0]["snapshot_available"] != false {
		t.Fatalf("paused logs=%v snapshots=%d", rows, f.snapshots)
	}
}

type uncertainLaunchBackend struct{ fakeBackend }

func (f *uncertainLaunchBackend) Launch(context.Context, Ticket) error {
	return errors.New("launch failed ghp_private")
}

func TestSchedulerLaunchErrorLog(t *testing.T) {
	var output bytes.Buffer
	f := &uncertainLaunchBackend{fakeBackend: fakeBackend{tickets: []Ticket{{ID: "one", Identifier: "ZII-1", Repo: "b", Candidate: true}}}}
	s := setup(nil)
	s.Backend = f
	s.Logger = logx.New("scheduler", &output)
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("lost launch failure")
	}
	if strings.Contains(output.String(), "ghp_private") {
		t.Fatal("unredacted launch error")
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["event"] == "launch" {
			found = true
			if row["result"] != "uncertain" || row["err"] != "launch failed [REDACTED]" {
				t.Fatalf("launch log=%v", row)
			}
		}
	}
	if !found {
		t.Fatal("missing launch error log")
	}
	if err := s.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "ZII-1") || !strings.Contains(err.Error(), "launch failed [REDACTED]") {
		t.Fatalf("launch problem lacks ticket or reason: %v", err)
	}
}

type staleLaunchBackend struct{ fakeBackend }

func (f *staleLaunchBackend) Launch(context.Context, Ticket) error {
	return errLaunchEvidenceChanged
}

// A launch refused because the ticket moved on since the snapshot is a safe
// deferral to the next round, not a failure.
func TestSchedulerLaunchDeferredOnEvidenceChange(t *testing.T) {
	var output bytes.Buffer
	f := &staleLaunchBackend{fakeBackend: fakeBackend{tickets: []Ticket{{ID: "one", Identifier: "ZII-1", Repo: "b", Candidate: true}}}}
	s := setup(nil)
	s.Backend = f
	s.Logger = logx.New("scheduler", &output)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("deferred launch reported as failure: %v", err)
	}
	if !strings.Contains(output.String(), `"result":"deferred"`) {
		t.Fatalf("deferred launch not logged: %s", output.String())
	}
}

type refusingLaunchBackend struct {
	fakeBackend
	launchErr error
}

func (f *refusingLaunchBackend) Launch(context.Context, Ticket) error {
	return f.launchErr
}

func deferredEvents(t *testing.T, output string) map[string]bool {
	t.Helper()
	events := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["result"] == "deferred" {
			if row["err"] == nil || row["err"] == "" {
				t.Fatalf("deferred decision lost its reason: %v", row)
			}
			events[row["event"].(string)] = true
		}
	}
	return events
}

// A maintenance refusal is known not to have reached any external system, so
// claim, recovery and launch wait for the next round instead of failing the tick.
func TestSchedulerMaintenanceRefusalDeferred(t *testing.T) {
	tickets := []Ticket{
		{ID: "one", Identifier: "ZII-1", Repo: "b", Candidate: true},
		{ID: "two", Identifier: "ZII-2", Repo: "b", Owner: machine.OwnerExecutor, Attempt: 1, UpdatedAt: setup(nil).Now()},
	}
	for _, applyErr := range []error{fmt.Errorf("apply: %w", maintenance.ErrRefused), errResnapshot} {
		var applyOutput bytes.Buffer
		s := setup(nil)
		s.Backend = &fakeBackend{tickets: tickets, absent: true, inactive: true, applyErr: applyErr}
		s.Logger = logx.New("scheduler", &applyOutput)
		if err := s.Tick(context.Background()); err != nil {
			t.Fatalf("apply %v reported as failure: %v", applyErr, err)
		}
		if events := deferredEvents(t, applyOutput.String()); !events["claim"] || !events["apply"] {
			t.Fatalf("apply %v not deferred: %v\n%s", applyErr, events, applyOutput.String())
		}
	}

	for _, launchErr := range []error{fmt.Errorf("launch: %w", maintenance.ErrRefused), errReservationChanged} {
		var output bytes.Buffer
		s := setup(nil)
		s.Backend = &refusingLaunchBackend{fakeBackend: fakeBackend{tickets: tickets, absent: true, inactive: true}, launchErr: launchErr}
		s.Logger = logx.New("scheduler", &output)
		if err := s.Tick(context.Background()); err != nil {
			t.Fatalf("launch %v reported as failure: %v", launchErr, err)
		}
		if events := deferredEvents(t, output.String()); !events["launch"] {
			t.Fatalf("launch %v not deferred:\n%s", launchErr, output.String())
		}
	}
}
