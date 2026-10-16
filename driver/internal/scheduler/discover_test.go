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

// TestRefreshProjectsHotReloadsConfigButFreezesLedgerIdentity is OF-45's
// acceptance test: a non-identity change (MaxInFlight) takes effect on the
// next discovery, while a ledger identity change (AGENT_FEISHU_BASE) freezes
// the repository on its old configuration and logs its warning only once.
func TestRefreshProjectsHotReloadsConfigButFreezesLedgerIdentity(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	result := projects.Result{Eligible: []projects.Project{{Path: "/a", Repository: config.Repository{
		Path: "/a", MaxInFlight: 1, StaleHours: 8,
		Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_BASE": "https://base1.feishu.cn/base/tok1"},
	}}}}
	f := &discoveryBackend{fakeBackend: fakeBackend{absent: true, inactive: true}}
	var logs bytes.Buffer
	s := discoveryScheduler(f, &logs, &now, func(context.Context) (projects.Result, error) { return result, nil })
	if err := s.RefreshProjects(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Second discovery raises MaxInFlight to 2 and also changes the ledger
	// identity key AGENT_FEISHU_BASE; the identity change must win and keep
	// the repository on its old configuration entirely.
	now = now.Add(time.Minute)
	result.Eligible[0].Repository = config.Repository{
		Path: "/a", MaxInFlight: 2, StaleHours: 8,
		Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_BASE": "https://base2.feishu.cn/base/tok2"},
	}
	if err := s.RefreshProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.Repositories[0].MaxInFlight; got != 1 {
		t.Fatalf("MaxInFlight=%d, want frozen at 1 alongside the changed ledger identity", got)
	}
	if got := s.Repositories[0].Env["AGENT_FEISHU_BASE"]; got != "https://base1.feishu.cn/base/tok1" {
		t.Fatalf("AGENT_FEISHU_BASE=%q, want frozen to the old value pending restart", got)
	}
	if n := strings.Count(logs.String(), "ledger identity changed"); n != 1 {
		t.Fatalf("identity-drift warning logged %d times, want once:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "base1.feishu.cn") || strings.Contains(logs.String(), "base2.feishu.cn") {
		t.Fatalf("log leaked an Env value, want key names only:\n%s", logs.String())
	}

	// Rediscovering the same drift again must not log a second warning.
	now = now.Add(time.Minute)
	if err := s.RefreshProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "ledger identity changed"); n != 1 {
		t.Fatalf("identity-drift warning re-logged, want once total:\n%s", logs.String())
	}

	// A third discovery drops the identity drift (AGENT_FEISHU_BASE reverts)
	// but keeps MaxInFlight at 2; the hot-reloadable change now applies.
	now = now.Add(time.Minute)
	result.Eligible[0].Repository = config.Repository{
		Path: "/a", MaxInFlight: 2, StaleHours: 8,
		Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_BASE": "https://base1.feishu.cn/base/tok1"},
	}
	if err := s.RefreshProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.Repositories[0].MaxInFlight; got != 2 {
		t.Fatalf("MaxInFlight=%d, want hot-reloaded to 2 once the ledger identity matches again", got)
	}

	// The next tick claims up to the hot-reloaded MaxInFlight of 2.
	now = now.Add(time.Minute)
	f.tickets = []Ticket{
		{ID: "a-1", Repo: "/a", Candidate: true, CreatedAt: now},
		{ID: "a-2", Repo: "/a", Candidate: true, CreatedAt: now.Add(time.Second)},
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := launchedIDs(f); !reflect.DeepEqual(got, []string{"a-1", "a-2"}) {
		t.Fatalf("launches=%v, want both tickets claimed under the hot-reloaded limit of 2", got)
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

// TestRefreshProjectsCoreBehindTakenOverAndSynced is OF-53: an eligible
// project whose core copy lags stays taken over, its lag is warned once per
// distinct file list, and every discovery hands it to SyncCore (whose own
// remote checks keep one PR per factory version); a project in step is not.
func TestRefreshProjectsCoreBehindTakenOverAndSynced(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	result := eligible("/a", "/b")
	result.Eligible[0].CoreBehind = []string{"install.sh"}
	f := &discoveryBackend{fakeBackend: fakeBackend{absent: true, inactive: true}}
	var logs bytes.Buffer
	s := discoveryScheduler(f, &logs, &now, func(context.Context) (projects.Result, error) { return result, nil })
	var synced []string
	s.SyncCore = func(_ context.Context, p projects.Project) error {
		synced = append(synced, p.Path+":"+strings.Join(p.CoreBehind, ","))
		return nil
	}
	for range 2 {
		if err := s.RefreshProjects(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if paths := []string{s.Repositories[0].Path, s.Repositories[1].Path}; !reflect.DeepEqual(paths, []string{"/a", "/b"}) || len(s.retired) != 0 {
		t.Fatalf("repositories=%v retired=%v, want both taken over", paths, s.retired)
	}
	if !reflect.DeepEqual(synced, []string{"/a:install.sh", "/a:install.sh"}) {
		t.Fatalf("synced=%v", synced)
	}
	if n := strings.Count(logs.String(), `msg="project core behind" path=/a files=[install.sh]`); n != 1 {
		t.Fatalf("core behind warned %d times, want once:\n%s", n, logs.String())
	}
}

// TestStartupRetiresUntakeableProjectWithInFlightTickets is OF-98: a
// repository that is already not takeable when serve starts, but whose ledger
// still holds driver-owned in-flight tickets, is tracked as retired so those
// tickets keep advancing while its new tickets stay unclaimed; a not-takeable
// repository with nothing in flight is not tracked at all.
func TestStartupRetiresUntakeableProjectWithInFlightTickets(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	result := eligible("/a")
	result.Rejected = []projects.Rejection{
		{Path: "/z", Reason: projects.ReasonCheckFailed, Repository: &config.Repository{Path: "/z", MaxInFlight: 3, StaleHours: 8}},
		{Path: "/y", Reason: projects.ReasonCheckFailed, Repository: &config.Repository{Path: "/y", MaxInFlight: 3, StaleHours: 8}},
		{Path: "/x", Reason: projects.ReasonLedgerNotFeishu},
	}
	f := &discoveryBackend{fakeBackend: fakeBackend{absent: true, inactive: true}}
	f.tickets = []Ticket{
		{ID: "z-running", Identifier: "ZCP-1", Repo: "/z", Owner: machine.OwnerExecutor, Phase: machine.PhaseWorker, Attempt: 1, Terminal: "term", UpdatedAt: now},
		{ID: "z-merging", Identifier: "ZCP-2", Repo: "/z", Owner: machine.OwnerMerge, UpdatedAt: now},
		{ID: "z-new", Identifier: "ZCP-3", Repo: "/z", Candidate: true, CreatedAt: now},
		{ID: "y-new", Identifier: "YYY-1", Repo: "/y", Candidate: true, CreatedAt: now},
		{ID: "a-new", Identifier: "AAA-1", Repo: "/a", Candidate: true, CreatedAt: now},
	}
	var logs bytes.Buffer
	s := discoveryScheduler(f, &logs, &now, func(context.Context) (projects.Result, error) { return result, nil })
	if err := s.RefreshProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range s.Repositories {
		paths = append(paths, r.Path)
	}
	if !reflect.DeepEqual(paths, []string{"/a", "/z"}) || !reflect.DeepEqual(s.retiredPaths(), []string{"/z"}) {
		t.Fatalf("repositories=%v retired=%v, want /z retired and /y, /x untracked", paths, s.retiredPaths())
	}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := launchedIDs(f); !reflect.DeepEqual(got, []string{"z-running", "a-new"}) {
		t.Fatalf("launches=%v, want the retired in-flight ticket resumed and only the eligible candidate claimed", got)
	}
	if len(s.MergeQueue) != 1 || s.MergeQueue[0].ID != "z-merging" {
		t.Fatalf("merge queue=%v, want the retired project's merge to keep advancing", s.MergeQueue)
	}

	now = now.Add(time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := `msg="project not taken over but has in-flight tickets" path=/z reason="check 失败" tickets="[ZCP-1 ZCP-2]"`
	if n := strings.Count(logs.String(), want); n != 1 {
		t.Fatalf("in-flight warning logged %d times, want once:\n%s", n, logs.String())
	}
}
