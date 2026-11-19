package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/hostmem"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
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
func TestTokenFairnessAcrossRepositories(t *testing.T) {
	for _, owner := range []machine.Owner{machine.OwnerResolver, machine.OwnerExecutor} {
		t.Run(string(owner), func(t *testing.T) {
			f := &fakeBackend{absent: true, inactive: true}
			s := setup(f)
			now := s.Now()
			s.Now = func() time.Time { return now }
			s.Config.TokenBucket.Capacity = 4
			for i := 0; i < 8; i++ {
				f.tickets = append(f.tickets, Ticket{ID: fmt.Sprintf("a-%d", i), Repo: "a", Owner: owner, Attempt: 1, UpdatedAt: now})
			}
			f.tickets = append(f.tickets, Ticket{ID: "b-1", Repo: "b", Owner: owner, Attempt: 1, UpdatedAt: now})
			for round := 0; round < 3; round++ {
				f.launched = nil
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(f.launched) > 4 {
					t.Fatalf("round launched %d tickets, limit 4", len(f.launched))
				}
				for _, ticket := range f.launched {
					if ticket.ID == "b-1" {
						return
					}
				}
				now = now.Add(time.Minute)
			}
			t.Fatal("repository b received no token within 3 rounds")
		})
	}
}

func TestTokenSkipIsObservable(t *testing.T) {
	for _, owner := range []machine.Owner{machine.OwnerResolver, machine.OwnerExecutor, machine.OwnerNone} {
		t.Run(string(owner), func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			f := &fakeBackend{absent: true, inactive: true}
			s := setup(f)
			s.Config.TokenBucket.Capacity = 1
			for _, id := range []string{"first", "waiting"} {
				f.tickets = append(f.tickets, Ticket{ID: id, Identifier: id, Repo: "b", Owner: owner, Candidate: owner == machine.OwnerNone, Attempt: 1, UpdatedAt: s.Now()})
			}
			decisions := captureDecisions(t, s)
			skips := 0
			for _, decision := range decisions {
				if decision["event"] == "skip" && decision["ticket_id"] == "waiting" && decision["reason"] == "token" {
					skips++
				}
			}
			if skips != 1 {
				t.Errorf("token skip records=%d, want 1: %v", skips, decisions)
			}
			data, err := os.ReadFile(status.PushPath())
			if err != nil {
				t.Fatal(err)
			}
			var view status.View
			if err := json.Unmarshal(data, &view); err != nil {
				t.Fatal(err)
			}
			for _, row := range view.Tickets {
				if row.Identifier == "waiting" {
					if row.BlockedReason != "token" {
						t.Errorf("waiting reason=%q, want token", row.BlockedReason)
					}
					return
				}
			}
			t.Fatal("waiting ticket missing from status")
		})
	}
}

