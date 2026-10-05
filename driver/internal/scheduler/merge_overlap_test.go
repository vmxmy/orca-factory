package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

// Both sides edit the same file; distant lines merge cleanly. Gate is a command
// boundary substitute, while merge-tree, commit-tree and worktrees are real Git.
func overlapFixture(t *testing.T, conflict bool) *mergeFixture {
	t.Helper()
	f := newMergeFixture(t)
	dir := f.config.Repositories[0].Path
	gitMergeTest(t, dir, "checkout", "main")
	text := "first\n" + strings.Repeat("middle\n", 20) + "last\n"
	os.WriteFile(filepath.Join(dir, "shared"), []byte(text), 0600)
	os.MkdirAll(filepath.Join(dir, "scripts/agent-gates"), 0700)
	os.WriteFile(filepath.Join(dir, "scripts/agent-gates/gate.sh"), []byte("#!/bin/sh\nexit 2\n"), 0700)
	gitMergeTest(t, dir, "add", ".")
	gitMergeTest(t, dir, "commit", "-m", "common")
	base := gitMergeTest(t, dir, "rev-parse", "HEAD")
	gitMergeTest(t, dir, "checkout", "agent/ENG-1/executor")
	gitMergeTest(t, dir, "merge", "--no-edit", "main")
	os.WriteFile(filepath.Join(dir, "shared"), []byte(strings.Replace(text, "first", "worker", 1)), 0600)
	gitMergeTest(t, dir, "add", ".")
	gitMergeTest(t, dir, "commit", "-m", "worker")
	f.head = gitMergeTest(t, dir, "rev-parse", "HEAD")
	gitMergeTest(t, dir, "checkout", "main")
	from := "last"
	if conflict {
		from = "first"
	}
	os.WriteFile(filepath.Join(dir, "shared"), []byte(strings.Replace(text, from, "main", 1)), 0600)
	gitMergeTest(t, dir, "add", ".")
	gitMergeTest(t, dir, "commit", "-m", "advance")
	f.main = gitMergeTest(t, dir, "rev-parse", "HEAD")
	f.prBody["head"].(map[string]any)["sha"] = f.head
	if conflict {
		f.prBody["mergeable"] = false
		f.prBody["mergeable_state"] = "dirty"
	}
	rewriteHandoff(t, f, func(h map[string]any) {
		h["head"] = f.head
		h["baseline"] = base
		h["gate_spec"] = "UNIT: executor\nSCOPE: feature shared\nCHECK: test merged\nLANE: 1\n"
		h["gate_output"] = "UNIT executor\nVERDICT green\nSTEP all\nREASON passed\nEVIDENCE test\nSCOPE feature shared\nLANE 1"
	})
	f.runtime.DriverPath = "/test/driver"
	return f
}

func TestRuntimeTextConflictStartsCorrection(t *testing.T) {
	f := overlapFixture(t, true)
	if err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	o := f.runtime.Orca.(*fakeOrca)
	var p merge.Progress
	for _, row := range f.linear.rows {
		for _, line := range strings.Split(row.Body, "\n") {
			if strings.HasPrefix(line, merge.ProgressPrefix) {
				p = merge.Progress{}
				json.Unmarshal([]byte(strings.TrimPrefix(line, merge.ProgressPrefix)), &p)
			}
		}
	}
	if f.puts != 0 || len(o.launches) != 1 || p.Phase != "correction" || p.Corrections != 1 || p.CatchupRounds != 1 || p.Head != f.head || p.Main != f.main || !strings.Contains(strings.Join(f.linear.bodies, "\n"), "归属: 执行器") {
		t.Fatalf("not repaired: puts=%d launch=%v receipt=%+v", f.puts, o.launches, p)
	}
	// New scheduler sees executor ownership, not the old merge handoff.
	tickets, err := f.runtime.Snapshot(context.Background(), f.config.Repositories)
	if err != nil || len(tickets) != 1 || string(tickets[0].Owner) != "执行器" {
		t.Fatalf("lost ownership: %+v %v", tickets, err)
	}
}

