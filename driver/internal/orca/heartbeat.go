package orca

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	// HeartbeatInterval is how often a running `driver exec` or `driver resolve` rewrites its heartbeat.
	HeartbeatInterval = 30 * time.Second
	// DefaultHeartbeatTimeout is how stale a heartbeat may be before the scheduler stops counting it as alive.
	DefaultHeartbeatTimeout = 5 * time.Minute
)

// Beat is the process-level liveness record of one executor or resolver.
type Beat struct {
	PID      int       `json:"pid"`
	Run      string    `json:"run,omitempty"`
	Terminal string    `json:"terminal,omitempty"`
	At       time.Time `json:"at"`
}

// Liveness is the scheduler's verdict on a heartbeat.
type Liveness string

const (
	// LivenessNone means the process wrote no heartbeat; other evidence decides.
	LivenessNone      Liveness = "none"
	LivenessAlive     Liveness = "alive"
	LivenessDead      Liveness = "dead"
	LivenessUncertain Liveness = "uncertain"
)

// HeartbeatPath is the heartbeat file of unit ("executor" or "resolver") for a ticket.
func HeartbeatPath(root, repository, ticket, unit string) string {
	return filepath.Join(root, repository, ticket, unit+".heartbeat.json")
}

// HeartbeatTimeout reads AGENT_HEARTBEAT_TIMEOUT_MINUTES; absent or invalid values use the default.
func HeartbeatTimeout() time.Duration {
	minutes, err := strconv.ParseInt(os.Getenv("AGENT_HEARTBEAT_TIMEOUT_MINUTES"), 10, 64)
	if err != nil || minutes <= 0 || minutes > int64((1<<63-1)/time.Minute) {
		return DefaultHeartbeatTimeout
	}
	return time.Duration(minutes) * time.Minute
}

// WriteBeat replaces the heartbeat file atomically.
func WriteBeat(path string, b Beat) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("cannot create heartbeat directory")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return errors.New("cannot encode heartbeat")
	}
	if err = os.WriteFile(path+".new", raw, 0600); err != nil {
		return errors.New("cannot write heartbeat")
	}
	if err = os.Rename(path+".new", path); err != nil {
		return errors.New("cannot commit heartbeat")
	}
	return nil
}

// ReadBeat reports found=false when no heartbeat file exists.
func ReadBeat(path string) (b Beat, found bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Beat{}, false, nil
	}
	if err != nil {
		return Beat{}, false, errors.New("cannot read heartbeat")
	}
	if json.Unmarshal(raw, &b) != nil || b.PID <= 0 || b.At.IsZero() {
		return Beat{}, false, errors.New("invalid heartbeat")
	}
	return b, true, nil
}

// PIDAlive reports whether a process with pid exists. EPERM still proves existence.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Judge combines heartbeat age and pid existence. A fresh beat with a missing
// pid, or a stale beat with a live pid, is contradictory evidence: uncertain.
func (b Beat) Judge(now time.Time, timeout time.Duration, alive func(int) bool) Liveness {
	fresh := now.Sub(b.At) <= timeout
	exists := alive(b.PID)
	switch {
	case fresh && exists:
		return LivenessAlive
	case !fresh && !exists:
		return LivenessDead
	default:
		return LivenessUncertain
	}
}

// Heartbeat periodically rewrites a heartbeat file for the current process.
type Heartbeat struct {
	path     string
	terminal string
	now      func() time.Time
	mu       sync.Mutex
	run      string
}

// StartHeartbeat writes the first beat before returning, then keeps beating
// until ctx ends. A nil Heartbeat is a valid no-op receiver.
func StartHeartbeat(ctx context.Context, path, terminal string, interval time.Duration, now func() time.Time) (*Heartbeat, error) {
	if now == nil {
		now = time.Now
	}
	h := &Heartbeat{path: path, terminal: terminal, now: now}
	if err := h.beat(); err != nil {
		return nil, err
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = h.beat()
			}
		}
	}()
	return h, nil
}

// SetRun records the Orca Run in the next beat and writes one immediately.
func (h *Heartbeat) SetRun(run string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.run = run
	h.mu.Unlock()
	_ = h.beat()
}

func (h *Heartbeat) beat() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return WriteBeat(h.path, Beat{PID: os.Getpid(), Run: h.run, Terminal: h.terminal, At: h.now().UTC()})
}
