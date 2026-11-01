package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func crossReviewScript(t *testing.T, dir, name, output string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := fmt.Sprintf("#!/bin/sh\ncat > /dev/null\necho run >> %s.count\n[ -e tracked ] || echo missing-checkout >> %s.count\ncat <<'OUT'\n%s\nOUT\n", path, path, output)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

type crossReviewFixture struct {
	store   *notifyStore
	repo    config.Repository
	runtime *Runtime
	head    string
	first   string
	second  string
}

// A real repository with one branch commit, so the job really fetches, diffs and
// checks out the pinned head, and an escalated ticket owned by a human.
func newCrossReviewFixture(t *testing.T, class, changed string) *crossReviewFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir, bin := t.TempDir(), t.TempDir()
	gitMergeTest(t, dir, "init", "-b", "main")
	gitMergeTest(t, dir, "config", "user.email", "test@example.test")
	gitMergeTest(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitMergeTest(t, dir, "add", ".")
	gitMergeTest(t, dir, "commit", "-m", "base")
	gitMergeTest(t, dir, "checkout", "-b", "agent/OF-1/executor")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, changed)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, changed), []byte("fix"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitMergeTest(t, dir, "add", ".")
	gitMergeTest(t, dir, "commit", "-m", "fix")
	head := gitMergeTest(t, dir, "rev-parse", "HEAD")
	gitMergeTest(t, dir, "checkout", "main")
	gitMergeTest(t, dir, "remote", "add", "origin", dir)
	green := `{"step":"cross-review","verdict":"green","expands_authority":false,"reason":"一行修复"}`
	f := &crossReviewFixture{head: head, first: crossReviewScript(t, bin, "astra", green), second: crossReviewScript(t, bin, "fable", green)}
	result, err := json.Marshal(map[string]string{"action": "escalate", "reason": "需要扩 SCOPE", "body": "根因\n归属: 合并通道", "class": class, "head": head})
	if err != nil {
		t.Fatal(err)
	}
	report := resolverReport(t, 1, "failed", string(result))
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(report.Body, "resolver-state ")), &record); err != nil {
		t.Fatal(err)
	}
	record["phase"], record["dispatch"] = "human", "ctx_1"
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	f.store = &notifyStore{commentsByID: map[string][]task.Comment{"id": {{ID: "h", Body: "转人: 需要扩 SCOPE\n阶段: 转人\n归属: 人\nresolver-state " + string(raw), CreatedAt: time.Now()}}}}
	f.store.issues = []task.Issue{{ID: "id", Identifier: "OF-1", Title: "blocked", URL: "https://example.feishu.cn/base/base?record=id", Project: task.NamedID{ID: "project"}}}
	f.store.description = "## 要做\n1. 修\n\n## SCOPE\nsrc/*\n"
	f.repo = config.Repository{Path: dir, MaxInFlight: 3, StaleHours: 8, Env: map[string]string{
		"AGENT_ISSUE_BACKEND": "feishu", "AGENT_GH_REPO": "o/r",
		"AGENT_ROLE_REVIEWER_CMD": f.first, "AGENT_ROLE_SECOND_REVIEWER_CMD": f.second,
		"AGENT_LANE3_GLOBS": "lane3/*", "AGENT_CROSS_REVIEW_FORBIDDEN_GLOBS": "core/*",
	}}
	f.runtime = &Runtime{Clients: map[string]issues.Store{dir: f.store}, Repositories: map[string]config.Repository{dir: f.repo}, Now: time.Now}
	t.Cleanup(func() {
		if err := f.runtime.Close(30 * time.Second); err != nil {
			t.Error(err)
		}
	})
	return f
}

// crossReviewBackend runs a real Runtime's snapshot and cross reviews under the
// scheduler, with the rest of the backend faked.
type crossReviewBackend struct {
	notifyTickBackend
}

func (b *crossReviewBackend) AdvanceCrossReviews(ctx context.Context, tickets []Ticket) error {
	return b.runtime.AdvanceCrossReviews(ctx, tickets)
}

func (f *crossReviewFixture) scheduler() *Scheduler {
	backend := &crossReviewBackend{notifyTickBackend{runtime: f.runtime}}
	backend.snapshot = func() ([]Ticket, error) {
		return f.runtime.Snapshot(context.Background(), []config.Repository{f.repo})
	}
	s := setup(&backend.fakeBackend)
	s.Backend = backend
	s.Repositories = []config.Repository{f.repo}
	return s
}