func TestRuntimeOverlapRunsFrozenGateOnMergedTree(t *testing.T) {
	for _, verdict := range []string{"green", "red", "infra"} {
		t.Run(verdict, func(t *testing.T) {
			f := overlapFixture(t, false)
			dir := f.config.Repositories[0].Path
			calls := 0
			f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if s.Path != "bash" {
					return runx.Run(ctx, s)
				}
				calls++
				if len(s.Args) != 3 {
					t.Fatalf("not full gate: %+v", s)
				}
				wt := s.Args[1]
				data, err := os.ReadFile(filepath.Join(wt, "shared"))
				if err != nil || !strings.HasPrefix(string(data), "worker\n") || !strings.HasSuffix(string(data), "main\n") {
					t.Fatalf("not merged result: %s %v", data, err)
				}
				parents := gitMergeTest(t, wt, "show", "-s", "--format=%P", "HEAD")
				if parents != f.head+" "+f.main {
					t.Fatalf("unpinned parents: %s", parents)
				}
				spec, err := os.ReadFile(s.Args[2])
				if err != nil || string(spec) != "UNIT: executor\nSCOPE: feature shared\nCHECK: test merged\nLANE: 1\n" {
					t.Fatalf("lost spec: %s %v", spec, err)
				}
				if !strings.Contains(strings.Join(s.Env, "\n"), "GATE_BASE="+f.main+"\n") || !strings.Contains(strings.Join(s.Env, "\n"), "GATE_PROJECT_SRC="+f.main) {
					t.Fatalf("mutable gate refs: %v", s.Env)
				}
				if verdict == "infra" {
					return nil, fmt.Errorf("cannot launch gate")
				}
				output := []byte("UNIT executor\nVERDICT " + verdict + "\nSTEP all\nREASON checked\nEVIDENCE test\nSCOPE feature shared\nLANE 1")
				if verdict == "red" {
					return output, gateExit{}
				}
				return output, nil
			}
			err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
			if calls != 1 {
				t.Fatalf("gate not run: calls=%d err=%v", calls, err)
			}
			if verdict == "green" && (err != nil || f.puts != 1 || f.linear.issue.State.Name != "Done") {
				t.Fatalf("green not merged: %v puts=%d", err, f.puts)
			}
			if verdict == "red" && (err != nil || f.puts != 0 || len(f.runtime.Orca.(*fakeOrca).launches) != 1) {
				t.Fatalf("red not corrected: %v puts=%d", err, f.puts)
			}
			if verdict == "infra" && (err == nil || f.puts != 0 || len(f.runtime.Orca.(*fakeOrca).launches) != 0 || strings.Contains(strings.Join(f.linear.bodies, "\n"), `"phase":"correction"`)) {
				t.Fatalf("infra charged: %v %v", err, f.linear.bodies)
			}
			if got := gitMergeTest(t, dir, "rev-parse", "agent/ENG-1/executor"); got != f.head {
				t.Fatal("worker branch rewritten")
			}
			if got := gitMergeTest(t, dir, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
				t.Fatalf("leaked worktree: %s", got)
			}
			matches, _ := filepath.Glob(filepath.Join(dir, ".merge-validation-*"))
			if len(matches) != 0 {
				t.Fatalf("leaked temp: %v", matches)
			}
		})
	}
}

type gateExit struct{}

func (gateExit) Error() string { return "gate red" }
func (gateExit) ExitCode() int { return 1 }

func TestRuntimeOverlapCatchupSurvivesRestart(t *testing.T) {
	f := overlapFixture(t, false)
	dir := f.config.Repositories[0].Path
	calls := 0
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path != "bash" {
			return runx.Run(ctx, s)
		}
		calls++
		gitMergeTest(t, dir, "commit", "--allow-empty", "-m", fmt.Sprint("move-", calls))
		f.main = gitMergeTest(t, dir, "rev-parse", "HEAD")
		return []byte("UNIT executor\nVERDICT green\nSTEP all\nREASON checked\nEVIDENCE test\nSCOPE feature shared\nLANE 1"), nil
	}
	for round := 1; round <= 2; round++ {
		rebuilt := *f.runtime
		err := (&Scheduler{Config: f.config, Backend: &rebuilt}).Tick(context.Background())
		if round == 1 && err == nil {
			t.Fatal("first moving main must wait")
		}
		if round == 2 && err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || f.puts != 0 || !strings.Contains(strings.Join(f.linear.bodies, "\n"), "归属: 人") || !strings.Contains(strings.Join(f.linear.bodies, "\n"), `"catchup_rounds":2`) {
		t.Fatalf("budget lost: gates=%d puts=%d comments=%v", calls, f.puts, f.linear.bodies)
	}
}

