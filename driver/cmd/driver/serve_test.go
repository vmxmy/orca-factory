package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

type statusFakeBackend struct {
	scheduler.Backend
	snapshots int
	tickets   []scheduler.Ticket
	err       error
}

func (b *statusFakeBackend) Snapshot(context.Context, []config.Repository) ([]scheduler.Ticket, error) {
	b.snapshots++
	return b.tickets, b.err
}
func (b *statusFakeBackend) ActualChanges(context.Context, scheduler.Ticket) ([]string, bool, error) {
	return []string{"actual.go"}, true, nil
}

type statusFakeClient struct {
	issues.Store
	usage github.Usage
}

func (c *statusFakeClient) Usage() github.Usage { return c.usage }

func TestStatusSnapshotServe(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "writes once per tick"
		if fail {
			name = "write failure does not stop ticks"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "status.json")
			if fail {
				path = filepath.Join(dir, "missing", "status.json")
			}
			t.Setenv("DRIVER_STATUS_FILE", path)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
			backend := &statusFakeBackend{tickets: []scheduler.Ticket{{Repo: "/repo", Identifier: "ZII-883", Owner: machine.OwnerExecutor, Phase: machine.PhaseGate}, {Repo: "/repo", Identifier: "ZII-999", Candidate: true}}}
			s := &scheduler.Scheduler{Backend: backend, Now: func() time.Time { return now }, Config: config.Config{MaxInFlight: 4, TokenBucket: config.TokenBucket{Capacity: 4, RefillPerSecond: 1}, Tuning: config.Tuning{Scheduler: config.DefaultScheduler()}}}
			var waits int
			err := serveWithStatus(ctx, filepath.Join(dir, "lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
				return s, &scheduler.Runtime{}, nil
			}, func(context.Context) error {
				waits++
				if !fail {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var snapshot status.Snapshot
					if err := json.Unmarshal(data, &snapshot); err != nil {
						t.Fatal(err)
					}
					expected := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
					if waits == 3 {
						expected = expected.Add(time.Minute)
					}
					if !snapshot.GeneratedAt.Equal(expected) {
						t.Fatalf("unexpected write at wait %d: %v", waits, snapshot.GeneratedAt)
					}
					if len(snapshot.Tickets) != 1 || snapshot.Tickets[0].Identifier != "ZII-883" || snapshot.Tickets[0].Owner != "执行器" || snapshot.Tickets[0].Phase != string(machine.PhaseGate) {
						t.Fatalf("tickets: %+v", snapshot.Tickets)
					}
				}
				now = now.Add(30 * time.Second)
				if waits == 3 {
					cancel()
				}
				return nil
			}, func(err error) { t.Errorf("inspection failed: %v", err) })
			if err != nil {
				t.Fatal(err)
			}
			if backend.snapshots != 2 || waits != 3 {
				t.Fatalf("snapshots=%d waits=%d", backend.snapshots, waits)
			}
		})
	}
}

func TestStatusSnapshotCollection(t *testing.T) {
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	a := &statusFakeClient{usage: github.Usage{Hour: now, Requests: 3, Limit: 100, Remaining: 97, Reset: now.Add(time.Hour)}}
	b := &statusFakeClient{usage: github.Usage{Hour: now, Requests: 5}}
	runtime := &scheduler.Runtime{Clients: map[string]issues.Store{"/b": b, "/a": a, "/shared": a}}
	backend := &statusBackend{Backend: &statusFakeBackend{}}
	s := &scheduler.Scheduler{}
	snapshot := collectStatus(s, backend, runtime, now)
	if snapshot.GitHub.Requests != 8 || !snapshot.GitHub.Hour.Equal(now) {
		t.Fatalf("usage: %+v", snapshot.GitHub)
	}
	if len(snapshot.GitHub.Limits) != 3 || snapshot.GitHub.Limits[0].Repo != "/a" || snapshot.GitHub.Limits[0].Requests.Remaining != 97 {
		t.Fatalf("limits: %+v", snapshot.GitHub.Limits)
	}
	files, found, err := backend.ActualChanges(context.Background(), scheduler.Ticket{})
	if err != nil || !found || !reflect.DeepEqual(files, []string{"actual.go"}) {
		t.Fatalf("optional backend method lost: %v %v %v", files, found, err)
	}
}