func TestClaimsRespectRoundTokenLimit(t *testing.T) {
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
			if len(claims) != 0 {
				t.Fatalf("claims=%v after all 4 round slots were consumed", claims)
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

func TestTokenRefillDuringRound(t *testing.T) {
	for _, owner := range []machine.Owner{machine.OwnerResolver, machine.OwnerExecutor, machine.OwnerNone} {
		t.Run(string(owner), func(t *testing.T) {
			f := &fakeBackend{absent: true, inactive: true}
			s := setup(f)
			now := s.Now()
			s.Now = func() time.Time { return now }
			s.Config.TokenBucket = config.TokenBucket{Capacity: 4, RefillPerSecond: 0.5}
			for _, id := range []string{"one", "two", "three"} {
				f.tickets = append(f.tickets, Ticket{ID: id, Identifier: id, Repo: "b", Owner: owner, Candidate: owner == machine.OwnerNone, Attempt: 1, UpdatedAt: now})
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.launched) != 3 {
				t.Fatalf("initial launches=%d, want 3", len(f.launched))
			}
			f.launched = nil
			f.onApply = func(Ticket, Action) { now = now.Add(2 * time.Second) }
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.launched) != 3 {
				t.Fatalf("launches=%d, want 3 with elapsed-time refill", len(f.launched))
			}
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

type runtimeFailureOrca struct {
	fakeOrca
	failing       bool
	probes        int
	creates       int
	failureClient *orca.Client
	success       orca.Receipt
	closed        []string
}

func (o *runtimeFailureOrca) TerminalCreate(ctx context.Context, opts orca.TerminalOptions) (orca.Receipt, error) {
	if opts.Command == "true" {
		o.probes++
	} else {
		o.creates++
	}
	if o.failing {
		return o.failureClient.TerminalCreate(ctx, opts)
	}
	return o.success, nil
}

func (o *runtimeFailureOrca) TerminalClose(_ context.Context, handle string) error {
	o.closed = append(o.closed, handle)
	return nil
}

type runtimeFailureBackend struct {
	*fakeBackend
	runtime   *Runtime
	notices   int
	launchErr error
	merges    int
	merge     func(context.Context) error
}

func (b *runtimeFailureBackend) Launch(ctx context.Context, ticket Ticket) error {
	b.launched = append(b.launched, ticket)
	if b.launchErr != nil {
		return b.launchErr
	}
	return b.runtime.Launch(ctx, ticket)
}
func (b *runtimeFailureBackend) ProbeOrcaRuntime(ctx context.Context, repo string) error {
	return b.runtime.ProbeOrcaRuntime(ctx, repo)
}
func (b *runtimeFailureBackend) NotifyOrcaRuntime(context.Context, Ticket) error {
	b.notices++
	return nil
}
func (b *runtimeFailureBackend) OrcaTerminalCreateFailures() int {
	return b.runtime.OrcaTerminalCreateFailures()
}
func (b *runtimeFailureBackend) AdvanceMerges(ctx context.Context, _ []Ticket) error {
	b.merges++
	if b.merge == nil {
		return nil
	}
	return b.merge(ctx)
}

// recordedTerminalCreateRuntimeErrorCLI replays the rejection the driver journal
// recorded for `terminal create` during the 2026-11-18 Orca outage.
func recordedTerminalCreateRuntimeErrorCLI(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/recorded/driver-journal-orca-terminal-create-runtime-error.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var call struct {
		Msg     string `json:"msg"`
		Command string `json:"command"`
		Result  string `json:"result"`
		Request string `json:"request_id"`
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		if call.Msg == "Orca call" && call.Command == "terminal create" {
			break
		}
	}
	if call.Command != "terminal create" || call.Result != "runtime_error" || call.Request == "" {
		t.Fatalf("recorded terminal create rejection missing: %+v", call)
	}
	envelope, err := json.Marshal(map[string]any{"id": call.Request, "ok": false, "error": map[string]string{"code": call.Result}})
	if err != nil {
		t.Fatal(err)
	}
	reply := filepath.Join(t.TempDir(), "reply.json")
	if err := os.WriteFile(reply, envelope, 0600); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\ncat '"+reply+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return cli
}

type runtimeFailureFixture struct {
	s   *Scheduler
	f   *fakeBackend
	b   *runtimeFailureBackend
	r   *Runtime
	oc  *runtimeFailureOrca
	now *time.Time
}

func newRuntimeFailureFixture(t *testing.T, tickets int) runtimeFailureFixture {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	rawSuccess, err := os.ReadFile("../../testdata/recorded/orca-terminal-create-probe.json")
	if err != nil {
		t.Fatal(err)
	}
	var success struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(rawSuccess, &success); err != nil {
		t.Fatal(err)
	}
	oc := &runtimeFailureOrca{failing: true, failureClient: &orca.Client{Path: recordedTerminalCreateRuntimeErrorCLI(t), Timeout: time.Second}, success: success.Result}
	f := &fakeBackend{absent: true, inactive: true}
	r := &Runtime{Orca: oc, DriverPath: "/opt/driver", Clients: map[string]issues.Store{}, Repositories: map[string]config.Repository{}}
	b := &runtimeFailureBackend{fakeBackend: f, runtime: r}
	s := setup(f)
	s.Backend = b
	s.Config.MaxInFlight = 10
	s.Repositories = nil
	now := s.Now()
	s.Now = func() time.Time { return now }
	for i := 0; i < tickets; i++ {
		repo := fmt.Sprintf("/repo%d", i)
		id := fmt.Sprintf("OF-%d", i+1)
		s.Repositories = append(s.Repositories, config.Repository{Path: repo, MaxInFlight: 2, StaleHours: 8})
		r.Repositories[repo] = config.Repository{Path: repo}
		r.Clients[repo] = &fakeIssues{issues: []task.Issue{{ID: id, Identifier: id, State: task.NamedID{Name: "Todo"}, Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "ready-for-agent"}}}}}}
		f.tickets = append(f.tickets, Ticket{ID: id, Identifier: id, Repo: repo, Candidate: true})
	}
	f.onApply = func(ticket Ticket, action Action) {
		store := r.Clients[ticket.Repo].(*fakeIssues)
		body := fmt.Sprintf("阶段: %s\n归属: 执行器 term=- run=- attempt=%d scheduler-attempt=%d since=%s", action.Phase, action.Attempt, action.Attempt, now.Format(time.RFC3339))
		if action.Kind == Resolve {
			body = strings.Replace(body, "执行器", "处置Agent", 1)
		}
		if _, err := store.CreateComment(context.Background(), ticket.ID, body); err != nil {
			t.Fatal(err)
		}
		for i := range f.tickets {
			if f.tickets[i].ID == ticket.ID {
				f.tickets[i] = fromEvidence(t, Ticket{ID: ticket.ID, Identifier: ticket.Identifier, Repo: ticket.Repo}, r.Clients[ticket.Repo].(*fakeIssues).issueComments)
			}
		}
	}
	return runtimeFailureFixture{s: s, f: f, b: b, r: r, oc: oc, now: &now}
}

func TestOrcaRuntimeErrorPausesLaunchWithoutBurningAttempts(t *testing.T) {
	x := newRuntimeFailureFixture(t, 5)
	s, f, b, oc := x.s, x.f, x.b, x.oc
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("expected runtime failure")
	}
	if len(f.actions) != 3 || len(f.launched) != 3 {
		t.Fatalf("threshold must stop claims and launches: actions=%v launches=%d", f.actions, len(f.launched))
	}
	for i := 0; i < 3; i++ {
		*x.now = x.now.Add(time.Hour)
		_ = s.Tick(context.Background())
	}
	if len(f.actions) != 3 || len(f.launched) != 3 || b.notices != 1 || oc.probes != 3 {
		t.Fatalf("paused runtime changed ownership or dispatched: actions=%v launches=%d notices=%d probes=%d", f.actions, len(f.launched), b.notices, oc.probes)
	}
	for _, ticket := range f.tickets[:3] {
		if ticket.Attempt != 1 || ticket.Owner != machine.OwnerExecutor {
			t.Fatalf("burned attempt or escalated: %+v", ticket)
		}
	}
	if got := s.OrcaRuntime(); !got.Paused || got.Failures < 3 {
		t.Fatalf("missing runtime fault: %+v", got)
	}
	oc.failing = false
	*x.now = x.now.Add(time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 8 || len(f.actions) != 5 {
		t.Fatalf("recovery must retry same reservations then claim: actions=%v launches=%v", f.actions, f.launched)
	}
	if !reflect.DeepEqual(oc.closed, []string{"term_4aa07a88-df90-4866-aa2e-dcefaf47537c"}) {
		t.Fatalf("probe terminal not closed: %v", oc.closed)
	}
	if got := s.OrcaRuntime(); got.Paused || got.Failures != 0 {
		t.Fatalf("recovery state not cleared: %+v", got)
	}
	for _, ticket := range f.launched {
		if ticket.Attempt != 1 {
			t.Fatalf("attempt changed during recovery: %+v", ticket)
		}
	}
}

// A retry that fails before terminal create, or is deferred, neither drops the
// reservation nor counts as Orca having recovered.
func TestOrcaRuntimeRetryKeepsReservationUntilTerminalCreateAnswers(t *testing.T) {
	x := newRuntimeFailureFixture(t, 1)
	s, f, b, oc := x.s, x.f, x.b, x.oc
	_ = s.Tick(context.Background())
	if got := s.OrcaRuntime(); got.Failures != 1 || len(f.launched) != 1 {
		t.Fatalf("first runtime failure not counted: %+v launches=%d", got, len(f.launched))
	}
	for _, launchErr := range []error{errors.New("claim issue unavailable"), maintenance.ErrRefused} {
		*x.now = x.now.Add(time.Hour)
		b.launchErr = launchErr
		_ = s.Tick(context.Background())
		if got := s.OrcaRuntime(); got.Failures != 1 {
			t.Fatalf("launch without terminal create changed the failure count after %v: %+v", launchErr, got)
		}
	}
	b.launchErr = nil
	oc.failing = false
	*x.now = x.now.Add(time.Hour)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.OrcaRuntime(); got.Failures != 0 {
		t.Fatalf("terminal create success did not clear the count: %+v", got)
	}
	if len(f.actions) != 1 || len(f.launched) != 4 {
		t.Fatalf("reservation dropped and reclaimed: actions=%v launches=%v", f.actions, f.launched)
	}
	for _, ticket := range f.launched {
		if ticket.Attempt != 1 {
			t.Fatalf("attempt burned while Orca was failing: %+v", ticket)
		}
	}
}

// A reservation relaunched after a long outage has the same claim time as
// before the outage, so its executor gets a fresh grace to register.
func TestOrcaRuntimeRecoveredLaunchGetsFreshRegistrationGrace(t *testing.T) {
	x := newRuntimeFailureFixture(t, 1)
	s, f, oc := x.s, x.f, x.oc
	_ = s.Tick(context.Background())
	if len(f.actions) != 1 || len(f.launched) != 1 {
		t.Fatalf("claim not attempted: actions=%v launches=%d", f.actions, len(f.launched))
	}
	oc.failing = false
	*x.now = x.now.Add(time.Hour)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.launched) != 2 {
		t.Fatalf("recovery did not relaunch the reservation: launches=%d", len(f.launched))
	}
	*x.now = x.now.Add(s.unregisteredGrace() / 2)
	_ = s.Tick(context.Background())
	if len(f.actions) != 1 || len(f.launched) != 2 || f.tickets[0].Attempt != 1 {
		t.Fatalf("recovered executor judged dead within its grace: actions=%v launches=%d ticket=%+v", f.actions, len(f.launched), f.tickets[0])
	}
}

// correctionTickets records executor-owned correction reservations, as the merge
// lane leaves them before it launches the correction executor.
func correctionTickets(t *testing.T, x runtimeFailureFixture, ids ...string) []Ticket {
	t.Helper()
	tickets := make([]Ticket, 0, len(ids))
	for _, id := range ids {
		repo := "/merge-" + id
		store := &fakeIssues{issues: []task.Issue{{ID: id, Identifier: id, State: task.NamedID{Name: "Todo"}, Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "ready-for-agent"}}}}}}
		body := fmt.Sprintf("阶段: %s\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1 since=%s", machine.PhaseCorrection, x.now.Format(time.RFC3339))
		if _, err := store.CreateComment(context.Background(), id, body); err != nil {
			t.Fatal(err)
		}
		x.r.Clients[repo] = store
		x.r.Repositories[repo] = config.Repository{Path: repo}
		tickets = append(tickets, fromEvidence(t, Ticket{ID: id, Identifier: id, Repo: repo}, store.issueComments))
	}
	return tickets
}

