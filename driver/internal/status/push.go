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
func Push(now time.Time, update func(*View)) {
	if err := push(now, update); err != nil {
		log.Print("cannot push driver status snapshot")
	}
}

func push(now time.Time, update func(*View)) error {
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
	view := View{Tickets: []Ticket{}, Linear: Usage{Hour: now.UTC().Truncate(time.Hour), Limits: []ClientLimits{}}}
	if data, err := os.ReadFile(path); err == nil {
		var previous View
		if json.Unmarshal(data, &previous) == nil {
			view = previous
		}
	}
	update(&view)
	view.State, view.GeneratedAt = "ok", now
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

func normalize(view *View, now time.Time) {
	if view.Tickets == nil {
		view.Tickets = []Ticket{}
	}
	hour := now.UTC().Truncate(time.Hour)
	if !view.Linear.Hour.Equal(hour) {
		view.Linear.Hour = hour
		view.Linear.Requests = 0
		view.Linear.Complexity = 0
	}
	if view.Linear.Limits == nil {
		view.Linear.Limits = []ClientLimits{}
	}
}

// SetTicket preserves other repositories and executor observations.
func SetTicket(view *View, ticket Ticket, active bool) {
	for i, old := range view.Tickets {
		if old.Repo == ticket.Repo && old.Identifier == ticket.Identifier {
			view.Tickets = append(view.Tickets[:i], view.Tickets[i+1:]...)
			break
		}
	}
	if active {
		view.Tickets = append(view.Tickets, ticket)
	}
}
