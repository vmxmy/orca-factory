// Package status writes a disposable, credential-free observation cache.
package status

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type Snapshot struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Tickets     []Ticket  `json:"tickets"`
	Pauses      []Pause   `json:"pauses"`
	Linear      Usage     `json:"linear"`
}

type Ticket struct {
	Repo       string `json:"repo"`
	Identifier string `json:"identifier"`
	Owner      string `json:"owner"`
	Phase      string `json:"phase"`
}

type Pause struct {
	Dependency string    `json:"dependency"`
	Repo       string    `json:"repo"`
	Kind       string    `json:"kind"`
	Start      time.Time `json:"start"`
	// Duration is elapsed wall time in nanoseconds.
	Duration time.Duration `json:"duration"`
}

type Usage struct {
	Hour       time.Time `json:"hour"`
	Requests   int64     `json:"requests"`
	Complexity int64     `json:"complexity"`
	// Limits retain each repository's latest headers, not a sum of server quotas.
	Limits []ClientLimits `json:"limits"`
}

type ClientLimits struct {
	Repo       string    `json:"repo"`
	Requests   RateLimit `json:"requests"`
	Complexity RateLimit `json:"complexity"`
}

type RateLimit struct {
	Limit     int64     `json:"limit"`
	Remaining int64     `json:"remaining"`
	Reset     time.Time `json:"reset"`
}

func Path() string {
	if path := os.Getenv("DRIVER_STATUS_FILE"); path != "" {
		return path
	}
	return filepath.Join(os.TempDir(), "orca-factory-driver-status.json")
}

// Write replaces the cache atomically; failures leave the old cache intact.
// Errors deliberately exclude paths and serialized input.
func Write(snapshot Snapshot) error {
	path := Path()
	file, err := os.CreateTemp(filepath.Dir(path), ".driver-status-*")
	if err != nil {
		return errors.New("cannot create driver status snapshot")
	}
	defer os.Remove(file.Name())
	if snapshot.Tickets == nil {
		snapshot.Tickets = []Ticket{}
	}
	if snapshot.Pauses == nil {
		snapshot.Pauses = []Pause{}
	}
	if snapshot.Linear.Limits == nil {
		snapshot.Linear.Limits = []ClientLimits{}
	}
	writeErr := json.NewEncoder(file).Encode(snapshot)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("cannot write driver status snapshot")
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return errors.New("cannot replace driver status snapshot")
	}
	return nil
}

// staleAfter tolerates ten missed 30-second serve snapshots.
const staleAfter = 5 * time.Minute

type View struct {
	State       string      `json:"state"`
	GeneratedAt time.Time   `json:"generatedAt"`
	Tickets     []Ticket    `json:"tickets"`
	GlobalPause GlobalPause `json:"globalPause"`
	Linear      Usage       `json:"linear"`
}

type GlobalPause struct {
	Active          bool  `json:"active"`
	DurationSeconds int64 `json:"durationSeconds"`
}

// Read observes only the disposable snapshot. Missing or damaged caches are unknown.
func Read(now time.Time) View {
	hour := now.UTC().Truncate(time.Hour)
	view := View{State: "unknown", Tickets: []Ticket{}, Linear: Usage{Hour: hour, Limits: []ClientLimits{}}}
	data, err := os.ReadFile(Path())
	if err != nil {
		return view
	}
	var snapshot Snapshot
	if json.Unmarshal(data, &snapshot) != nil || snapshot.GeneratedAt.IsZero() {
		return view
	}
	view.State = "ok"
	if now.Sub(snapshot.GeneratedAt) > staleAfter {
		view.State = "stale"
	}
	view.GeneratedAt = snapshot.GeneratedAt
	if snapshot.Tickets != nil {
		view.Tickets = snapshot.Tickets
	}
	view.Linear = snapshot.Linear
	if !view.Linear.Hour.Equal(hour) {
		view.Linear.Hour = hour
		view.Linear.Requests = 0
		view.Linear.Complexity = 0
	}
	if view.Linear.Limits == nil {
		view.Linear.Limits = []ClientLimits{}
	}
	for _, pause := range snapshot.Pauses {
		if pause.Repo == "" {
			view.GlobalPause.Active = true
			if !pause.Start.IsZero() {
				view.GlobalPause.DurationSeconds = max(view.GlobalPause.DurationSeconds, int64(now.Sub(pause.Start)/time.Second))
			}
		}
	}
	return view
}
