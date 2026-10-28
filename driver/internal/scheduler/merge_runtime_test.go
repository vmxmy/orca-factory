package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type mergeTransport struct{ url string }

func (m mergeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	c.URL.Scheme = "http"
	c.URL.Host = strings.TrimPrefix(m.url, "http://")
	return (&http.Transport{}).RoundTrip(c)
}

type mergeIssues struct {
	issues.Store
	issue     task.Issue
	rows      []task.Comment
	bodies    []string
	failDone  bool
	failFinal bool
	// failBody makes every comment containing it fail to write.
	failBody string
	// labels applies UpdateIssue label edits, as a claim confirmation needs.
	labels bool
}

func (m *mergeIssues) Snapshot(context.Context, []string, time.Time) ([]task.Issue, error) {
	return []task.Issue{m.issue}, nil
}
func (m *mergeIssues) Repository() string { return "o/r" }
func (m *mergeIssues) Read(context.Context, string) (issues.Task, error) {
	return issues.Task{Issue: m.issue, URL: "https://tickets.example/" + m.issue.ID, Complete: true, NativeState: "open", Metadata: issues.Metadata{Complete: true}}, nil
}
func (m *mergeIssues) ListComments(context.Context, string) ([]task.Comment, error) {
	return m.rows, nil
}
func (m *mergeIssues) CreateComment(_ context.Context, _ string, b string) (task.Comment, error) {
	if (m.failFinal && strings.Contains(b, "归属: 无")) || (m.failBody != "" && strings.Contains(b, m.failBody)) {
		return task.Comment{}, fmt.Errorf("offline")
	}
	c := task.Comment{ID: fmt.Sprint(len(m.rows) + 1), Body: b, CreatedAt: time.Now()}
	m.rows = append(m.rows, c)
	m.bodies = append(m.bodies, b)
	return c, nil
}
func (m *mergeIssues) FreshIssue(context.Context, string) (task.Issue, error) {
	return m.issue, nil
}
func (m *mergeIssues) UpdateIssue(_ context.Context, _ string, state string, add []string, remove []string) (task.Issue, error) {
	if m.failDone {
		return task.Issue{}, fmt.Errorf("offline")
	}
	if state != "" {
		m.issue.State.Name = state
	}
	if m.labels {
		var kept []task.NamedID
		for _, l := range m.issue.Labels.Nodes {
			if !slices.Contains(remove, l.Name) {
				kept = append(kept, l)
			}
		}
		for _, name := range add {
			kept = append(kept, task.NamedID{Name: name})
		}
		m.issue.Labels.Nodes = kept
	}
	return m.issue, nil
}
func gitMergeTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %s %v", args, b, e)
	}
	return strings.TrimSpace(string(b))
}

type mergeFixture struct {
	loseReadback bool
	duplicatePR  bool
	checksBody   string
	statusesBody string
	mainReads    int
	moveMain     bool
	requests     []string
	runtime      *Runtime
	config       config.Config
	repos        []config.Repository
	tracker      *mergeIssues
	head, main   string
	puts         int
	merged       bool
	checkState   string
	prBody       map[string]any
	server       *httptest.Server
	// rejectPuts makes that many squash PUTs fail without merging.
	rejectPuts int
}

