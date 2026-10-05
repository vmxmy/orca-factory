// Package pause tracks dependency outages and the union of their stopped clocks.
package pause

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

type Dependency string

const (
	Claude Dependency = "Claude"
	Codex  Dependency = "Codex"
	Linear Dependency = "Linear"
	GitHub Dependency = "GitHub"
	Orca   Dependency = "Orca"
)

type Kind string

const (
	Credentials Kind = "credentials"
	Quota       Kind = "quota"
)

// An empty Repo means a shared dependency. Kind separates quota and login
// failures so clearing one cannot accidentally clear the other.
type Key struct {
	Dependency Dependency
	Repo       string
	Kind       Kind
}
type Probe interface {
	Check(context.Context, Key) error
}

// Open must use Key as an idempotency key for ambiguous transport failures.
// Close must be idempotent; failures are retried on the next Poll.
type Alerts interface {
	Open(context.Context, Key, string) (string, error)
	Close(context.Context, string) error
}
type episode struct {
	key              Key
	start, end, next time.Time
	alert            string
}
type Manager struct {
	mu              sync.Mutex
	pollMu          sync.Mutex
	now             func() time.Time
	probe           Probe
	alerts          Alerts
	active          map[Key]*episode
	history         []*episode
	nextCredentials time.Time
}

func New(now func() time.Time, probe Probe, alerts Alerts) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{now: now, probe: probe, alerts: alerts, active: map[Key]*episode{}}
}
func (m *Manager) Pause(key Key, reset time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.active[key]; e != nil {
		if reset.After(e.next) {
			e.next = reset
		}
		return
	}
	now := m.now()
	next := now.Add(5 * time.Minute)
	if key.Kind == Quota && !reset.IsZero() {
		next = reset
	}
	e := &episode{key: key, start: now, next: next}
	m.active[key] = e
	m.history = append(m.history, e)
}
func (m *Manager) Resume(key Key) { m.mu.Lock(); defer m.mu.Unlock(); m.resume(key) }
func (m *Manager) resume(key Key) {
	if e := m.active[key]; e != nil {
		e.end = m.now()
		delete(m.active, key)
	}
}
func (m *Manager) Paused(repo string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.active {
		if k.Repo == "" || k.Repo == repo {
			return true
		}
	}
	return false
}

// PausedDuration clips and unions overlapping outages; dependency names may be
// supplied to restrict the query to those used by a particular ticket.
func (m *Manager) PausedDuration(repo string, from, to time.Time, deps ...Dependency) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !to.After(from) {
		return 0
	}
	type span struct{ start, end time.Time }
	var spans []span
	for _, e := range m.history {
		if e.key.Repo != "" && e.key.Repo != repo {
			continue
		}
		match := len(deps) == 0
		for _, d := range deps {
			if d == e.key.Dependency {
				match = true
			}
		}
		if !match {
			continue
		}
		start, end := e.start, e.end
		if start.Before(from) {
			start = from
		}
		if end.IsZero() || end.After(to) {
			end = to
		}
		if end.After(start) {
			spans = append(spans, span{start, end})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })
	var total time.Duration
	cursor := from
	for _, s := range spans {
		if s.start.Before(cursor) {
			s.start = cursor
		}
		if s.end.After(s.start) {
			total += s.end.Sub(s.start)
			cursor = s.end
		}
	}
	return total
}
func (m *Manager) EffectiveElapsed(repo string, since time.Time, deps ...Dependency) time.Duration {
	now := m.now()
	if !now.After(since) {
		return 0
	}
	return now.Sub(since) - m.PausedDuration(repo, since, now, deps...)
}

// Poll is safe to call more frequently than five minutes. All injected external
// operations get a timeout, and their potentially sensitive errors are sanitized.
func (m *Manager) Poll(ctx context.Context) error {
	m.pollMu.Lock()
	defer m.pollMu.Unlock()
	now := m.now()
	var problems []error
	m.mu.Lock()
	var due []Key
	if !now.Before(m.nextCredentials) && m.probe != nil {
		for _, d := range []Dependency{Claude, Codex, Linear, GitHub, Orca} {
			kind := Credentials
			if d == Orca {
				kind = ""
			}
			due = append(due, Key{Dependency: d, Kind: kind})
		}
		m.nextCredentials = now.Add(5 * time.Minute)
	}
	for k, e := range m.active {
		if !now.Before(e.next) {
			due = append(due, k)
		}
	}
	m.mu.Unlock()
	seen := map[Key]bool{}
	for _, k := range due {
		if seen[k] || m.probe == nil {
			continue
		}
		seen[k] = true
		m.mu.Lock()
		blocked := false
		for q, e := range m.active {
			if q.Dependency == k.Dependency && q.Kind == Quota && now.Before(e.next) {
				blocked = true
			}
		}
		m.mu.Unlock()
		if blocked {
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := m.probe.Check(bounded, k)
		cancel()
		if err != nil {
			m.Pause(k, time.Time{})
			m.mu.Lock()
			m.active[k].next = now.Add(5 * time.Minute)
			m.mu.Unlock()
			problems = append(problems, errors.New("dependency probe failed"))
		} else {
			m.Resume(k)
		}
	}
	if m.alerts != nil {
		m.mu.Lock()
		history := append([]*episode(nil), m.history...)
		m.mu.Unlock()
		for _, e := range history {
			m.mu.Lock()
			open := e.end.IsZero() && now.Sub(e.start) > 30*time.Minute && e.alert == ""
			id := e.alert
			closeAlert := !e.end.IsZero() && id != ""
			m.mu.Unlock()
			if open {
				bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
				created, err := m.alerts.Open(bounded, e.key, "流水线暂停："+string(e.key.Dependency))
				cancel()
				if err != nil || created == "" {
					problems = append(problems, errors.New("pause alert creation failed"))
				} else {
					m.mu.Lock()
					e.alert = created
					m.mu.Unlock()
				}
			}
			if closeAlert {
				bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := m.alerts.Close(bounded, id)
				cancel()
				if err != nil {
					problems = append(problems, errors.New("pause alert closure failed"))
				} else {
					m.mu.Lock()
					e.alert = ""
					m.mu.Unlock()
				}
			}
		}
	}
	return errors.Join(problems...)
}

// NextProbe lets a scheduler wake for recovery even while normal inspections
// back off. A zero time means there is no injected probe to schedule.
func (m *Manager) NextProbe() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.probe == nil {
		return time.Time{}
	}
	next := m.nextCredentials
	now := m.now()
	for _, e := range m.active {
		if e.next.After(now) && (next.IsZero() || e.next.Before(next)) {
			next = e.next
		}
	}
	return next
}

// Episode is a read-only observation of a currently active pause.
type Episode struct {
	Key   Key
	Start time.Time
}

// Active returns independent copies ordered by dependency, repository and kind.
func (m *Manager) Active() []Episode {
	m.mu.Lock()
	defer m.mu.Unlock()
	episodes := make([]Episode, 0, len(m.active))
	for key, e := range m.active {
		episodes = append(episodes, Episode{Key: key, Start: e.start})
	}
	sort.Slice(episodes, func(i, j int) bool {
		a, b := episodes[i].Key, episodes[j].Key
		if a.Dependency != b.Dependency {
			return a.Dependency < b.Dependency
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Kind < b.Kind
	})
	return episodes
}
