package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
)

// discoveryBackend snapshots only the repositories the scheduler passes, so a
// project the scheduler stops reading also stops yielding tickets.
type discoveryBackend struct {
	fakeBackend
	tracked []string
}

func (f *discoveryBackend) Snapshot(_ context.Context, repos []config.Repository) ([]Ticket, error) {
	f.snapshots++
	var rows []Ticket
	for _, t := range f.tickets {
		if slices.ContainsFunc(repos, func(r config.Repository) bool { return r.Path == t.Repo }) {
			rows = append(rows, t)
		}
	}
	return rows, nil
}

func (f *discoveryBackend) Track(_ context.Context, repo config.Repository) error {
	f.tracked = append(f.tracked, repo.Path)
	return nil
}

func eligible(paths ...string) projects.Result {
	var result projects.Result
	for _, path := range paths {
		result.Eligible = append(result.Eligible, projects.Project{Path: path, Repository: config.Repository{Path: path, MaxInFlight: 3, StaleHours: 8}})
	}
	return result
}

func discoveryScheduler(f Backend, logs *bytes.Buffer, now *time.Time, discover func(context.Context) (projects.Result, error)) *Scheduler {
	return &Scheduler{
		Config:        config.Config{MaxInFlight: 10, TokenBucket: config.TokenBucket{Capacity: 10, RefillPerSecond: 1}, Tuning: config.Tuning{Scheduler: config.DefaultScheduler()}},
		Backend:       f,
		Logger:        slog.New(slog.NewTextHandler(logs, nil)),
		Now:           func() time.Time { return *now },
		Discover:      discover,
		DiscoverEvery: time.Minute,
	}
}

func launchedIDs(f *discoveryBackend) []string {
	ids := []string{}
	for _, t := range f.launched {
		ids = append(ids, t.ID)
	}
	return ids
}

func TestDiscoverRetiredProjectFinishesInFlightButClaimsNothingNew(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	result := eligible("/a", "/b", "/c")
	f := &discoveryBackend{fakeBackend: fakeBackend{absent: true, inactive: true}}
	var logs bytes.Buffer
	s := discoveryScheduler(f, &logs, &now, func(context.Context) (projects.Result, error) { return result, nil })
	if err := s.RefreshProjects(context.Background()); err != nil {
		t.Fatal(err)
	}

	result = eligible("/a", "/b")
	result.Rejected = []projects.Rejection{{Path: "/c", Reason: projects.ReasonProjectDisabled}}
	now = now.Add(time.Minute)
	f.tickets = []Ticket{
		{ID: "c-running", Repo: "/c", Owner: machine.OwnerExecutor, Phase: machine.PhaseWorker, Attempt: 1, Terminal: "term", UpdatedAt: now},
		{ID: "c-merging", Repo: "/c", Owner: machine.OwnerMerge, UpdatedAt: now},
		{ID: "c-new", Repo: "/c", Candidate: true, CreatedAt: now},
		{ID: "a-new", Repo: "/a", Candidate: true, CreatedAt: now},
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := launchedIDs(f); !reflect.DeepEqual(got, []string{"c-running", "a-new"}) {
		t.Fatalf("launches=%v, want retired in-flight resumed and only the eligible candidate claimed", got)
	}
	if len(s.MergeQueue) != 1 || s.MergeQueue[0].ID != "c-merging" {
		t.Fatalf("merge queue=%v, want the retired project's merge to keep advancing", s.MergeQueue)
	}

	result.Eligible = eligible("/a", "/b", "/d").Eligible
	now = now.Add(time.Minute)
	f.launched = nil
	f.tickets = append(f.tickets, Ticket{ID: "d-new", Repo: "/d", Candidate: true, CreatedAt: now})
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := launchedIDs(f); !slices.Contains(got, "d-new") || slices.Contains(got, "c-new") {
		t.Fatalf("launches=%v, want the added project claimed and the retired one still skipped", got)
	}
	if !reflect.DeepEqual(f.tracked, []string{"/a", "/b", "/c", "/d"}) {
		t.Fatalf("tracked=%v", f.tracked)
	}
	if n := strings.Count(logs.String(), `path=/c reason=项目未启用`); n != 1 {
		t.Fatalf("retired warning logged %d times, want once for an unchanged rejection:\n%s", n, logs.String())
	}
}

func TestDiscoverFailureKeepsPreviousProjects(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	fail := false
	f := &discoveryBackend{}
	var logs bytes.Buffer
	s := discoveryScheduler(f, &logs, &now, func(context.Context) (projects.Result, error) {
		if fail {
			return projects.Result{}, errors.New("orca offline")
		}
		return eligible("/a", "/b"), nil
	})
	if err := s.RefreshProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail = true
	now = now.Add(time.Minute)
	if err := s.RefreshProjects(context.Background()); err == nil {
		t.Fatal("discovery failure was not reported")
	}
	f.tickets = []Ticket{{ID: "a-new", Repo: "/a", Candidate: true, CreatedAt: now}, {ID: "b-new", Repo: "/b", Candidate: true, CreatedAt: now}}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := launchedIDs(f); !reflect.DeepEqual(got, []string{"a-new", "b-new"}) {
		t.Fatalf("launches=%v, want both previously discovered projects still claimable", got)
	}
}