func newMergeFixture(t *testing.T) *mergeFixture {
	t.Helper()
	dir := t.TempDir()
	gitMergeTest(t, dir, "init", "-b", "main")
	gitMergeTest(t, dir, "config", "user.email", "test@example.test")
	gitMergeTest(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "base"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	// A behind head runs the merged-result gate from main.
	if err := os.MkdirAll(filepath.Join(dir, "scripts/agent-gates"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts/agent-gates/gate.sh"), []byte("printf 'UNIT executor\\nVERDICT green\\nSTEP all\\nREASON passed\\nEVIDENCE test\\nSCOPE feature\\nLANE 1\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	gitMergeTest(t, dir, "add", ".")
	gitMergeTest(t, dir, "commit", "-m", "base")
	main := gitMergeTest(t, dir, "rev-parse", "HEAD")
	gitMergeTest(t, dir, "checkout", "-b", "agent/ENG-1/executor")
	if err := os.WriteFile(filepath.Join(dir, "feature"), []byte("feature"), 0600); err != nil {
		t.Fatal(err)
	}
	gitMergeTest(t, dir, "add", ".")
	gitMergeTest(t, dir, "commit", "-m", "feature")
	head := gitMergeTest(t, dir, "rev-parse", "HEAD")
	gitMergeTest(t, dir, "remote", "add", "origin", dir)
	f := &mergeFixture{head: head, main: main, checkState: "success"}
	f.prBody = map[string]any{"number": 1, "state": "open", "draft": false, "merged": false, "mergeable": true, "mergeable_state": "clean", "head": map[string]any{"sha": head, "ref": "agent/ENG-1/executor", "repo": map[string]any{"full_name": "o/r"}}, "base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "o/r"}}, "body": "Fixes ENG-1", "merge_commit_sha": strings.Repeat("a", 40)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/1/merge"):
			var b map[string]string
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
				t.Error(err)
				return
			}
			if r.Method != "PUT" || b["sha"] != f.head || b["merge_method"] != "squash" {
				t.Errorf("unsafe merge %s %v", r.Method, b)
			}
			f.puts++
			if f.rejectPuts > 0 {
				f.rejectPuts--
				w.WriteHeader(409)
				return
			}
			f.merged = true
			w.WriteHeader(502)
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			if f.merged && f.loseReadback {
				w.WriteHeader(502)
				return
			}
			f.prBody["merged"] = f.merged
			if err := json.NewEncoder(w).Encode(f.prBody); err != nil {
				t.Error(err)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
			f.mainReads++
			main := f.main
			if f.moveMain && f.mainReads >= 3 {
				main = strings.Repeat("c", 40)
			}
			if _, err := fmt.Fprintf(w, `{"object":{"sha":%q}}`, main); err != nil {
				t.Error(err)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			if f.checksBody != "" {
				if _, err := fmt.Fprint(w, f.checksBody); err != nil {
					t.Error(err)
					return
				}
				return
			}
			if _, err := fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":1,"name":"ci","status":"completed","conclusion":%q,"head_sha":%q}]}`, f.checkState, f.head); err != nil {
				t.Error(err)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/status"):
			if f.statusesBody != "" {
				if _, err := fmt.Fprint(w, f.statusesBody); err != nil {
					t.Error(err)
					return
				}
				return
			}
			if _, err := fmt.Fprint(w, `{"total_count":0,"statuses":[],"state":"pending"}`); err != nil {
				t.Error(err)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			if f.duplicatePR {
				if err := json.NewEncoder(w).Encode([]any{f.prBody, f.prBody}); err != nil {
					t.Error(err)
					return
				}
			} else if f.merged {
				if err := json.NewEncoder(w).Encode([]any{}); err != nil {
					t.Error(err)
					return
				}
			} else {
				if err := json.NewEncoder(w).Encode([]any{f.prBody}); err != nil {
					t.Error(err)
					return
				}
			}
		default:
			t.Errorf("unexpected HTTP %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	old := infra.HTTPTransport
	infra.HTTPTransport = mergeTransport{f.server.URL}
	t.Cleanup(func() { infra.HTTPTransport = old })
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf test-token"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	evidence := map[string]any{"version": 1, "ticket_id": "id", "identifier": "ENG-1", "repository": "o/r", "unit": "executor", "pr": 1, "head": head, "baseline": main, "gate_spec": "UNIT: executor\nSCOPE: feature\nCHECK: true\nLANE: 1\n", "gate_output": "UNIT executor\nVERDICT green\nSTEP all\nREASON passed\nEVIDENCE test\nSCOPE feature\nLANE 1", "review_output": `{"verdict":"green","step":"review"}`, "lane": 1, "run": "run_1", "task": "task_1", "dispatch": "dispatch_1", "worktree": dir, "attempt": 1, "enqueued_at": "2026-10-06T01:00:00Z", "queue_id": "q1"}
	b, bMarshalErr := json.Marshal(evidence)
	if bMarshalErr != nil {
		t.Fatal(bMarshalErr)
	}
	f.tracker = &mergeIssues{issue: task.Issue{ID: "id", Identifier: "ENG-1", Project: task.NamedID{ID: "o/r"}, State: task.NamedID{Name: "In Progress"}}, rows: []task.Comment{{ID: "handoff", CreatedAt: time.Now(), Body: "阶段: 排队合并\n归属: 合并通道\nmerge-handoff-v1: " + string(b)}}}
	repo := config.Repository{Path: dir, StaleHours: 8, MaxInFlight: 3, Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "o/r", "AGENT_SCOPE_AVOID": "0"}}
	f.repos = []config.Repository{repo}
	f.config = config.Config{MaxInFlight: 3, TokenBucket: config.TokenBucket{Capacity: 3, RefillPerSecond: 1}}
	f.runtime = &Runtime{Clients: map[string]issues.Store{dir: f.tracker}, Repositories: map[string]config.Repository{dir: repo}, Orca: &fakeOrca{automations: receipt(`{"automations":[]}`)}}
	return f
}
func TestRuntimeMergeCompletesAndRecoversUncertainSquash(t *testing.T) {
	f := newMergeFixture(t)
	s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.puts != 1 || !f.merged || f.tracker.issue.State.Name != "Done" {
		t.Fatalf("merge not completed: squash=%d merged=%v state=%s", f.puts, f.merged, f.tracker.issue.State.Name)
	}
	if !strings.Contains(strings.Join(f.tracker.bodies, "\n"), "归属: 无") {
		t.Fatalf("ownership not released: %v", f.tracker.bodies)
	}
}

func rewriteHandoff(t *testing.T, f *mergeFixture, edit func(map[string]any)) {
	t.Helper()
	parts := strings.Split(f.tracker.rows[0].Body, "merge-handoff-v1: ")
	var h map[string]any
	if err := json.Unmarshal([]byte(parts[1]), &h); err != nil {
		t.Fatal(err)
	}
	edit(h)
	b, bMarshalErr := json.Marshal(h)
	if bMarshalErr != nil {
		t.Fatal(bMarshalErr)
	}
	f.tracker.rows[0].Body = parts[0] + "merge-handoff-v1: " + string(b)
}
func TestRuntimeMergeRequiresValidationAndPolicy(t *testing.T) {
	// AGENT_LANE2_MANUAL_GLOBS and AGENT_LANE3_GLOBS no longer leave the
	// ticket Waiting with a generic policy error: TestRuntimeMergeManualGlobReviewLeavesQueue
	// and TestRuntimeMergeLane3EscalatesDirectlyToHuman assert their dedicated
	// escalation receipts and dequeue (OF-103).
	for _, kind := range []string{"gate-red", "review-red", "wrong-spec", "missing-orca", "multiple-pr"} {
		t.Run(kind, func(t *testing.T) {
			f := newMergeFixture(t)
			switch kind {
			case "gate-red":
				rewriteHandoff(t, f, func(h map[string]any) {
					h["gate_output"] = strings.ReplaceAll(h["gate_output"].(string), "VERDICT green", "VERDICT red")
				})
			case "review-red":
				rewriteHandoff(t, f, func(h map[string]any) { h["review_output"] = `{"verdict":"red","step":"review"}` })
			case "wrong-spec":
				rewriteHandoff(t, f, func(h map[string]any) { h["gate_spec"] = "UNIT: unrelated\nCHECK: true\nSCOPE: other\nLANE: 1" })
			case "missing-orca":
				rewriteHandoff(t, f, func(h map[string]any) { h["task"] = "" })
			case "multiple-pr":
				f.duplicatePR = true
			}
			s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
			err := s.Tick(context.Background())
			if f.puts != 0 || f.tracker.issue.State.Name != "In Progress" {
				t.Fatalf("unsafe merge %s: puts=%d state=%s", kind, f.puts, f.tracker.issue.State.Name)
			}
			if err == nil {
				t.Fatal("waiting must explain missing evidence")
			}
		})
	}
}

func TestRuntimeMergeRetainsOwnershipUntilCompletionConfirmed(t *testing.T) {
	for _, failure := range []string{"done", "final-comment"} {
		t.Run(failure, func(t *testing.T) {
			f := newMergeFixture(t)
			f.tracker.failDone = failure == "done"
			f.tracker.failFinal = failure == "final-comment"
			s := &Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}
			if err := s.Tick(context.Background()); err == nil {
				t.Fatal("expected unconfirmed completion")
			}
			if f.puts != 1 {
				t.Fatalf("squash=%d", f.puts)
			}
			f.tracker.failDone = false
			f.tracker.failFinal = false
			rebuilt := *f.runtime
			rebuilt.GitHub = nil
			s = &Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.puts != 1 || f.tracker.issue.State.Name != "Done" || !strings.Contains(strings.Join(f.tracker.bodies, "\n"), "归属: 无") {
				t.Fatalf("completion lost/replayed: puts=%d state=%s comments=%v", f.puts, f.tracker.issue.State.Name, f.tracker.bodies)
			}
		})
	}
}

func TestRuntimeMergeMissingDraftIsUnknown(t *testing.T) {
	f := newMergeFixture(t)
	delete(f.prBody, "draft")
	err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	if err == nil || f.puts != 0 {
		t.Fatalf("missing draft was accepted: err=%v puts=%d", err, f.puts)
	}
}

func TestRuntimeMergeConservativeEvidence(t *testing.T) {
	for _, kind := range []string{"empty-ci", "unknown-ci", "wrong-ci-head", "missing-ci-count", "truncated-ci", "status-red", "read-failure", "moving-main", "invalid-deployment", "invalid-smoke", "changed-head", "missing-handoff", "malformed-handoff", "missing-main", "unknown-mergeability"} {
		t.Run(kind, func(t *testing.T) {
			f := newMergeFixture(t)
			switch kind {
			case "empty-ci":
				f.checksBody = `{"total_count":0,"check_runs":[]}`
			case "unknown-ci":
				f.checkState = "mystery"
			case "wrong-ci-head":
				f.checksBody = `{"total_count":1,"check_runs":[{"id":1,"status":"completed","conclusion":"success","head_sha":"old"}]}`
			case "missing-ci-count":
				f.checksBody = `{"check_runs":[]}`
			case "truncated-ci":
				f.checksBody = `{"total_count":1,"check_runs":[]}`
			case "status-red":
				f.statusesBody = `{"total_count":1,"statuses":[{"context":"build","state":"failure"}]}`
			case "read-failure":
				f.runtime.Run = func(context.Context, runx.Spec) ([]byte, error) { return nil, fmt.Errorf("git unavailable") }
			case "moving-main":
				f.moveMain = true
			case "invalid-deployment":
				f.repos[0].Env["AGENT_DEPLOY_WORKFLOW"] = " "
			case "invalid-smoke":
				f.repos[0].Env["AGENT_SMOKE_CMD"] = " "
			case "changed-head":
				f.prBody["head"].(map[string]any)["sha"] = strings.Repeat("f", 40)
			case "missing-handoff":
				f.tracker.rows[0].Body = "归属: 合并通道"
			case "malformed-handoff":
				f.tracker.rows[0].Body = "归属: 合并通道\nmerge-handoff-v1: {"
			case "missing-main":
				f.main = ""
			case "unknown-mergeability":
				delete(f.prBody, "mergeable")
			}
			err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
			if err == nil || f.puts != 0 || f.tracker.issue.State.Name != "In Progress" {
				t.Fatalf("unsafe evidence %s: err=%v puts=%d state=%s", kind, err, f.puts, f.tracker.issue.State.Name)
			}
		})
	}
}

func TestRuntimeMergeRecoveryPreservesFixedMainReceipt(t *testing.T) {
	f := newMergeFixture(t)
	f.tracker.failDone = true
	_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	f.tracker.failDone = false
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, body := range f.tracker.bodies {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "merge-progress-v1: ") {
				var p map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "merge-progress-v1: ")), &p); err != nil {
					t.Fatal(err)
				}
				if p["main"] != f.main {
					t.Fatalf("recovery lost fixed main: %s", line)
				}
			}
		}
	}
}

