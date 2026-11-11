package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// wakeMinInterval is the shortest gap a wake leaves after an inspection ends.
// serve's wait loop polls every 30 seconds, so a shorter gap starts no sooner.
const wakeMinInterval = 30 * time.Second

// wakeFile under the state root carries the newest wake request from any
// driver process to serve.
const wakeFile = "scheduler-wake.json"

type wakeRequest struct {
	Reason string    `json:"reason"`
	Ticket string    `json:"ticket"`
	At     time.Time `json:"at"`
}

// requestWake replaces any earlier request; the rename keeps serve from
// reading a partial file.
func requestWake(root string, request wakeRequest) error {
	if root == "" {
		return errors.New("state root unknown")
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(root, wakeFile+".*")
	if err != nil {
		return err
	}
	_, err = file.Write(raw)
	if err = errors.Join(err, file.Close()); err == nil {
		err = os.Rename(file.Name(), filepath.Join(root, wakeFile))
	}
	if err != nil {
		return errors.Join(err, os.Remove(file.Name()))
	}
	return nil
}

// wake brings the next inspection forward for a request made after the last
// one started, no sooner than wakeMinInterval after it ended. Outside running
// maintenance the timer alone decides, so a wake never adds one to a drain.
func (s *Scheduler) wake() {
	root := stateRoot()
	if s.NextTickAt.IsZero() || root == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(root, wakeFile))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var request wakeRequest
	if err == nil {
		err = json.Unmarshal(raw, &request)
	}
	if err != nil {
		s.logger().Warn("scheduler wake request unreadable", "err", err)
		return
	}
	if !request.At.After(s.tickStarted) {
		return
	}
	if state, err := maintenance.Read(); err != nil || state.Mode != "running" {
		return
	}
	at := s.tickEnded.Add(wakeMinInterval)
	if request.At.After(at) {
		at = request.At
	}
	if at.Before(s.NextTickAt) {
		s.NextTickAt, s.wakeReason = at, request.Reason
	}
}

// resolverWakeReason names the wake a resolver record asks for: a result the
// scheduler applies, or a waiting round due for its next dispatch.
func resolverWakeReason(p resolver.Record, now time.Time) string {
	switch {
	case p.Phase == "result":
		return "resolver-result"
	case p.Phase == "waiting" && !now.Before(p.RetryAt):
		return "resolver-redispatch"
	}
	return ""
}

// wakeFor asks serve to inspect early when p needs the scheduler now. A failed
// request leaves the next inspection to the timer.
func (r *Runtime) wakeFor(ctx context.Context, t Ticket, p resolver.Record) {
	now := r.clock()
	reason := resolverWakeReason(p, now)
	if reason == "" {
		return
	}
	if err := requestWake(r.StateRoot, wakeRequest{Reason: reason, Ticket: t.Identifier, At: now}); err != nil {
		runx.LoggerFrom(ctx).Warn("scheduler wake request failed", "ticket", t.Identifier, "err", err)
	}
}
