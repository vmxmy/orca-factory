package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestStatusFilePush(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	Push(now, func(v *View) { v.Tickets = []Ticket{{Identifier: "initial"}} })
	stop := make(chan struct{})
	readErr := make(chan error, 1)
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(PushPath())
			if err == nil {
				var v View
				err = json.Unmarshal(data, &v)
			}
			if err != nil {
				select {
				case readErr <- err:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		Push(now, func(v *View) {
			v.Tickets = make([]Ticket, 100)
			for j := range v.Tickets {
				v.Tickets[j] = Ticket{Repo: "repo", Identifier: "ZII-891", Owner: "执行器", Phase: "gate"}
			}
		})
	}
	close(stop)
	readers.Wait()
	select {
	case err := <-readErr:
		t.Fatal(err)
	default:
	}
	data, err := os.ReadFile(PushPath())
	if err != nil {
		t.Fatal(err)
	}
	var view View
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(view, Read(now, 0)) {
		t.Fatalf("file differs from status JSON: %+v", view)
	}
	if len(view.Tickets) != 100 {
		t.Fatalf("lost write: %+v", view)
	}
}

func TestPartialPushPreservesObservationFreshness(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	observed := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	initial := View{State: "ok", GeneratedAt: observed, Tickets: []Ticket{{Repo: "repo", Identifier: "old", Phase: "worker"}}}
	if err := os.MkdirAll(filepath.Dir(PushPath()), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PushPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	now := observed.Add(10 * time.Minute)
	Push(now, func(v *View) {
		SetTicket(v, Ticket{Repo: "repo", Identifier: "current", Phase: "gate"}, true)
	})
	view := Read(now, 0)
	if view.State != "stale" || !view.GeneratedAt.Equal(observed) {
		t.Fatalf("partial write renewed observation: %+v", view)
	}
	if len(view.Tickets) != 2 || view.Tickets[0].Identifier != "current" || view.Tickets[0].Phase != "gate" {
		t.Fatalf("partial write lost ticket updates: %+v", view)
	}
	data, err = os.ReadFile(PushPath())
	if err != nil {
		t.Fatal(err)
	}
	var pushed View
	if err := json.Unmarshal(data, &pushed); err != nil {
		t.Fatal(err)
	}
	if pushed.State != "stale" || !pushed.GeneratedAt.Equal(observed) {
		t.Fatalf("file reader would see renewed observation: %+v", pushed)
	}
}

func TestPartialPushWithoutObservationIsUnknown(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	// An older-format cache must not hide an explicitly unknown pushed view.
	if err := Write(Snapshot{GeneratedAt: now, Tickets: []Ticket{{Identifier: "legacy"}}}); err != nil {
		t.Fatal(err)
	}
	Push(now, func(v *View) {
		SetTicket(v, Ticket{Repo: "repo", Identifier: "executor", Phase: "gate"}, true)
	})
	view := Read(now, 0)
	if view.State != "unknown" || !view.GeneratedAt.IsZero() || len(view.Tickets) != 1 || view.Tickets[0].Identifier != "executor" {
		t.Fatalf("executor-only cache claims scheduler observation or loses ticket: %+v", view)
	}
}

func TestPushedObservationHonorsConfiguredStaleness(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	PushObservation(now, func(v *View) { v.Tickets = []Ticket{} })
	if got := Read(now.Add(2*time.Minute), time.Minute); got.State != "stale" {
		t.Fatalf("configured stale threshold ignored by pushed observation: %s", got.State)
	}
	if got := Read(now.Add(2*time.Minute), 3*time.Minute); got.State != "ok" {
		t.Fatalf("unexpired pushed observation rejected: %s", got.State)
	}
}

func TestPushCarriesTitleAndPhaseSince(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	row := func(phase, title string) Ticket {
		return Ticket{Repo: "/repo", Identifier: "OF-1", Owner: "执行器", Phase: phase, Title: title}
	}
	read := func() Ticket {
		t.Helper()
		var view View
		data, err := os.ReadFile(PushPath())
		if err != nil || json.Unmarshal(data, &view) != nil || len(view.Tickets) != 1 {
			t.Fatalf("status cache: %s %v", data, err)
		}
		return view.Tickets[0]
	}
	PushObservation(start, func(v *View) { v.Tickets = []Ticket{row("gate", "冒烟测试")} })
	if got := read(); !got.Since.Equal(start) || got.Title != "冒烟测试" {
		t.Fatalf("first observation: %+v", got)
	}
	PushObservation(start.Add(time.Minute), func(v *View) { v.Tickets = []Ticket{row("gate", "")} })
	if got := read(); !got.Since.Equal(start) || got.Title != "冒烟测试" {
		t.Fatalf("same phase after rebuild: %+v", got)
	}
	Push(start.Add(2*time.Minute), func(v *View) { SetTicket(v, row("审查", ""), true) })
	if got := read(); !got.Since.Equal(start.Add(2*time.Minute)) || got.Title != "冒烟测试" || got.Phase != "审查" {
		t.Fatalf("phase change: %+v", got)
	}
}