// lastSnapshotReason returns the most recent merge-progress-v1 reason written
// to the ticket, the artifact the merge channel and the scheduler log share.
func lastSnapshotReason(t *testing.T, f *mergeFixture) string {
	t.Helper()
	for i := len(f.tracker.bodies) - 1; i >= 0; i-- {
		for _, line := range strings.Split(f.tracker.bodies[i], "\n") {
			if strings.HasPrefix(line, "merge-progress-v1: ") {
				var p struct {
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "merge-progress-v1: ")), &p); err != nil {
					t.Fatal(err)
				}
				return p.Reason
			}
		}
	}
	t.Fatal("no merge-progress-v1 written")
	return ""
}

// Each of ZCP-70's seven ambiguous Snapshot failure steps must produce a
// distinguishable, credential-free reason in both the scheduler error and the
// persisted merge-progress-v1, and the ticket must stay Waiting (no status
// change), not escalate to a hard, unexplained scheduler error.
func TestRuntimeMergeSnapshotReasonNamesFailureStep(t *testing.T) {
	cases := []struct {
		kind      string
		step      string
		configure func(f *mergeFixture)
	}{
		{"pr", "pr", func(f *mergeFixture) { delete(f.prBody, "mergeable") }},
		{"duplicate-pr", "duplicate-pr", func(f *mergeFixture) { f.duplicatePR = true }},
		{"main", "main", func(f *mergeFixture) { f.main = "" }},
		{"fetch", "fetch", func(f *mergeFixture) {
			f.runtime.Run = func(context.Context, runx.Spec) ([]byte, error) {
				return nil, fmt.Errorf("fatal: unable to access remote: TEST-PLACEHOLDER-CRED")
			}
		}},
		{"merge-base", "merge-base", func(f *mergeFixture) {
			f.runtime.Run = func(ctx context.Context, spec runx.Spec) ([]byte, error) {
				if len(spec.Args) > 0 && spec.Args[0] == "merge-base" {
					return nil, fmt.Errorf("fatal: TEST-PLACEHOLDER-CRED not a valid object name")
				}
				return runx.Run(ctx, spec)
			}
		}},
		// AGENT_LANE2_GLOBS/AGENT_LANE2_MANUAL_GLOBS/AGENT_LANE3_GLOBS matches
		// now return their own sentinels (OF-102, OF-103, OF-104) and never
		// reach this generic reason path; an invalid glob pattern still does.
		{"policy", "policy", func(f *mergeFixture) { f.repos[0].Env["AGENT_LANE2_GLOBS"] = "[" }},
		{"checks", "checks", func(f *mergeFixture) { f.checksBody = "{not-json" }},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			f := newMergeFixture(t)
			tc.configure(f)
			err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
			if err == nil || f.puts != 0 || f.tracker.issue.State.Name != "In Progress" {
				t.Fatalf("expected a waiting snapshot failure: err=%v puts=%d state=%s", err, f.puts, f.tracker.issue.State.Name)
			}
			reason := lastSnapshotReason(t, f)
			want := "merge snapshot unavailable: " + tc.step + ": "
			if !strings.HasPrefix(reason, want) {
				t.Fatalf("reason=%q want prefix %q", reason, want)
			}
			if strings.Contains(err.Error(), "TEST-PLACEHOLDER-CRED") || strings.Contains(reason, "TEST-PLACEHOLDER-CRED") {
				t.Fatalf("credential leaked: err=%v reason=%q", err, reason)
			}
			if seen[reason] {
				t.Fatalf("reason not distinguishable from an earlier step: %q", reason)
			}
			seen[reason] = true
		})
	}
}