func TestStatusSnapshotFailedTick(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	backend := &statusFakeBackend{err: errors.New("ledger unavailable")}
	s := &scheduler.Scheduler{Backend: backend, Now: func() time.Time { return now }, Config: config.Config{MaxInFlight: 4, TokenBucket: config.TokenBucket{Capacity: 4, RefillPerSecond: 1}, Tuning: config.Tuning{Scheduler: config.DefaultScheduler()}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := serveWithStatus(ctx, filepath.Join(dir, "lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
		return s, &scheduler.Runtime{}, nil
	}, func(context.Context) error {
		data, err := os.ReadFile(status.Path())
		if err != nil {
			t.Fatal(err)
		}
		var snapshot status.Snapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			t.Fatal(err)
		}
		if !snapshot.GeneratedAt.Equal(now) || backend.snapshots != 1 {
			t.Fatalf("failed tick snapshot=%+v reads=%d", snapshot, backend.snapshots)
		}
		cancel()
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestStatusServePushesGitHubUsage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	client := &statusFakeClient{usage: github.Usage{Hour: now, Requests: 3, Limit: 100, Remaining: 97, Reset: now.Add(time.Hour)}}
	runtime := &scheduler.Runtime{Clients: map[string]issues.Store{"/repo": client}}
	s := &scheduler.Scheduler{Backend: &statusFakeBackend{}, Now: func() time.Time { return now }, Config: config.Config{MaxInFlight: 4, TokenBucket: config.TokenBucket{Capacity: 4, RefillPerSecond: 1}, Tuning: config.Tuning{Scheduler: config.DefaultScheduler()}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := serveWithStatus(ctx, filepath.Join(dir, "lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
		return s, runtime, nil
	}, func(context.Context) error {
		cancel()
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(status.PushPath())
	if err != nil {
		t.Fatal(err)
	}
	var view status.View
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	if view.GitHub.Requests != 3 || len(view.GitHub.Limits) != 1 || view.GitHub.Limits[0].Repo != "/repo" || view.GitHub.Limits[0].Requests.Remaining != 97 {
		t.Fatalf("pushed GitHub usage: %+v", view.GitHub)
	}
}

func TestStatusServeRecordsNextScan(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	s := &scheduler.Scheduler{Backend: &statusFakeBackend{}, Now: func() time.Time { return now }, Config: config.Config{MaxInFlight: 4, TokenBucket: config.TokenBucket{Capacity: 4, RefillPerSecond: 1}, Tuning: config.Tuning{Scheduler: config.DefaultScheduler()}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := serveWithStatus(ctx, filepath.Join(dir, "lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
		return s, &scheduler.Runtime{}, nil
	}, func(context.Context) error {
		cancel()
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	read := func(at time.Time) string {
		data, err := json.Marshal(status.Read(at, 0))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		return string(fields["nextScanAt"])
	}
	if got := read(now); got != `"2026-10-05T08:01:00Z"` {
		t.Fatalf("snapshot file nextScanAt = %s", got)
	}
	t.Setenv("DRIVER_STATUS_FILE", "")
	if got := read(now); got != `"2026-10-05T08:01:00Z"` {
		t.Fatalf("pushed nextScanAt = %s", got)
	}
	if got := read(now.Add(status.DefaultStaleAfter + time.Second)); got != "" {
		t.Fatalf("stale snapshot kept nextScanAt = %s", got)
	}
}

// serve wraps the Runtime for status snapshots; the wrapper must still expose
// heartbeat liveness, or a killed executor is never judged dead (OF-22).
func TestStatusBackendForwardsHeartbeatLiveness(t *testing.T) {
	root := t.TempDir()
	if err := orca.WriteBeat(orca.HeartbeatPath(root, "proj", "OF-1", "executor"), orca.Beat{PID: 7, At: time.Unix(0, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	runtime := &scheduler.Runtime{StateRoot: root, PIDAlive: func(int) bool { return false },
		Repositories: map[string]config.Repository{"/repo": {Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "o/proj"}}}}
	var backend scheduler.Backend = &statusBackend{Backend: runtime}
	liveness, ok := backend.(scheduler.LivenessBackend)
	if !ok {
		t.Fatal("status wrapper hides heartbeat liveness")
	}
	got, err := liveness.Liveness(context.Background(), scheduler.Ticket{Identifier: "OF-1", Repo: "/repo", Owner: machine.OwnerExecutor, Terminal: "-"})
	if err != nil || got != orca.LivenessDead {
		t.Fatalf("liveness=%q err=%v, want dead", got, err)
	}
}
