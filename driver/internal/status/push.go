package status

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// PushPath is a disposable machine-wide cache, never recovery input.
func PushPath() string {
	root := os.Getenv("XDG_RUNTIME_DIR")
	if root == "" {
		root = "/tmp"
	}
	return filepath.Join(root, "orca-factory", "status.json")
}

// Push serializes read-modify-replace across executor processes. Readers see a
// complete old or new JSON document; cache errors never escape into the driver.
// Partial writes do not renew the last full scheduler observation.
func Push(now time.Time, update func(*View)) {
	if err := push(now, update, false, time.Time{}); err != nil {
		log.Print("cannot push driver status snapshot")
	}
}

// PushObservation renews freshness after a successful full scheduler snapshot.
// Executor transitions, quota reads and pause updates must use Push instead.
func PushObservation(now, observedFrom time.Time, update func(*View)) {
	if err := push(now, update, true, observedFrom); err != nil {
		log.Print("cannot push driver status snapshot")
	}
}

func push(now time.Time, update func(*View), observation bool, observedFrom time.Time) error {
	path := PushPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	view := View{Tickets: []Ticket{}}
	if data, err := os.ReadFile(path); err == nil {
		var previous View
		if json.Unmarshal(data, &previous) == nil {
			view = previous
		}
	}
	observedAt := view.GeneratedAt
	before := append([]Ticket(nil), view.Tickets...)
	update(&view)
	carryTickets(before, view.Tickets, now, observation, observedFrom)
	if observation {
		observedAt = now
	}
	view.GeneratedAt = observedAt
	view.State = observationState(observedAt, now, DefaultStaleAfter)
	normalize(&view, now)
	sort.Slice(view.Tickets, func(i, j int) bool {
		a, b := view.Tickets[i], view.Tickets[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Identifier < b.Identifier
	})
	file, err := os.CreateTemp(filepath.Dir(path), ".status-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	writeErr := json.NewEncoder(file).Encode(view)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("cannot encode status")
	}
	return os.Rename(file.Name(), path)
}

// carryTickets keeps each row's title and phase start across rewrites, which
// rebuild rows from the ledger or from an executor that knows neither.
// A partial push stamps only the rows it wrote, so it cannot pin other rows
// against a newer ledger observation.
func carryTickets(before, after []Ticket, now time.Time, observation bool, observedFrom time.Time) {
	previous := make(map[[2]string]Ticket, len(before))
	for _, t := range before {
		previous[[2]string{t.Repo, t.Identifier}] = t
	}
	for i := range after {
		t := &after[i]
		old, ok := previous[[2]string{t.Repo, t.Identifier}]
		phaseChanged := old.Owner != t.Owner || old.Phase != t.Phase || old.Stage != t.Stage
		if observation && ok && old.PushedAt.After(observedFrom) && phaseChanged {
			t.Owner = old.Owner
			t.Phase = old.Phase
			t.Stage = old.Stage
			t.HumanKind = old.HumanKind
			t.HumanReason = old.HumanReason
			phaseChanged = false
		}
		if t.Title == "" {
			t.Title = old.Title
		}
		if ok && !old.Since.IsZero() && !phaseChanged {
			t.Since = old.Since
		} else {
			t.Since = now
		}
		if observation || t.PushedAt.IsZero() {
			t.PushedAt = now
		}
	}
}

func observationState(observedAt, now time.Time, staleAfter time.Duration) string {
	if observedAt.IsZero() || observedAt.After(now) {
		return "unknown"
	}
	if now.Sub(observedAt) > staleAfter {
		return "stale"
	}
	return "ok"
}

func normalize(view *View, now time.Time) {
	if view.Tickets == nil {
		view.Tickets = []Ticket{}
	}
	for i := range view.Tickets {
		ticket := &view.Tickets[i]
		if n := len(ticket.Stages); n > 0 && ticket.Stages[n-1].Name != ticket.Phase {
			ticket.Stages = nil
		}
	}
	hour := now.UTC().Truncate(time.Hour)
	if !view.GitHub.Hour.IsZero() || view.GitHub.Requests != 0 || view.GitHub.Limits != nil {
		if !view.GitHub.Hour.Equal(hour) {
			view.GitHub.Hour = hour
			view.GitHub.Requests = 0
		}
		if view.GitHub.Limits == nil {
			view.GitHub.Limits = []ClientLimits{}
		}
	}
}

// SetTicket preserves other repositories and executor observations. It clears
// PushedAt so Push stamps the written row even when copied from the cache.
func SetTicket(view *View, ticket Ticket, active bool) {
	for i, old := range view.Tickets {
		if old.Repo == ticket.Repo && old.Identifier == ticket.Identifier {
			view.Tickets = append(view.Tickets[:i], view.Tickets[i+1:]...)
			break
		}
	}
	if active {
		ticket.PushedAt = time.Time{}
		view.Tickets = append(view.Tickets, ticket)
	}
}