type mergeTickets struct {
	issues.Store
	rows []*mergeIssues
}

func (m *mergeTickets) Snapshot(context.Context, []string, time.Time) ([]task.Issue, error) {
	var issues []task.Issue
	for _, r := range m.rows {
		issues = append(issues, r.issue)
	}
	return issues, nil
}
func (m *mergeTickets) row(id string) *mergeIssues {
	for _, r := range m.rows {
		if r.issue.ID == id {
			return r
		}
	}
	panic("unknown issue " + id)
}
func (m *mergeTickets) FreshIssue(ctx context.Context, id string) (task.Issue, error) {
	return m.row(id).FreshIssue(ctx, id)
}
func (m *mergeTickets) Read(ctx context.Context, id string) (issues.Task, error) {
	return m.row(id).Read(ctx, id)
}
func (m *mergeTickets) ListComments(ctx context.Context, id string) ([]task.Comment, error) {
	return m.row(id).ListComments(ctx, id)
}
func (m *mergeTickets) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	return m.row(id).CreateComment(ctx, id, body)
}
func (m *mergeTickets) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (task.Issue, error) {
	return m.row(id).UpdateIssue(ctx, id, state, add, remove)
}

func TestRuntimeMergeFIFOIsFrozenAcrossRestarts(t *testing.T) {
	f := newMergeFixture(t)
	next := *f.tracker
	next.issue.ID = "next"
	next.issue.Identifier = "ENG-2"
	next.rows = append([]task.Comment(nil), f.tracker.rows...)
	next.issue.UpdatedAt = time.Now().Add(-48 * time.Hour)
	f.tracker.issue.UpdatedAt = time.Now()
	nf := *f
	nf.tracker = &next
	rewriteHandoff(t, &nf, func(h map[string]any) {
		h["ticket_id"] = "next"
		h["identifier"] = "ENG-2"
		h["pr"] = 2
		h["queue_id"] = "q2"
		h["enqueued_at"] = "2026-10-06T02:00:00Z"
	})
	source := &mergeTickets{rows: []*mergeIssues{&next, f.tracker}}
	f.runtime.Clients[f.repos[0].Path] = source
	// Waiting head blocks the later PR, even when snapshot and updatedAt favor it.
	f.checkState = "pending"
	for i := 0; i < 2; i++ {
		rebuilt := *f.runtime
		rebuilt.GitHub = nil
		err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background())
		if err == nil || f.puts != 0 {
			t.Fatalf("FIFO wait lost on restart: %v puts=%d", err, f.puts)
		}
	}
	f.checkState = "success"
	f.tracker.failDone = true
	_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	f.tracker.failDone = false
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.puts != 1 || f.tracker.issue.State.Name != "Done" || next.issue.State.Name != "In Progress" {
		t.Fatalf("FIFO completion: puts=%d first=%s next=%s", f.puts, f.tracker.issue.State.Name, next.issue.State.Name)
	}
	for _, request := range f.requests {
		if strings.HasSuffix(request, "/pulls/2") {
			t.Fatal("later PR overtook retained head")
		}
	}
}

