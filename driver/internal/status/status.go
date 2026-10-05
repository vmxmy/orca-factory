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
