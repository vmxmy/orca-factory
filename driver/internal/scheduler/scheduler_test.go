package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

type fakeBackend struct {
	tickets                  []Ticket
	snapshots                int
	enabled                  map[string]bool
	absent, inactive         bool
	terminalErr, dispatchErr error
	applyErr                 error
	actions                  []Action
	launched                 []Ticket
	legacy                   map[string]Legacy
}

func (f *fakeBackend) Snapshot(context.Context, []config.Repository) ([]Ticket, error) {
	f.snapshots++
	return append([]Ticket(nil), f.tickets...), nil
}
func (f *fakeBackend) AutomationDisabled(_ context.Context, r config.Repository) (bool, error) {
	return !f.enabled[r.Path], nil
}
func (f *fakeBackend) TerminalAbsent(context.Context, Ticket) (bool, error) {
	return f.absent, f.terminalErr
}
func (f *fakeBackend) NoActiveDispatch(context.Context, Ticket) (bool, error) {
	return f.inactive, f.dispatchErr
}
func (f *fakeBackend) InspectLegacy(_ context.Context, t Ticket) (Legacy, error) {
	return f.legacy[t.ID], nil
}
func (f *fakeBackend) Apply(_ context.Context, _ Ticket, a Action) error {
	f.actions = append(f.actions, a)
	return f.applyErr
}
func (f *fakeBackend) Launch(_ context.Context, t Ticket) error {
	f.launched = append(f.launched, t)
	return nil
}
func setup(f *fakeBackend) *Scheduler {
	return &Scheduler{Config: config.Config{MaxInFlight: 3, TokenBucket: config.TokenBucket{Capacity: 10, RefillPerSecond: 1}, Repositories: []config.Repository{{Path: "a", MaxInFlight: 1, StaleHours: 8}, {Path: "b", MaxInFlight: 3, StaleHours: 8}}}, Backend: f, Now: func() time.Time { return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) }}
}
func TestSecondInstanceCannotLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Acquire(path)
	if err == nil {
		second.Close()
		t.Fatal("second instance acquired lock")
	}
}
func TestCrossRepositoryOrderAndLimits(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	f := &fakeBackend{tickets: []Ticket{
		{ID: "existing", Repo: "b", Owner: machine.OwnerMerge, UpdatedAt: now},
		{ID: "a-new", Repo: "a", Candidate: true, Priority: 1, CreatedAt: now},
		{ID: "a-old", Repo: "a", Candidate: true, Priority: 1, CreatedAt: now.Add(-time.Hour)},
		{ID: "b-low", Repo: "b", Candidate: true, Priority: 3, CreatedAt: now},
		{ID: "b-high", Repo: "b", Candidate: true, Priority: 2, CreatedAt: now},
	}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, v := range f.launched {
		ids = append(ids, v.ID)
	}
	if !reflect.DeepEqual(ids, []string{"a-old", "b-high"}) {
		t.Fatalf("launches=%v", ids)
	}
	if f.snapshots != 1 || len(s.MergeQueue) != 1 {
		t.Fatalf("snapshot=%d merge=%v", f.snapshots, s.MergeQueue)
	}
}
func TestExecutorDeathRequiresPositiveEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		absent, inactive         bool
		terminalErr, dispatchErr error
		want                     int
	}{
		{"dead", true, true, nil, nil, 1}, {"terminal present", false, true, nil, nil, 0}, {"active dispatch", true, false, nil, nil, 0},
		{"terminal read failed", true, true, errors.New("offline"), nil, 0}, {"dispatch read failed", true, true, nil, errors.New("offline"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBackend{absent: tc.absent, inactive: tc.inactive, terminalErr: tc.terminalErr, dispatchErr: tc.dispatchErr}
			s := setup(f)
			f.tickets = []Ticket{{ID: "ticket", Repo: "a", Owner: machine.OwnerExecutor, Attempt: 1, UpdatedAt: s.Now()}}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.launched) != tc.want {
				t.Fatalf("launches=%v", f.launched)
			}
			if tc.want == 1 && f.launched[0].Attempt != 2 {
				t.Fatal("attempt did not advance")
			}
		})
	}
}
func TestSecondExecutorFailureEscalates(t *testing.T) {
	f := &fakeBackend{absent: true, inactive: true}
	s := setup(f)
	f.tickets = []Ticket{{ID: "ticket", Repo: "a", Owner: machine.OwnerExecutor, Attempt: 2, UpdatedAt: s.Now()}}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 0 || len(f.actions) != 1 || f.actions[0].Kind != Human {
		t.Fatalf("actions=%v", f.actions)
	}
}
func TestEnabledAutomationRefusesRepositoryEveryTick(t *testing.T) {
	f := &fakeBackend{enabled: map[string]bool{"a": true}, tickets: []Ticket{{ID: "ticket", Repo: "a", Candidate: true}}}
	s := setup(f)
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("accepted enabled automation")
	}
	if len(f.actions) != 0 || len(f.launched) != 0 {
		t.Fatal("mutated refused repository")
	}
	f.enabled["a"] = false
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.enabled["a"] = true
	f.launched = nil
	f.actions = nil
	if err := s.Tick(context.Background()); err == nil || len(f.launched) != 0 {
		t.Fatal("did not recheck automation")
	}
}
func TestLegacyTakeover(t *testing.T) {
	for _, tc := range []struct {
		name          string
		legacy        Legacy
		kind          ActionKind
		launch, merge int
	}{
		{"PR", Legacy{HasPR: true}, Merge, 0, 1}, {"complete branch", Legacy{Complete: true}, Resume, 1, 0}, {"release", Legacy{}, Release, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBackend{legacy: map[string]Legacy{"ticket": tc.legacy}}
			s := setup(f)
			f.tickets = []Ticket{{ID: "ticket", Repo: "a", Legacy: true, UpdatedAt: s.Now()}}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.actions) != 1 || f.actions[0].Kind != tc.kind || len(f.launched) != tc.launch || len(s.MergeQueue) != tc.merge {
				t.Fatalf("actions=%v launch=%v merge=%v", f.actions, f.launched, s.MergeQueue)
			}
			if tc.launch == 1 && f.launched[0].Phase != machine.PhaseGate {
				t.Fatal("legacy branch did not request gate resume")
			}
		})
	}
}
func TestStaleInflightEscalatesWithoutRetry(t *testing.T) {
	f := &fakeBackend{absent: true, inactive: true}
	s := setup(f)
	f.tickets = []Ticket{{ID: "ticket", Repo: "a", Owner: machine.OwnerExecutor, Attempt: 1, UpdatedAt: s.Now().Add(-9 * time.Hour)}}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 1 || f.actions[0].Kind != Human || len(f.launched) != 0 {
		t.Fatalf("actions=%v", f.actions)
	}
}
func TestTokenBucketUsesInjectedClock(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{{ID: "one", Repo: "b", Candidate: true}, {ID: "two", Repo: "b", Candidate: true}}}
	s := setup(f)
	s.Config.TokenBucket = config.TokenBucket{Capacity: 1, RefillPerSecond: 0.5}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 1 {
		t.Fatalf("launches=%d", len(f.launched))
	}
	f.launched = nil
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 0 {
		t.Fatal("refilled without time passing")
	}
	now := s.Now().Add(2 * time.Second)
	s.Now = func() time.Time { return now }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 1 {
		t.Fatal("did not refill")
	}
}