func TestRuntimeMergeBlockedRepositoryDoesNotBlockOtherRepository(t *testing.T) {
	f := newMergeFixture(t)
	f.checkState = "pending"
	b := *f.tracker
	b.issue.ID = "b"
	b.issue.Project.ID = "o/s"
	b.rows = append([]task.Comment(nil), f.tracker.rows...)
	bf := *f
	bf.tracker = &b
	rewriteHandoff(t, &bf, func(h map[string]any) { h["repository"] = "o/s"; h["ticket_id"] = "b"; h["queue_id"] = "b" })
	dir := t.TempDir()
	gitMergeTest(t, dir, "clone", f.repos[0].Path, ".")
	br := config.Repository{Path: dir, StaleHours: 8, MaxInFlight: 3, Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "o/s", "AGENT_SCOPE_AVOID": "0"}}
	f.repos = append(f.repos, br)
	f.runtime.Repositories[dir] = br
	source := &mergeTickets{rows: []*mergeIssues{f.tracker, &b}}
	f.runtime.Clients[dir] = source
	f.runtime.Clients[f.repos[0].Path] = source
	original := f.server.Config.Handler
	bMerged := false
	bPuts := 0
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/o/s/") {
			original.ServeHTTP(w, r)
			return
		}
		raw, rawMarshalErr := json.Marshal(f.prBody)
		if rawMarshalErr != nil {
			t.Error(rawMarshalErr)
			return
		}
		var pr map[string]any
		if err := json.Unmarshal(raw, &pr); err != nil {
			t.Error(err)
			return
		}
		pr["head"].(map[string]any)["repo"] = map[string]any{"full_name": "o/s"}
		pr["base"].(map[string]any)["repo"] = map[string]any{"full_name": "o/s"}
		pr["merged"] = bMerged
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/1/merge"):
			bPuts++
			bMerged = true
			if _, err := fmt.Fprint(w, `{}`); err != nil {
				t.Error(err)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			if err := json.NewEncoder(w).Encode(pr); err != nil {
				t.Error(err)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			if bMerged {
				if err := json.NewEncoder(w).Encode([]any{}); err != nil {
					t.Error(err)
					return
				}
			} else {
				if err := json.NewEncoder(w).Encode([]any{pr}); err != nil {
					t.Error(err)
					return
				}
			}
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			if _, err := fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":1,"status":"completed","conclusion":"success","head_sha":%q}]}`, f.head); err != nil {
				t.Error(err)
				return
			}
		default:
			original.ServeHTTP(w, r)
		}
	})
	err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	if err == nil || f.puts != 0 || bPuts != 1 || b.issue.State.Name != "Done" || f.tracker.issue.State.Name != "In Progress" {
		t.Fatalf("repository isolation failed: err=%v a=%d b=%d state=%s", err, f.puts, bPuts, b.issue.State.Name)
	}
}

