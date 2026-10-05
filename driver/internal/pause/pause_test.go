package pause

import (
	"context"
	"errors"
	"testing"
	"time"
)

type probeFake struct {
	calls map[Key]int
	fail  map[Key]bool
}

func (p *probeFake) Check(_ context.Context, k Key) error {
	p.calls[k]++
	if p.fail[k] {
		return errors.New("secret must not escape")
	}
	return nil
}

type alertsFake struct {
	opened, closed int
	title          string
	closeErr       bool
}

func (a *alertsFake) Open(_ context.Context, _ Key, title string) (string, error) {
	a.opened++
	a.title = title
	return "alert-1", nil
}
func (a *alertsFake) Close(_ context.Context, _ string) error {
	a.closed++
	if a.closeErr {
		return errors.New("secret")
	}
	return nil
}
func TestPauseScopeAndElapsedUnion(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := now
	m := New(func() time.Time { return now }, nil, nil)
	local := Key{Dependency: GitHub, Repo: "a"}
	global := Key{Dependency: Claude}
	m.Pause(local, time.Time{})
	now = now.Add(10 * time.Minute)
	m.Pause(global, time.Time{})
	if !m.Paused("b") || !m.Paused("a") {
		t.Fatal("shared dependency must pause all repos")
	}
	now = now.Add(10 * time.Minute)
	m.Resume(local)
	now = now.Add(10 * time.Minute)
	m.Resume(global)
	if got := m.PausedDuration("a", start, now); got != 30*time.Minute {
		t.Fatalf("union=%v", got)
	}
	if got := m.EffectiveElapsed("b", start); got != 10*time.Minute {
		t.Fatalf("elapsed=%v", got)
	}
	if got := m.PausedDuration("a", start.Add(15*time.Minute), now.Add(-5*time.Minute)); got != 10*time.Minute {
		t.Fatalf("clipped=%v", got)
	}
	m.Pause(local, time.Time{})
	if m.Paused("b") || !m.Paused("a") {
		t.Fatal("local pause leaked")
	}
}
func TestPauseProbeCredentialsAndQuotaReset(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, dep := range []Dependency{Claude, Codex, Linear, GitHub} {
		t.Run(string(dep), func(t *testing.T) {
			p := &probeFake{calls: map[Key]int{}, fail: map[Key]bool{}}
			k := Key{Dependency: dep, Kind: Credentials}
			p.fail[k] = true
			m := New(func() time.Time { return now }, p, nil)
			if err := m.Poll(context.Background()); err == nil {
				t.Fatal("credential failure not reported")
			}
			if !m.Paused("any") {
				t.Fatal("credential failure did not stop dispatch")
			}
			now = now.Add(4 * time.Minute)
			_ = m.Poll(context.Background())
			if p.calls[k] != 1 {
				t.Fatal("probe too soon")
			}
			now = now.Add(time.Minute)
			p.fail[k] = false
			if err := m.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if m.Paused("any") {
				t.Fatal("recovery not observed")
			}
		})
	}
	p := &probeFake{calls: map[Key]int{}, fail: map[Key]bool{}}
	m := New(func() time.Time { return now }, p, nil)
	k := Key{Dependency: Linear, Kind: Quota}
	reset := now.Add(time.Hour)
	m.Pause(k, reset)
	now = now.Add(5 * time.Minute)
	_ = m.Poll(context.Background())
	if p.calls[k] != 0 || !m.Paused("a") {
		t.Fatal("quota probed before reset")
	}
	now = reset
	if err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls[k] != 1 || m.Paused("a") {
		t.Fatal("quota reset not probed")
	}
}
func TestPauseAlertLifecycle(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := &alertsFake{}
	m := New(func() time.Time { return now }, nil, a)
	k := Key{Dependency: Orca}
	m.Pause(k, time.Time{})
	now = now.Add(30 * time.Minute)
	_ = m.Poll(context.Background())
	if a.opened != 0 {
		t.Fatal("alert at threshold")
	}
	now = now.Add(time.Second)
	_ = m.Poll(context.Background())
	_ = m.Poll(context.Background())
	if a.opened != 1 || a.title != "流水线暂停：Orca" {
		t.Fatalf("alert=%+v", a)
	}
	m.Resume(k)
	a.closeErr = true
	if err := m.Poll(context.Background()); err == nil {
		t.Fatal("close failure not returned")
	}
	a.closeErr = false
	_ = m.Poll(context.Background())
	_ = m.Poll(context.Background())
	if a.closed != 2 {
		t.Fatalf("close retries=%d", a.closed)
	}
}

func TestPauseNextProbeHonorsQuotaReset(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := &probeFake{calls: map[Key]int{}, fail: map[Key]bool{}}
	m := New(func() time.Time { return now }, p, nil)
	_ = m.Poll(context.Background())
	if got := m.NextProbe(); !got.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("next=%v", got)
	}
	reset := now.Add(2 * time.Minute)
	m.Pause(Key{Dependency: Linear, Kind: Quota}, reset)
	if got := m.NextProbe(); !got.Equal(reset) {
		t.Fatalf("reset probe delayed: %v", got)
	}
}
