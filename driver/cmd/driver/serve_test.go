package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/pause"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

type statusFakeBackend struct {
	scheduler.Backend
	snapshots int
	tickets   []scheduler.Ticket
	limits    []github.Usage
}

func (b *statusFakeBackend) Snapshot(context.Context, []config.Repository) ([]scheduler.Ticket, error) {
	b.snapshots++
	return b.tickets, nil
}
func (b *statusFakeBackend) GitHubLimits() []github.Usage { return b.limits }
func (b *statusFakeBackend) ActualChanges(context.Context, scheduler.Ticket) ([]string, bool, error) {
	return []string{"actual.go"}, true, nil
}

type statusFakeClient struct {
	scheduler.IssueClient
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
					if len(snapshot.Pauses) != 0 {
						t.Fatalf("pauses: %v", snapshot.Pauses)
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
	runtime := &scheduler.Runtime{Clients: map[string]scheduler.IssueClient{"/b": b, "/a": a, "/shared": a}}
	backend := &statusBackend{Backend: &statusFakeBackend{}}
	s := &scheduler.Scheduler{}
	snapshot := collectStatus(s, backend, runtime, now)
	if len(snapshot.Pauses) != 0 {
		t.Fatal(snapshot.Pauses)
	}
	if snapshot.GitHub.Requests != 8 || !snapshot.GitHub.Hour.Equal(now) {
		t.Fatalf("usage: %+v", snapshot.GitHub)
	}
	if len(snapshot.GitHub.Limits) != 3 || snapshot.GitHub.Limits[0].Repo != "/a" || snapshot.GitHub.Limits[0].Requests.Remaining != 97 {
		t.Fatalf("limits: %+v", snapshot.GitHub.Limits)
	}
	start := now.Add(-time.Minute)
	s.Pauses = pause.New(func() time.Time { return start }, nil, nil)
	keys := []pause.Key{{Dependency: pause.GitHub, Repo: "/b", Kind: pause.Quota}, {Dependency: pause.Codex, Kind: pause.Credentials}, {Dependency: pause.GitHub, Repo: "/a", Kind: pause.Quota}, {Dependency: pause.GitHub, Repo: "/a", Kind: pause.Credentials}}
	for _, key := range keys {
		s.Pauses.Pause(key, time.Time{})
	}
	active := s.Pauses.Active()
	wantKeys := []pause.Key{keys[1], keys[3], keys[2], keys[0]}
	for i, e := range active {
		if e.Key != wantKeys[i] || !e.Start.Equal(start) {
			t.Fatalf("active = %v", active)
		}
	}
	active[0].Key.Repo = "changed"
	snapshot = collectStatus(s, backend, runtime, now)
	if len(snapshot.Pauses) != 4 || snapshot.Pauses[0].Repo != "" || snapshot.Pauses[0].Duration != time.Minute || snapshot.Pauses[0].Dependency != "Codex" {
		t.Fatalf("pauses: %+v", snapshot.Pauses)
	}
	s.Pauses.Resume(keys[1])
	if len(s.Pauses.Active()) != 3 {
		t.Fatal("resumed episode retained")
	}
	files, found, err := backend.ActualChanges(context.Background(), scheduler.Ticket{})
	if err != nil || !found || !reflect.DeepEqual(files, []string{"actual.go"}) {
		t.Fatalf("optional backend method lost: %v %v %v", files, found, err)
	}
}

func TestStatusSnapshotPausedTick(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	backend := &statusFakeBackend{limits: []github.Usage{{Limit: 100, Remaining: 1, Reset: now.Add(time.Hour)}}}
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
		if len(snapshot.Pauses) != 1 || snapshot.Pauses[0].Dependency != "GitHub" || backend.snapshots != 0 {
			t.Fatalf("paused snapshot=%+v reads=%d", snapshot, backend.snapshots)
		}
		cancel()
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}