func TestRuntimeMergeObservedWithoutIntentSurvivesCompletionFailure(t *testing.T) {
	f := newMergeFixture(t)
	f.merged = true
	f.tracker.failDone = true
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background()); err == nil {
		t.Fatal("expected Done failure")
	}
	f.tracker.failDone = false
	for i := 0; i < 2; i++ {
		rebuilt := *f.runtime
		rebuilt.GitHub = nil
		if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err != nil {
			t.Fatalf("observed merged recovery stuck: %v", err)
		}
	}
	if f.puts != 0 || f.tracker.issue.State.Name != "Done" {
		t.Fatalf("replayed/unfinished: puts=%d state=%s", f.puts, f.tracker.issue.State.Name)
	}
}
func TestRuntimeMergeLostResponseAndReadbackRetainsIntent(t *testing.T) {
	f := newMergeFixture(t)
	f.loseReadback = true
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background()); err == nil {
		t.Fatal("expected readback failure")
	}
	if f.puts != 1 || !strings.Contains(strings.Join(f.tracker.bodies, "\n"), `"phase":"merge-intent"`) {
		t.Fatalf("missing durable pre-write intent: puts=%d bodies=%v", f.puts, f.tracker.bodies)
	}
	f.loseReadback = false
	f.tracker.failDone = true
	_ = (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	f.tracker.failDone = false
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.puts != 1 || f.tracker.issue.State.Name != "Done" {
		t.Fatalf("uncertain write replayed: puts=%d state=%s", f.puts, f.tracker.issue.State.Name)
	}
}

