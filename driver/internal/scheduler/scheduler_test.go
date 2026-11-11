package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
)

type fakeBackend struct {
	tickets                  []Ticket
	snapshots                int
	enabled                  map[string]bool
	absent, inactive         bool
	terminalErr, dispatchErr error
	applyErr                 error
	onApply                  func(Ticket, Action)
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
func (f *fakeBackend) Apply(_ context.Context, ticket Ticket, a Action) error {
	f.actions = append(f.actions, a)
	if f.onApply != nil {
		f.onApply(ticket, a)
	}
	return f.applyErr
}
func (f *fakeBackend) Launch(_ context.Context, t Ticket) error {
	f.launched = append(f.launched, t)
	return nil
}
func setup(f *fakeBackend) *Scheduler {
	return &Scheduler{Config: config.Config{MaxInFlight: 3, TokenBucket: config.TokenBucket{Capacity: 10, RefillPerSecond: 1}, Tuning: config.Tuning{Scheduler: config.DefaultScheduler()}}, Repositories: []config.Repository{{Path: "a", MaxInFlight: 1, StaleHours: 8}, {Path: "b", MaxInFlight: 3, StaleHours: 8}}, Backend: f, Now: func() time.Time { return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) }}
}
func TestSecondInstanceCannotLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	}()
	second, err := Acquire(path)
	if err == nil {
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
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
	if len(f.launched) != 1 || len(f.actions) != 1 || f.actions[0].Kind != Resolve {
		t.Fatalf("actions=%v", f.actions)
	}
}

// OF-100: an Apply failure must name the ticket and the action it attempted,
// and must never surface a credential from the backend's raw error text.
func TestOwnershipApplyFailureNamesTicketAndAction(t *testing.T) {
	f := &fakeBackend{absent: true, inactive: true, applyErr: errors.New("write rejected: ghp_TEST-PLACEHOLDER-CRED")}
	s := setup(f)
	f.tickets = []Ticket{{ID: "ticket", Identifier: "OF-100", Repo: "a", Owner: machine.OwnerExecutor, Attempt: 1, UpdatedAt: s.Now()}}
	err := s.Tick(context.Background())
	if err == nil {
		t.Fatal("expected ownership update failure")
	}
	if !strings.Contains(err.Error(), "OF-100") || !strings.Contains(err.Error(), string(Resume)) {
		t.Fatalf("missing ticket or action: %v", err)
	}
	if strings.Contains(err.Error(), "TEST-PLACEHOLDER-CRED") {
		t.Fatalf("leaked credential: %v", err)
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
	if len(f.actions) != 1 || f.actions[0].Kind != Resolve || len(f.launched) != 1 {
		t.Fatalf("actions=%v", f.actions)
	}
}
func TestClaimsNotStarvedByResumes(t *testing.T) {
	for _, advanceClock := range []bool{true, false} {
		name := "clock advances during resumes"
		if !advanceClock {
			name = "clock does not advance"
		}
		t.Run(name, func(t *testing.T) {
			backend := &fakeBackend{absent: true, inactive: true}
			scheduler := setup(backend)
			now := scheduler.Now()
			scheduler.Now = func() time.Time { return now }
			scheduler.Config.MaxInFlight = 5
			scheduler.Config.TokenBucket = config.TokenBucket{Capacity: 4, RefillPerSecond: 1.0 / 30}
			scheduler.Repositories[1].MaxInFlight = 5
			for _, identifier := range []string{"resume-1", "resume-2", "resume-3", "resume-4"} {
				backend.tickets = append(backend.tickets, Ticket{ID: identifier, Repo: "b", Owner: machine.OwnerExecutor, Attempt: 1, UpdatedAt: now})
			}
			backend.tickets = append(backend.tickets, Ticket{ID: "candidate", Repo: "b", Candidate: true})
			resumes := 0
			var claims []string
			backend.onApply = func(ticket Ticket, action Action) {
				switch action.Kind {
				case Resume:
					resumes++
					if advanceClock {
						now = now.Add(30 * time.Second)
					}
				case Claim:
					claims = append(claims, ticket.ID)
				}
			}
			decisions := captureDecisions(t, scheduler)
			if resumes != 4 {
				t.Fatalf("resumes=%d, want 4", resumes)
			}
			if advanceClock {
				if !reflect.DeepEqual(claims, []string{"candidate"}) {
					t.Fatalf("claims=%v, want candidate", claims)
				}
				return
			}
			if len(claims) != 0 {
				t.Fatalf("claims=%v without time passing", claims)
			}
			for _, decision := range decisions {
				if decision["event"] == "skip" && decision["ticket_id"] == "candidate" && decision["reason"] == "token" {
					return
				}
			}
			t.Fatalf("candidate not skipped for token: %v", decisions)
		})
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
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
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

// ledgerBackend decides from a fixed snapshot but applies and launches through
// the real Runtime, so the launch rereads the reservation the Resume wrote.
type ledgerBackend struct {
	fakeBackend
	runtime *Runtime
}

func (b *ledgerBackend) Apply(ctx context.Context, t Ticket, a Action) error {
	b.actions = append(b.actions, a)
	return b.runtime.Apply(ctx, t, a)
}
func (b *ledgerBackend) Launch(ctx context.Context, t Ticket) error {
	return b.runtime.Launch(ctx, t)
}

// OF-458's ledger: a correction executor registered its run, then exited on a
// maintenance refusal. The Resume reservation is written with run=-, and its
// launch must accept that reservation: a deferred launch leaves an unregistered
// claim that the next round counts as a failed executor.
func TestMaintenanceRefusedCorrectionResumeLaunches(t *testing.T) {
	comments := evidenceComments(
		"阶段: worker 运行中\n归属: 执行器 term=term_00d4edb4-af2e-4c2f-aaa7-6cd06a108537 run=run_37842cdf5951 attempt=1 since=2026-11-10T15:48:16Z",
		"阶段: gate",
		"阶段: 写 correction",
		"correction 1 → 升至 strong",
		maintenance.RefusedLine+"派工未发往 Orca，维护恢复后重放 term=term_00d4edb4-af2e-4c2f-aaa7-6cd06a108537",
	)
	lc := &fakeIssues{issueComments: comments}
	oc := &fakeOrca{}
	f := &ledgerBackend{fakeBackend: fakeBackend{absent: true, inactive: true}, runtime: &Runtime{Clients: map[string]issues.Store{"a": lc}, Orca: oc, DriverPath: "/opt/driver"}}
	s := setup(&f.fakeBackend)
	s.Backend = f
	f.tickets = []Ticket{fromEvidence(t, Ticket{ID: "id", Identifier: "OF-458", Repo: "a", UpdatedAt: s.Now()}, comments)}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 1 || f.actions[0].Kind != Resume || f.actions[0].Attempt != 1 || f.actions[0].Phase != machine.PhaseCorrection || len(oc.launches) != 1 {
		t.Fatalf("refused correction not relaunched at its attempt: actions=%+v launches=%d ledger=%q", f.actions, len(oc.launches), lc.comments)
	}
}
