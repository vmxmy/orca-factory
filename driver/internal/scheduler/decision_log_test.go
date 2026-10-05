package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
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
				s.Config.Repositories[1].Env = map[string]string{"AGENT_SCOPE_AVOID": "0"}
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
}
