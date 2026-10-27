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
	GitHub      Usage     `json:"github,omitzero"`
}

type Deployment struct {
	QueueID  string `json:"queueId"`
	SHA      string `json:"sha"`
	Workflow string `json:"workflow,omitempty"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
}

type Ticket struct {
	Deployment *Deployment `json:"deployment,omitempty"`
	Repo       string      `json:"repo"`
	Identifier string      `json:"identifier"`
	Owner      string      `json:"owner"`
	Phase      string      `json:"phase"`
	Backend    string      `json:"backend,omitempty"`
	URL        string      `json:"url,omitempty"`
	Held       bool        `json:"held,omitempty"`
	// BlockedReason explains why a visible ticket cannot advance.
	BlockedReason string `json:"blockedReason,omitempty"`
	// Stage distinguishes candidate (待领) and human (转人) tickets from in-flight ones.
	Stage string `json:"stage,omitempty"`
	Title string `json:"title,omitempty"`
	// Since is when the push cache first saw this owner, phase and stage.
	Since time.Time `json:"since,omitzero"`
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
	defer func() { _ = os.Remove(file.Name()) }() // Cleanup only; successful rename removes this path.
	if snapshot.Tickets == nil {
		snapshot.Tickets = []Ticket{}
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

// DefaultStaleAfter tolerates ten missed 30-second serve snapshots.
const DefaultStaleAfter = 5 * time.Minute

// View.GeneratedAt records the last successful full scheduler observation.
// Partial writes retain it; zero means no scheduler observation is available.
type View struct {
	State       string    `json:"state"`
	GeneratedAt time.Time `json:"generatedAt"`
	Tickets     []Ticket  `json:"tickets"`
	GitHub      Usage     `json:"github,omitzero"`
}

// Read observes only the disposable snapshot. Missing or damaged caches are unknown.
// Read reports the snapshot, treating it as stale once it is older than staleAfter.
// staleAfter <= 0 uses DefaultStaleAfter.
func Read(now time.Time, staleAfter time.Duration) View {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	// The pushed view has exactly the public JSON shape. Legacy observation
	// caches remain readable for callers that explicitly select their old path.
	if os.Getenv("DRIVER_STATUS_FILE") == "" {
		if data, err := os.ReadFile(PushPath()); err == nil {
			var view View
			if json.Unmarshal(data, &view) == nil && (!view.GeneratedAt.IsZero() || view.State == "unknown") {
				normalize(&view, now)
				view.State = observationState(view.GeneratedAt, now, staleAfter)
				return view
			}
		}
	}
	view := View{State: "unknown", Tickets: []Ticket{}}
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
	view.GitHub = snapshot.GitHub
	normalize(&view, now)
	return view
}