func TestRuntimeMergeCorrectionCapsPersistedBudget(t *testing.T) {
	for _, limit := range []string{"corrections", "catchup"} {
		t.Run(limit, func(t *testing.T) {
			f := overlapFixture(t, true)
			rewriteHandoff(t, f, func(h map[string]any) {
				if limit == "corrections" {
					h["corrections"] = 3
				} else {
					h["catchup_rounds"] = 1
				}
			})
			if err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.runtime.Orca.(*fakeOrca).launches) != 0 || !strings.Contains(strings.Join(f.linear.bodies, "\n"), "归属: 人") {
				t.Fatalf("cap ignored: %v", f.linear.bodies)
			}
		})
	}
}

func TestRuntimeOverlapGreenCleanupFailureStillCompletes(t *testing.T) {
	f := overlapFixture(t, false)
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "bash" {
			return []byte("UNIT executor\nVERDICT green\nSTEP all\nREASON checked\nEVIDENCE test\nSCOPE feature shared\nLANE 1"), nil
		}
		if len(s.Args) > 1 && s.Args[0] == "worktree" && s.Args[1] == "remove" {
			return nil, fmt.Errorf("cleanup unavailable")
		}
		return runx.Run(ctx, s)
	}
	err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
	if !errors.Is(err, merge.ErrWorktreeCleanup) || f.puts != 1 || f.linear.issue.State.Name != "Done" || !strings.Contains(strings.Join(f.linear.bodies, "\n"), "归属: 无") {
		t.Fatalf("lost successful merge: err=%v puts=%d state=%s receipts=%v", err, f.puts, f.linear.issue.State.Name, f.linear.bodies)
	}
}

func TestRuntimeMergeExplicitUnmergeableCannotSquash(t *testing.T) {
	f := newMergeFixture(t)
	f.prBody["mergeable"] = false // Inconsistent GitHub fields must not become approval.
	err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
	if err == nil || f.puts != 0 {
		t.Fatalf("false mergeability accepted: %v puts=%d", err, f.puts)
	}
}

func TestRuntimeOverlapExecutesOriginalGateScript(t *testing.T) {
	f := overlapFixture(t, false)
	dir := f.config.Repositories[0].Path
	script, err := os.ReadFile("../../../scripts/agent-gates/gate.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "scripts/agent-gates/gate.sh"), script, 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".omc/\n.merge-validation-*/\n"), 0600)
	// The real gate's project hook and CHECK both inspect the merged contents.
	os.WriteFile(filepath.Join(dir, "scripts/agent-gates/gate.project.sh"), []byte("#!/bin/sh\nset -e\necho '== merged-project'\ntest \"$(head -1 shared)\" = worker\ntest \"$(tail -1 shared)\" = main\n"), 0700)
	gitMergeTest(t, dir, "add", ".")
	gitMergeTest(t, dir, "commit", "-m", "trusted gate")
	f.main = gitMergeTest(t, dir, "rev-parse", "HEAD")
	rewriteHandoff(t, f, func(h map[string]any) {
		h["gate_spec"] = "UNIT: executor\nSCOPE: feature shared\nCHECK: test \"$(head -1 shared)\" = worker && test \"$(tail -1 shared)\" = main\nCHECK_IS_NEW_TEST: no\nLANE: 1\n"
	})
	if err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.puts != 1 || f.linear.issue.State.Name != "Done" {
		t.Fatalf("original full gate did not pass: %v", f.linear.bodies)
	}
}

func TestRuntimeOverlapInfrastructureDoesNotCorrect(t *testing.T) {
	for _, failure := range []string{"merge-tree", "commit-tree", "worktree-add", "gate-script-read", "gate-cancel", "gate-malformed"} {
		t.Run(failure, func(t *testing.T) {
			f := overlapFixture(t, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if s.Path == "bash" {
					if failure == "gate-cancel" {
						cancel()
						return nil, context.Canceled
					}
					return []byte("VERDICT red\n"), gateExit{}
				}
				joined := strings.Join(s.Args, " ")
				if (failure == "merge-tree" && strings.HasPrefix(joined, "merge-tree ")) || (failure == "commit-tree" && strings.Contains(joined, "commit-tree ")) || (failure == "worktree-add" && strings.HasPrefix(joined, "worktree add ")) || (failure == "gate-script-read" && strings.HasPrefix(joined, "show ")) {
					return nil, fmt.Errorf("unavailable")
				}
				return runx.Run(ctx, s)
			}
			err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(ctx)
			if err == nil || f.puts != 0 || len(f.runtime.Orca.(*fakeOrca).launches) != 0 || len(f.linear.bodies) != 0 {
				t.Fatalf("infrastructure spent correction: %v receipts=%v", err, f.linear.bodies)
			}
			dir := f.config.Repositories[0].Path
			if got := gitMergeTest(t, dir, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
				t.Fatalf("leaked canceled worktree: %s", got)
			}
		})
	}
}

func TestRuntimeOverlapGateLaneRedReturnsCorrection(t *testing.T) {
	f := overlapFixture(t, false)
	f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "bash" {
			return []byte("UNIT executor\nVERDICT red\nSTEP lane\nREASON changed files are lane 3\nEVIDENCE shared\nSCOPE feature shared\nLANE 3"), gateExit{}
		}
		return runx.Run(ctx, s)
	}
	err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
	if err != nil || f.puts != 0 || len(f.runtime.Orca.(*fakeOrca).launches) != 1 {
		t.Fatalf("valid gate red ignored: %v", err)
	}
}