func (f *crossReviewFixture) records(t *testing.T) []crossReviewRecord {
	t.Helper()
	var out []crossReviewRecord
	for _, c := range f.store.commentsByID["id"] {
		if raw, ok := strings.CutPrefix(c.Body, crossReviewPrefix); ok {
			var record crossReviewRecord
			if err := json.Unmarshal([]byte(raw), &record); err != nil {
				t.Fatal(err)
			}
			out = append(out, record)
		}
	}
	return out
}

// tick runs the real scheduler until a record is written and returns the
// ticket as the following snapshot reads it.
func (f *crossReviewFixture) tickUntilRecorded(t *testing.T) Ticket {
	t.Helper()
	ctx := context.Background()
	s := f.scheduler()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_ = s.Tick(ctx)
		tickets, err := f.runtime.Snapshot(ctx, []config.Repository{f.repo})
		if err != nil {
			t.Fatal(err)
		}
		if len(f.records(t)) > 0 {
			if len(tickets) != 1 {
				t.Fatalf("tickets=%+v", tickets)
			}
			return tickets[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no record: %+v", f.store.commentsByID["id"])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEscalationCrossReviewRecordsWithoutReleasing(t *testing.T) {
	f := newCrossReviewFixture(t, "scope-extension", "other/fix.go")
	ticket := f.tickUntilRecorded(t)
	if ticket.Owner != machine.OwnerHuman || ticket.Phase != machine.PhaseHuman {
		t.Fatalf("shadow review moved the ticket: owner=%s phase=%s", ticket.Owner, ticket.Phase)
	}
	s := f.scheduler()
	for range 3 {
		_ = s.Tick(context.Background())
	}
	got := f.records(t)
	if len(got) != 1 {
		t.Fatalf("records=%+v", got)
	}
	r := got[0]
	if !r.WouldRelease || r.Class != "scope-extension" || r.Head != f.head || r.Dispatch != "ctx_1" || len(r.Opinions) != 2 || strings.Join(r.OutsideScope, ",") != "other/fix.go" {
		t.Fatalf("record=%+v", r)
	}
	for _, script := range []string{f.first, f.second} {
		count, err := os.ReadFile(script + ".count")
		if err != nil {
			t.Fatal(err)
		}
		if string(count) != "run\n" {
			t.Fatalf("%s ran: %q", script, count)
		}
	}
	for _, c := range f.store.commentsByID["id"] {
		if strings.HasPrefix(c.Body, crossReviewPrefix) && strings.Contains(c.Body, "\n") {
			t.Fatalf("record can forge ledger lines: %q", c.Body)
		}
	}
}

func TestCrossReviewForbiddenPathNeverReleases(t *testing.T) {
	for _, changed := range []string{"postgres-migrations/0141_x.sql", "lane3/key", "core/gate.sh", ".github/workflows/deploy.yml", "wrangler.toml", "scripts/agent-gates/gate.sh"} {
		t.Run(changed, func(t *testing.T) {
			f := newCrossReviewFixture(t, "scope-extension", changed)
			f.tickUntilRecorded(t)
			r := f.records(t)[0]
			if r.WouldRelease || strings.Join(r.Forbidden, ",") != changed {
				t.Fatalf("record=%+v", r)
			}
		})
	}
}

func TestCrossReviewOutsideWhitelistDoesNotRun(t *testing.T) {
	for name, mutate := range map[string]func(*crossReviewFixture){
		"credential block":   nil,
		"no second reviewer": func(f *crossReviewFixture) { f.repo.Env["AGENT_ROLE_SECOND_REVIEWER_CMD"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			class := "credentials"
			if mutate != nil {
				class = "scope-extension"
			}
			f := newCrossReviewFixture(t, class, "src/fix.go")
			if mutate != nil {
				mutate(f)
			}
			s := f.scheduler()
			for range 3 {
				_ = s.Tick(context.Background())
				time.Sleep(20 * time.Millisecond)
			}
			if _, err := os.Stat(f.first + ".count"); err == nil {
				t.Fatal("reviewer ran")
			}
		})
	}
}

// Stdout of the real reviewer commands on this change's own diff, 2026-11-01,
// run with only HOME and PATH. Both end in prose followed by the verdict object.
func TestRecordedCrossReviewOutput(t *testing.T) {
	for _, name := range []string{"cross-review-output-astra.txt", "cross-review-output-fable.txt"} {
		got := parseCrossReviewOpinion("r", string(recorded(t, name)))
		if got.Verdict != "red" || got.ExpandsAuthority || got.Reason == "" {
			t.Errorf("%s: %+v", name, got)
		}
	}
	for _, output := range []string{"", "looks fine", `{"step":"cross-review","verdict":"green"}`, `{"step":"cross-review","verdict":"green","expands_authority":false}`, `{"step":"cross-review","verdict":"green","expands_authority":false,"reason":" "}`, `{"step":"review","verdict":"green","expands_authority":false}`, `{"step":"cross-review","verdict":"green","expands_authority":false} trailing`} {
		if got := parseCrossReviewOpinion("r", output); got.Verdict != crossReviewUnavailable {
			t.Errorf("%q counted as %+v", output, got)
		}
	}
}

// lostAckStore lands the first comment and then reports the write as failed,
// as when the ledger response is lost.
type lostAckStore struct {
	*notifyStore
	lost bool
}

func (s *lostAckStore) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	c, err := s.notifyStore.CreateComment(ctx, id, body)
	if err == nil && !s.lost {
		s.lost = true
		return c, errors.New("response lost")
	}
	return c, err
}

func TestCrossReviewLostWriteResponseDoesNotDuplicate(t *testing.T) {
	f := newCrossReviewFixture(t, "scope-extension", "src/fix.go")
	f.runtime.Clients[f.repo.Path] = &lostAckStore{notifyStore: f.store}
	f.tickUntilRecorded(t)
	s := f.scheduler()
	for range 3 {
		_ = s.Tick(context.Background())
	}
	if got := f.records(t); len(got) != 1 {
		t.Fatalf("records=%+v", got)
	}
}

// A repository still under the old Automation refuses this driver's work.
func TestCrossReviewRespectsRefusedRepository(t *testing.T) {
	f := newCrossReviewFixture(t, "scope-extension", "src/fix.go")
	s := f.scheduler()
	s.Backend.(*crossReviewBackend).enabled = map[string]bool{f.repo.Path: true}
	for range 3 {
		_ = s.Tick(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(f.first + ".count"); err == nil {
		t.Fatal("reviewer ran for a refused repository")
	}
}

// A record only settles a review when it names this ticket: any other comment
// must not be able to suppress the review.
func TestCrossReviewRecordMustNameTicket(t *testing.T) {
	line := func(version int, ticket string) task.Comment {
		raw, err := json.Marshal(crossReviewRecord{Version: version, TicketID: ticket, Dispatch: "ctx_1"})
		if err != nil {
			t.Fatal(err)
		}
		return task.Comment{Body: crossReviewPrefix + string(raw)}
	}
	var ticket Ticket
	ticket.ID = "id"
	readCrossReviews(&ticket, []task.Comment{line(1, "other"), line(2, "id"), {Body: crossReviewPrefix + "{"}})
	if ticket.CrossReviewed["ctx_1"] {
		t.Fatal("foreign record settled the review")
	}
	readCrossReviews(&ticket, []task.Comment{line(1, "id")})
	if !ticket.CrossReviewed["ctx_1"] {
		t.Fatal("own record ignored")
	}
}

func TestCrossReviewDecisionNeedsBothGreen(t *testing.T) {
	green := crossReviewOpinion{Reviewer: "a", Verdict: "green"}
	for _, tc := range []struct {
		name     string
		opinions []crossReviewOpinion
		want     bool
	}{
		{"both green", []crossReviewOpinion{green, green}, true},
		{"one red", []crossReviewOpinion{green, {Reviewer: "b", Verdict: "red"}}, false},
		{"one unavailable", []crossReviewOpinion{green, {Reviewer: "b", Verdict: crossReviewUnavailable}}, false},
		{"new authority", []crossReviewOpinion{green, {Reviewer: "b", Verdict: "green", ExpandsAuthority: true}}, false},
		{"one opinion", []crossReviewOpinion{green}, false},
	} {
		if got, reason := crossReviewDecision(nil, tc.opinions); got != tc.want || (!got && reason == "") {
			t.Errorf("%s: got=%v reason=%q", tc.name, got, reason)
		}
	}
	if got, _ := crossReviewDecision([]string{"lane3/key"}, []crossReviewOpinion{green, green}); got {
		t.Error("forbidden path released")
	}
}

func TestCrossReviewCloseCancelsRunningReviewer(t *testing.T) {
	f := newCrossReviewFixture(t, "scope-extension", "src/fix.go")
	if err := os.WriteFile(f.first, []byte("#!/bin/sh\necho run >> "+f.first+".count\nexec sleep 300\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := f.scheduler()
	_ = s.Tick(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(f.first + ".count"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reviewer did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := f.runtime.Close(20 * time.Second); err != nil {
		t.Fatal(err)
	}
	if got := f.records(t); len(got) != 0 {
		t.Fatalf("canceled review wrote a record: %+v", got)
	}
	worktrees := gitMergeTest(t, f.repo.Path, "worktree", "list", "--porcelain")
	if strings.Count(worktrees, "worktree ") != 1 {
		t.Fatalf("worktree left behind:\n%s", worktrees)
	}
}