func TestRuntimeMergeUsesGateLaneRatherThanBriefEstimate(t *testing.T) {
	f := newMergeFixture(t)
	// Executor briefs default to lane 2; gate computes the actual lane from paths.
	rewriteHandoff(t, f, func(h map[string]any) {
		h["gate_spec"] = strings.ReplaceAll(h["gate_spec"].(string), "LANE: 1", "LANE: 2")
	})
	err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	if err != nil || f.puts != 1 {
		t.Fatalf("gate lane 1 was overridden by brief estimate: err=%v puts=%d", err, f.puts)
	}
}

func TestRuntimeMergeRejectsRepeatedCIPages(t *testing.T) {
	f := newMergeFixture(t)
	f.checksBody = fmt.Sprintf(`{"total_count":2,"check_runs":[{"id":1,"status":"completed","conclusion":"success","head_sha":%q}]}`, f.head)
	err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	if err == nil || f.puts != 0 {
		t.Fatalf("repeated pages treated as complete: %v puts=%d", err, f.puts)
	}
}
func TestRuntimeMergeIncompleteHandoffAndUnknownProgressWait(t *testing.T) {
	for _, kind := range []string{"worktree", "budget", "progress"} {
		t.Run(kind, func(t *testing.T) {
			f := newMergeFixture(t)
			switch kind {
			case "worktree":
				rewriteHandoff(t, f, func(h map[string]any) { delete(h, "worktree") })
			case "budget":
				rewriteHandoff(t, f, func(h map[string]any) { h["corrections"] = -1 })
			case "progress":
				f.merged = true
				raw := fmt.Sprintf(`{"version":1,"queue_id":"q1","ticket_id":"id","repository":"o/r","pr":1,"head":%q,"main":%q,"merge_sha":%q,"phase":"unknown"}`, f.head, f.main, strings.Repeat("a", 40))
				f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "progress", CreatedAt: time.Now(), Body: "merge-progress-v1: " + raw})
			}
			err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
			if err == nil || f.puts != 0 || f.tracker.issue.State.Name != "In Progress" {
				t.Fatalf("invalid evidence accepted: %v puts=%d state=%s", err, f.puts, f.tracker.issue.State.Name)
			}
		})
	}
}

func TestRuntimeMergedPRWithAnotherCandidateDoesNotCompleteTicket(t *testing.T) {
	f := newMergeFixture(t)
	f.merged = true
	f.duplicatePR = true
	err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background())
	if err == nil || f.tracker.issue.State.Name != "In Progress" {
		t.Fatalf("ambiguous ticket completed after first PR: %v state=%s", err, f.tracker.issue.State.Name)
	}
}

func TestRuntimeMergeCanceledTicketIsNotAdvanced(t *testing.T) {
	f := newMergeFixture(t)
	f.tracker.issue.State.Name = "Canceled"
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.puts != 0 || f.tracker.issue.State.Name != "Canceled" {
		t.Fatalf("canceled ticket advanced: puts=%d state=%s", f.puts, f.tracker.issue.State.Name)
	}
}