// The merge lane launches corrections through Runtime.Launch, outside the claim
// loop, and joins every repository's error. Each terminal create rejection in
// the batch counts, and the threshold stops the rest of the batch.
func TestOrcaRuntimeErrorFromMergeCorrectionCounts(t *testing.T) {
	x := newRuntimeFailureFixture(t, 0)
	s, b := x.s, x.b
	s.Repositories = []config.Repository{{Path: "/repo", MaxInFlight: 2, StaleHours: 8}}
	tickets := correctionTickets(t, x, "OF-9", "OF-10", "OF-11", "OF-12")
	b.merge = func(ctx context.Context) error {
		var problems []error
		for _, ticket := range tickets {
			problems = append(problems, fmt.Errorf("merge waiting %s: %w", ticket.Identifier, b.runtime.Launch(ctx, ticket)))
		}
		return errors.Join(problems...)
	}
	_ = s.Tick(context.Background())
	if got := s.OrcaRuntime(); !got.Paused || got.Failures != orcaRuntimeFailureThreshold || b.notices != 1 {
		t.Fatalf("batched merge correction runtime errors not counted: %+v notices=%d", got, b.notices)
	}
	if x.oc.creates != orcaRuntimeFailureThreshold {
		t.Fatalf("terminal create continued after the threshold: creates=%d", x.oc.creates)
	}
	for _, ticket := range tickets {
		if _, ok := s.runtimePending[ticket.ID]; !ok {
			t.Fatalf("correction reservation %s not kept for retry: %v", ticket.Identifier, s.runtimePending)
		}
	}
}