func TestRuntimeCorrectionRequeueGoesBehindExistingTicket(t *testing.T) {
	f := newMergeFixture(t)
	later := *f.linear
	later.issue.ID, later.issue.Identifier = "repaired", "ENG-2"
	later.rows = append([]linear.Comment(nil), f.linear.rows...)
	nf := *f
	nf.linear = &later
	rewriteHandoff(t, &nf, func(h map[string]any) {
		h["ticket_id"] = "repaired"
		h["identifier"] = "ENG-2"
		h["pr"] = 2
		h["queue_id"] = "old"
		h["enqueued_at"] = "2026-10-06T00:00:00Z"
	})
	old := later.rows[0]
	rewriteHandoff(t, &nf, func(h map[string]any) {
		h["queue_id"] = "new-validated-head"
		h["enqueued_at"] = "2026-10-06T03:00:00Z"
		h["corrections"] = 1
		h["catchup_rounds"] = 1
	})
	fresh := later.rows[0]
	fresh.CreatedAt = fresh.CreatedAt.Add(time.Minute)
	later.rows = []linear.Comment{old, {Body: "归属: 执行器", CreatedAt: old.CreatedAt.Add(time.Second)}, fresh}
	source := &mergeTickets{rows: []*mergeLinear{&later, f.linear}}
	f.runtime.Reader = source
	f.runtime.Clients[f.config.Repositories[0].Path] = source
	if err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.puts != 1 || f.linear.issue.State.Name != "Done" || later.issue.State.Name != "In Progress" {
		t.Fatalf("repaired PR overtook original: %d %+v", f.puts, later.issue)
	}
}

func TestRuntimeUnknownProgressWaitsWithoutOverwriting(t *testing.T) {
	f := newMergeFixture(t)
	raw, _ := json.Marshal(merge.Progress{Version: 1, QueueID: "q1", TicketID: "id", Repository: "o/r", PR: 1, Head: f.head, Main: f.main, Phase: "future-phase"})
	f.linear.rows = append(f.linear.rows, linear.Comment{CreatedAt: time.Now().Add(time.Second), Body: merge.ProgressPrefix + string(raw)})
	err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
	if err == nil || f.puts != 0 || len(f.linear.bodies) != 0 {
		t.Fatalf("unknown progress overwritten: %v %v", err, f.linear.bodies)
	}
}

func TestRuntimeCatchupReceiptSurvivesCleanupAndCompletion(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprint(cleanupFails), func(t *testing.T) {
			f := overlapFixture(t, false)
			dir := f.config.Repositories[0].Path
			calls := 0
			f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if s.Path == "bash" {
					calls++
					if calls == 1 {
						gitMergeTest(t, dir, "commit", "--allow-empty", "-m", "move once")
						f.main = gitMergeTest(t, dir, "rev-parse", "HEAD")
					}
					return []byte("UNIT executor\nVERDICT green\nSTEP all\nREASON checked\nEVIDENCE test\nSCOPE feature shared\nLANE 1"), nil
				}
				if cleanupFails && len(s.Args) > 1 && s.Args[0] == "worktree" && s.Args[1] == "remove" {
					return nil, fmt.Errorf("cleanup unavailable")
				}
				return runx.Run(ctx, s)
			}
			if err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background()); err == nil {
				t.Fatal("moving main must wait")
			}
			err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
			if (!cleanupFails && err != nil) || (cleanupFails && !errors.Is(err, merge.ErrWorktreeCleanup)) {
				t.Fatal(err)
			}
			var p merge.Progress
			for _, row := range f.linear.rows {
				for _, line := range strings.Split(row.Body, "\n") {
					if strings.HasPrefix(line, merge.ProgressPrefix) {
						p = merge.Progress{}
						json.Unmarshal([]byte(strings.TrimPrefix(line, merge.ProgressPrefix)), &p)
					}
				}
			}
			if f.puts != 1 || p.Phase != "merged" || p.CatchupRounds != 1 {
				t.Fatalf("catchup history lost: puts=%d progress=%+v", f.puts, p)
			}
		})
	}
}

