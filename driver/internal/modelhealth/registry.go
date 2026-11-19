package modelhealth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/flock"
)

type Registry struct {
	mu       sync.Mutex
	path     string
	statuses map[string]Status
}

func Open(stateDir string) (*Registry, error) {
	r := &Registry{path: filepath.Join(stateDir, "model-health.json")}
	statuses, err := r.read()
	if err != nil {
		return nil, err
	}
	r.statuses = statuses
	return r, nil
}

func (r *Registry) read() (map[string]Status, error) {
	statuses := map[string]Status{}
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return statuses, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &statuses); err != nil {
		return nil, err
	}
	if statuses == nil {
		return nil, errors.New("model health registry must be an object")
	}
	for key, s := range statuses {
		if key != s.Ref.Key() || (s.State != OK && s.State != Unknown && s.State != Unavailable) {
			return nil, errors.New("invalid model health status")
		}
	}
	return statuses, nil
}

// Record reloads under a file lock so independent doctor runs retain each other's models.
func (r *Registry) Record(status Status) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, err := flock.TryLock(r.path + ".lock")
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	statuses, err := r.read()
	if err != nil {
		return err
	}
	key := status.Ref.Key()
	if statuses[key].CheckedAt.After(status.CheckedAt) {
		r.statuses = statuses
		return nil
	}
	statuses[key] = status
	data, err := json.MarshalIndent(statuses, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(r.path), ".model-health-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), r.path); err != nil {
		return err
	}
	r.statuses = statuses
	return nil
}

// Observe applies a probe to this in-memory snapshot for a read-only check.
func (r *Registry) Observe(status Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := status.Ref.Key()
	if !r.statuses[key].CheckedAt.After(status.CheckedAt) {
		r.statuses[key] = status
	}
}

func (r *Registry) Status(ref Ref, now time.Time) Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.statuses[ref.Key()]
	if !ok {
		return Status{Ref: ref, State: Unknown, Reason: "not_checked"}
	}
	s.Ref = ref
	if s.State == Unavailable && !now.Before(s.Until) {
		s.State, s.Reason = Unknown, "cooldown_expired"
	}
	return s
}

// Select preserves candidate order. Unknown and expired models remain eligible for a retry.
func (r *Registry) Select(candidates []Ref, now time.Time) (Ref, bool) {
	for _, ref := range candidates {
		if r.Status(ref, now).State != Unavailable {
			return ref, true
		}
	}
	return Ref{}, false
}