// Any terminal create success ends the consecutive run, including one from a
// merge correction the scheduler never sees succeed.
func TestOrcaRuntimeMergeCorrectionSuccessClearsCount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failing  []bool
		failures int
	}{
		{"failure after success", []bool{true, true, false, true}, 1},
		{"batch ends in success", []bool{true, true, false}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRuntimeFailureFixture(t, 0)
			s, b := x.s, x.b
			s.Repositories = []config.Repository{{Path: "/repo", MaxInFlight: 2, StaleHours: 8}}
			tickets := correctionTickets(t, x, "OF-9", "OF-10", "OF-11", "OF-12")[:len(tc.failing)]
			b.merge = func(ctx context.Context) error {
				var problems []error
				for i, ticket := range tickets {
					x.oc.failing = tc.failing[i]
					problems = append(problems, b.runtime.Launch(ctx, ticket))
				}
				return errors.Join(problems...)
			}
			_ = s.Tick(context.Background())
			if got := s.OrcaRuntime(); got.Paused || got.Failures != tc.failures {
				t.Fatalf("terminal create success did not restart the count: %+v", got)
			}
		})
	}
}

// A pause reached while retrying reservations holds the merge lane in the same
// round: a merge correction would create an executor terminal.
func TestOrcaRuntimePauseHoldsMergeLaneInSameRound(t *testing.T) {
	x := newRuntimeFailureFixture(t, orcaRuntimeFailureThreshold-1)
	_ = x.s.Tick(context.Background())
	if got := x.s.OrcaRuntime(); got.Paused || got.Failures != orcaRuntimeFailureThreshold-1 || x.b.merges != 1 {
		t.Fatalf("claims below the threshold: %+v merges=%d", got, x.b.merges)
	}
	*x.now = x.now.Add(time.Hour)
	_ = x.s.Tick(context.Background())
	if got := x.s.OrcaRuntime(); !got.Paused || x.b.merges != 1 {
		t.Fatalf("merge lane advanced after the pause: %+v merges=%d", got, x.b.merges)
	}
}