func TestServeContenderDoesNotInitialize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	initialized := false
	err = Serve(context.Background(), path, func(context.Context) (*Scheduler, error) { initialized = true; return nil, nil }, nil, nil)
	if err == nil || initialized {
		t.Fatal("contender initialized service without lock")
	}
}
func TestRestartAdoptsEveryMergeTicketAtCapacity(t *testing.T) {
	f := &fakeBackend{}
	s := setup(f)
	s.Config.MaxInFlight = 1
	f.tickets = []Ticket{{ID: "one", Repo: "a", Owner: machine.OwnerMerge, UpdatedAt: s.Now()}, {ID: "two", Repo: "b", Owner: machine.OwnerMerge, UpdatedAt: s.Now()}, {ID: "new", Repo: "b", Candidate: true}}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.MergeQueue) != 2 || len(f.launched) != 0 {
		t.Fatalf("merge=%v launch=%v", s.MergeQueue, f.launched)
	}
}
func TestUnprioritizedTicketSortsLast(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{{ID: "none", Repo: "b", Candidate: true, Priority: 0}, {ID: "low", Repo: "b", Candidate: true, Priority: 4}}}
	s := setup(f)
	s.Config.MaxInFlight = 1
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 1 || f.launched[0].ID != "low" {
		t.Fatalf("launch=%v", f.launched)
	}
}

func TestUncertainClaimReservesAdmissionSlot(t *testing.T) {
	f := &fakeBackend{applyErr: errors.New("write outcome unknown"), tickets: []Ticket{{ID: "one", Repo: "b", Candidate: true}, {ID: "two", Repo: "b", Candidate: true}}}
	s := setup(f)
	s.Config.MaxInFlight = 1
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("lost write failure")
	}
	if len(f.actions) != 1 || len(f.launched) != 0 {
		t.Fatalf("actions=%v launched=%v", f.actions, f.launched)
	}
}

func TestBlockedCandidateNotClaimed(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	f := &fakeBackend{tickets: []Ticket{
		{ID: "blocked", Repo: "b", Candidate: true, Blocked: true, Priority: 1, CreatedAt: now.Add(-time.Hour)},
		{ID: "free", Repo: "b", Candidate: true, Priority: 1, CreatedAt: now},
	}}
	s := setup(f)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 1 || f.launched[0].ID != "free" {
		t.Fatalf("launches=%v", f.launched)
	}
}

func TestUnregisteredExecutorRelaunchedAfterGrace(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		want int
	}{{"within grace", time.Minute, 0}, {"past grace", 5 * time.Minute, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBackend{terminalErr: errors.New("executor terminal unknown")}
			s := setup(f)
			f.tickets = []Ticket{{ID: "ticket", Repo: "a", Owner: machine.OwnerExecutor, Terminal: "-", Run: "-", Attempt: 1, UpdatedAt: s.Now().Add(-tc.age)}}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.launched) != tc.want {
				t.Fatalf("launches=%v", f.launched)
			}
			if tc.want == 1 && f.launched[0].Attempt != 2 {
				t.Fatal("attempt did not advance")
			}
		})
	}
}

type recordingMerger struct {
	*fakeBackend
	got [][]Ticket
}

func (m *recordingMerger) AdvanceMerges(_ context.Context, queue []Ticket) error {
	m.got = append(m.got, append([]Ticket(nil), queue...))
	return nil
}

// The merge queue is useless unless every tick hands it to the merge lane.
func TestTickHandsMergeQueueToMerger(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	f := &fakeBackend{tickets: []Ticket{{ID: "pr-open", Repo: "b", Owner: machine.OwnerMerge, UpdatedAt: now}}}
	s := setup(f)
	m := &recordingMerger{fakeBackend: f}
	s.Backend = m
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.got) != 1 || len(m.got[0]) != 1 || m.got[0][0].ID != "pr-open" {
		t.Fatalf("merger saw %v", m.got)
	}
}