func TestRuntimeMergeGateScriptReadRetainsSafeFailure(t *testing.T) {
	for _, kind := range []string{"cancel", "deadline", "global"} {
		t.Run(kind, func(t *testing.T) {
			f := overlapFixture(t, false)
			want := context.Canceled
			if kind == "deadline" || kind == "global" {
				want = context.DeadlineExceeded
			}
			failure := error(fmt.Errorf("secret-read-token: %w", want))
			if kind == "global" {
				failure = &infra.Global{Dep: infra.GitHub, Err: failure}
			}
			f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if len(s.Args) == 2 && s.Args[0] == "show" && strings.HasSuffix(s.Args[1], ":scripts/agent-gates/gate.sh") {
					return nil, failure
				}
				return runx.Run(ctx, s)
			}
			err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
			if !errors.Is(err, want) {
				t.Fatalf("lost read context identity: %v", err)
			}
			if kind == "global" {
				var global *infra.Global
				if !errors.As(err, &global) || global.Dep != infra.GitHub || strings.Contains(global.Error(), "secret-read-token") {
					t.Fatalf("lost safe global classification: %v", err)
				}
			}
			if strings.Contains(err.Error(), "secret-read-token") || f.puts != 0 || len(f.runtime.Orca.(*fakeOrca).launches) != 0 {
				t.Fatalf("unsafe failure/correction: %v", err)
			}
		})
	}
}

func TestRuntimeMergedGateAcceptsLowerActualLane(t *testing.T) {
	for _, policy := range []string{"approved", "manual", "lane3"} {
		t.Run(policy, func(t *testing.T) {
			f := overlapFixture(t, false)
			dir := f.config.Repositories[0].Path
			env := f.config.Repositories[0].Env
			env["AGENT_LANE2_GLOBS"] = "absorbed"
			if policy != "manual" {
				env["AGENT_LANE2_AUTOMERGE"] = "1"
			}
			if policy == "lane3" {
				env["AGENT_LANE3_GLOBS"] = "shared"
			}
			gitMergeTest(t, dir, "checkout", "agent/ENG-1/executor")
			if err := os.WriteFile(filepath.Join(dir, "absorbed"), []byte("same lane2 change\n"), 0600); err != nil {
				t.Fatal(err)
			}
			gitMergeTest(t, dir, "add", "absorbed")
			gitMergeTest(t, dir, "commit", "-m", "lane2 worker change")
			f.head = gitMergeTest(t, dir, "rev-parse", "HEAD")
			f.prBody["head"].(map[string]any)["sha"] = f.head
			gitMergeTest(t, dir, "checkout", "main")
			if err := os.WriteFile(filepath.Join(dir, "absorbed"), []byte("same lane2 change\n"), 0600); err != nil {
				t.Fatal(err)
			}
			script, err := os.ReadFile("../../../scripts/agent-gates/gate.sh")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "scripts/agent-gates/gate.sh"), script, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".omc/\n.merge-validation-*/\n"), 0600); err != nil {
				t.Fatal(err)
			}
			gitMergeTest(t, dir, "add", ".")
			gitMergeTest(t, dir, "commit", "-m", "main already absorbed lane2 change")
			f.main = gitMergeTest(t, dir, "rev-parse", "HEAD")
			rewriteHandoff(t, f, func(h map[string]any) {
				h["head"] = f.head
				h["lane"] = 2
				h["gate_spec"] = "UNIT: executor\nSCOPE: feature shared absorbed\nCHECK: test \"$(head -1 shared)\" = worker && test \"$(tail -1 shared)\" = main\nLANE: 2\n"
				h["gate_output"] = "UNIT executor\nVERDICT green\nSTEP all\nREASON checked\nEVIDENCE test\nSCOPE feature shared absorbed\nLANE 2"
			})
			err = (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
			if policy == "approved" {
				if err != nil || f.puts != 1 || f.linear.issue.State.Name != "Done" {
					t.Fatalf("lower actual lane blocked full green gate: %v puts=%d", err, f.puts)
				}
			} else if err == nil || f.puts != 0 {
				t.Fatalf("unsafe policy accepted: %s %v puts=%d", policy, err, f.puts)
			}
		})
	}
}