// Only serve publishes the fault; a separate Scheduler such as `serve --dry-run`
// starts at zero and must not clear the running service's paused state.
func TestSchedulerTickLeavesPublishedOrcaRuntime(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	s := setup(&fakeBackend{})
	status.Push(s.Now(), func(view *status.View) { view.OrcaRuntime = status.OrcaRuntime{Paused: true, Failures: 3} })
	_ = s.Tick(context.Background())
	if got := status.Read(s.Now(), 0).OrcaRuntime; !got.Paused {
		t.Fatalf("scheduler tick overwrote the published Orca runtime fault: %+v", got)
	}
}

func TestHostMemoryHoldSkipsNewLaunchesWithoutBurningAttempts(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := &fakeBackend{absent: true, inactive: true}
	s := setup(f)
	f.tickets = []Ticket{
		{ID: "candidate", Identifier: "candidate", Repo: "b", Candidate: true, CreatedAt: s.Now()},
		{ID: "executor", Identifier: "executor", Repo: "a", Owner: machine.OwnerExecutor, Attempt: 1, UpdatedAt: s.Now()},
	}
	reading := hostmem.Reading{Available: 5 << 30}
	s.HostMemory = func() (hostmem.Reading, error) { return reading, nil }
	skipped := map[string]bool{}
	for _, decision := range captureDecisions(t, s) {
		if decision["event"] == "skip" && decision["reason"] == "memory" {
			skipped[decision["ticket_id"].(string)] = true
		}
	}
	if len(f.actions) != 0 || len(f.launched) != 0 {
		t.Fatalf("held host memory claimed or launched: actions=%v launches=%v", f.actions, f.launched)
	}
	if !skipped["candidate"] || !skipped["executor"] {
		t.Fatalf("memory skips=%v, want candidate and executor", skipped)
	}
	if got := s.HostMemoryStatus(); !got.Paused || !got.Since.Equal(s.Now()) || got.Reading != reading {
		t.Fatalf("host memory status = %+v", got)
	}
	for _, ticket := range f.tickets {
		if ticket.Attempt != map[string]int{"candidate": 0, "executor": 1}[ticket.ID] {
			t.Fatalf("attempt changed while held: %+v", ticket)
		}
	}
	reading = hostmem.Reading{Available: 30 << 30}
	claimed := []string{}
	f.onApply = func(ticket Ticket, a Action) {
		if a.Kind == Claim {
			claimed = append(claimed, ticket.ID)
		}
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(claimed, []string{"candidate"}) {
		t.Fatalf("released round claims=%v, want candidate", claimed)
	}
	launched := map[string]int{}
	for _, ticket := range f.launched {
		launched[ticket.ID] = ticket.Attempt
	}
	if !reflect.DeepEqual(launched, map[string]int{"candidate": 1, "executor": 2}) {
		t.Fatalf("released round launches=%v", launched)
	}
	if got := s.HostMemoryStatus(); got.Paused {
		t.Fatalf("host memory still paused after release: %+v", got)
	}
}

func TestHostMemoryHoldStillAppliesReadyResolverResult(t *testing.T) {
	f := &fakeBackend{tickets: []Ticket{{ID: "id", Identifier: "ENG-1", Repo: "a", Owner: machine.OwnerResolver, Terminal: "controller", Resolution: &resolver.Record{Version: 1, Attempt: 1, Phase: "result"}}}}
	s := setup(f)
	s.HostMemory = func() (hostmem.Reading, error) { return hostmem.Reading{Available: 5 << 30}, nil }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 1 || f.actions[0].Kind != Resolve {
		t.Fatalf("held host memory must still apply a reported resolver result: actions=%+v", f.actions)
	}
}

func TestHostMemoryHysteresisSurvivesReadError(t *testing.T) {
	s := setup(&fakeBackend{})
	steps := []struct {
		reading hostmem.Reading
		err     error
		paused  bool
	}{
		{hostmem.Reading{Available: 5 << 30}, nil, true},
		{hostmem.Reading{}, errors.New("read /proc/meminfo: gone"), false},
		{hostmem.Reading{Available: 8 << 30}, nil, true},
	}
	start := s.Now()
	for i, step := range steps {
		now := start.Add(time.Duration(i) * time.Minute)
		s.Now = func() time.Time { return now }
		s.HostMemory = func() (hostmem.Reading, error) { return step.reading, step.err }
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := s.HostMemoryStatus()
		if got.Paused != step.paused {
			t.Fatalf("step %d: status=%+v, want paused=%v", i, got, step.paused)
		}
		if step.paused && !got.Since.Equal(start) {
			t.Fatalf("step %d: hold start %v, want the first hold at %v", i, got.Since, start)
		}
	}
}

type hostMemoryNotifyBackend struct {
	fakeBackend
	notices []status.HostMemory
	err     error
}

func (b *hostMemoryNotifyBackend) NotifyHostMemory(_ context.Context, memory status.HostMemory) error {
	b.notices = append(b.notices, memory)
	return b.err
}

func TestHostMemoryHoldNotifiesOnceAfterFifteenMinutes(t *testing.T) {
	b := &hostMemoryNotifyBackend{}
	s := setup(&b.fakeBackend)
	s.Backend = b
	start := s.Now()
	reading := hostmem.Reading{Available: 3 << 30}
	s.HostMemory = func() (hostmem.Reading, error) { return reading, nil }
	for _, step := range []struct {
		elapsed   time.Duration
		available uint64
		notices   int
	}{
		{0, 3 << 30, 0}, {14*time.Minute + 59*time.Second, 3 << 30, 0},
		{15 * time.Minute, 3 << 30, 1}, {75 * time.Minute, 3 << 30, 1},
		{76 * time.Minute, 20 << 30, 1}, {77 * time.Minute, 3 << 30, 1}, {92 * time.Minute, 3 << 30, 2},
	} {
		now := start.Add(step.elapsed)
		s.Now = func() time.Time { return now }
		reading.Available = step.available
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(b.notices) != step.notices {
			t.Fatalf("at %v: notices=%d, want %d", step.elapsed, len(b.notices), step.notices)
		}
	}
	if !b.notices[0].Since.Equal(start) || !b.notices[1].Since.Equal(start.Add(77*time.Minute)) {
		t.Fatalf("hold starts: %+v", b.notices)
	}
}

func TestHostMemoryNotifyClockExcludesUnreadableRounds(t *testing.T) {
	b := &hostMemoryNotifyBackend{}
	s := setup(&b.fakeBackend)
	s.Backend = b
	start := s.Now()
	low := hostmem.Reading{Available: 3 << 30}
	unreadable := errors.New("read /proc/meminfo: denied")
	for _, step := range []struct {
		elapsed time.Duration
		reading hostmem.Reading
		err     error
		paused  bool
		notices int
	}{
		{0, low, nil, true, 0},
		{time.Minute, hostmem.Reading{}, unreadable, false, 0},
		{15 * time.Minute, hostmem.Reading{}, unreadable, false, 0},
		{16 * time.Minute, low, nil, true, 0},
		{30*time.Minute + 59*time.Second, low, nil, true, 0},
		{31 * time.Minute, low, nil, true, 1},
		{32 * time.Minute, hostmem.Reading{}, unreadable, false, 1},
		{33 * time.Minute, low, nil, true, 1},
		{47*time.Minute + 59*time.Second, low, nil, true, 1},
		{48 * time.Minute, low, nil, true, 2},
	} {
		s.Now = func() time.Time { return start.Add(step.elapsed) }
		s.HostMemory = func() (hostmem.Reading, error) { return step.reading, step.err }
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := s.HostMemoryStatus(); got.Paused != step.paused || len(b.notices) != step.notices {
			t.Fatalf("at %v: paused=%v notices=%d, want paused=%v notices=%d", step.elapsed, got.Paused, len(b.notices), step.paused, step.notices)
		}
	}
	if got, want := b.notices[0].Since, start.Add(16*time.Minute); !got.Equal(want) {
		t.Errorf("notification since = %v, want %v", got, want)
	}
	if got, want := b.notices[1].Since, start.Add(33*time.Minute); !got.Equal(want) {
		t.Errorf("second notification since = %v, want %v", got, want)
	}
	if got := s.HostMemoryStatus().Since; !got.Equal(start) {
		t.Errorf("snapshot since = %v, want %v", got, start)
	}
}

func TestHostMemoryNotificationRetriesAfterFailure(t *testing.T) {
	b := &hostMemoryNotifyBackend{err: errors.New("offline")}
	s := setup(&b.fakeBackend)
	s.Backend = b
	now := s.Now()
	s.Now = func() time.Time { return now }
	s.HostMemory = func() (hostmem.Reading, error) { return hostmem.Reading{Available: 3 << 30}, nil }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.err = nil
	now = now.Add(time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.notices) != 2 {
		t.Fatalf("notification attempts = %d, want 2", len(b.notices))
	}
}

func TestHostMemoryCgroupErrorStillUsesHostReading(t *testing.T) {
	s := setup(&fakeBackend{})
	reading := hostmem.Reading{Available: 3 << 30, Source: "host"}
	s.HostMemory = func() (hostmem.Reading, error) { return reading, errors.New("read cgroup memory.stat: denied") }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := s.HostMemoryStatus()
	if !got.Paused || got.Reading != reading || !strings.Contains(got.CgroupError, "memory.stat") || got.Error != "" {
		t.Fatalf("fallback status = %+v", got)
	}
}

func TestHostMemoryReadFailureKeepsLastReading(t *testing.T) {
	s := setup(&fakeBackend{})
	reading := hostmem.Reading{Available: 3 << 30, SomeAvg60: 30, Source: "/orca.slice"}
	s.HostMemory = func() (hostmem.Reading, error) { return reading, nil }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.HostMemory = func() (hostmem.Reading, error) { return hostmem.Reading{}, errors.New("read /proc/meminfo: denied") }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.HostMemoryStatus(); got.Paused || got.Reading != reading || !strings.Contains(got.Error, "/proc/meminfo") || got.CgroupError != "" {
		t.Fatalf("unreadable host = %+v", got)
	}
}
