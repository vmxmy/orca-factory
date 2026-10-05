package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type currentFailureOrca struct{ *recoveryOrca }

func (o *currentFailureOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	if len(o.starts) == 0 {
		return []orca.Task{{ID: "task_one", RunID: run, Result: json.RawMessage(`{"provenance":"worker_report","outcome":"failed"}`)}}, nil
	}
	return o.recoveryOrca.TaskList(ctx, run)
}

func TestStartCurrentTaskFailureOverridesOldComplete(t *testing.T) {
	e, o, _, s, _, ticket := recoveryFixture(t)
	e.Orca = &currentFailureOrca{o}
	s.complete = true
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Worktree: "/original/worktree", Baseline: "base-sha"}); err != nil {
		t.Fatal(err)
	}
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || s.sessions != 1 || len(o.starts) != 1 {
		t.Fatalf("current failed task must receive correction: result=%+v err=%v sessions=%d starts=%d", result, err, s.sessions, len(o.starts))
	}
	if o.starts[0].Worktree != "path:/original/worktree" {
		t.Fatalf("lost worktree: %+v", o.starts[0])
	}
}

func TestStartPendingCorrectionCannotUseOldComplete(t *testing.T) {
	e, _, tools, s, _, ticket := recoveryFixture(t)
	s.complete = true
	o := &recordedTasks{tasks: []orca.Task{{ID: "child_task", RunID: "run_one", Status: "pending"}}}
	e.Orca = o
	// Legacy checkpoints may retain completion bits from the parent dispatch.
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "child_task", Pending: true, Completed: true, Settled: true, Corrections: 1, Worktree: "/original/worktree"}); err != nil {
		t.Fatal(err)
	}
	result, err := e.Start(context.Background(), ticket)
	if err == nil || result.State.Owner == machine.OwnerMerge || tools.gateCalls != 0 {
		t.Fatalf("pending correction must remain unresolved: result=%+v err=%v gates=%d", result, err, tools.gateCalls)
	}
	if o.releases != 0 {
		t.Fatalf("released unconfirmed correction: %d", o.releases)
	}
}

// A transport error can accompany a usable receipt; retain its association for
// a different terminal without replaying that terminal's mutation.
type uncertainReceiptOrca struct {
	*recoveryOrca
}

func (o *uncertainReceiptOrca) WorkerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	o.starts = append(o.starts, opts)
	return orca.Receipt{"task": json.RawMessage(`{"id":"task_one"}`), "dispatch": json.RawMessage(`{"id":"ctx_one"}`), "placement": json.RawMessage(`{"worktreePath":"/original/worktree"}`)}, errors.New("lost response")
}
func (o *uncertainReceiptOrca) TaskList(context.Context, string) ([]orca.Task, error) {
	return []orca.Task{{ID: "task_one", RunID: "run_one", Result: json.RawMessage(`{"provenance":"worker_report","outcome":"succeeded"}`)}}, nil
}
func TestStartRetainsUncertainReceiptAcrossTerminals(t *testing.T) {
	e, base, _, _, _, ticket := recoveryFixture(t)
	o := &uncertainReceiptOrca{recoveryOrca: base}
	e.Orca = o
	if _, err := e.Execute(context.Background(), ticket); err == nil {
		t.Fatal("expected uncertain start")
	}
	if o.releases != 0 {
		t.Fatal("released unconfirmed dispatch")
	}
	e.Config.Terminal = "replacement_terminal"
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || len(o.starts) != 1 || o.releases != 1 || result.DispatchID != "ctx_one" {
		t.Fatalf("must adopt receipt association: result=%+v err=%v starts=%d releases=%d", result, err, len(o.starts), o.releases)
	}
}

type uncertainChildService struct{ recoveryService }

func (s *uncertainChildService) ChildTask(context.Context, string, string, string, string) (string, error) {
	s.children++
	return "", errors.New("child created but response lost")
}
func TestStartUncertainChildCannotResumeParent(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	tools.gateReds = 1
	s := &uncertainChildService{}
	e.Recovery = s
	if _, err := e.Execute(context.Background(), ticket); err == nil {
		t.Fatal("expected uncertain child")
	}
	e.Config.Terminal = "replacement_terminal"
	result, err := e.Start(context.Background(), ticket)
	if err == nil || result.State.Owner == machine.OwnerMerge || s.children != 1 || len(o.starts) != 1 || len(e.GitHub.(*fakeGitHub).created) != 0 {
		t.Fatalf("must not replay child or complete parent: result=%+v err=%v children=%d starts=%d", result, err, s.children, len(o.starts))
	}
}

func TestStartRejectsIncompleteTaskEvidence(t *testing.T) {
	for _, incomplete := range []string{`"truncated":true`, `"page":{"hasMore":true,"nextCursor":"next"}`, `"count":2`, `"truncated":"unknown"`} {
		t.Run(incomplete, func(t *testing.T) {
			e, _, tools, _, _, ticket := recoveryFixture(t)
			dir := t.TempDir()
			payload := `{"ok":true,"result":{"tasks":[{"id":"task_one","run_id":"run_one","result":{"provenance":"worker_report","outcome":"succeeded"}}],` + incomplete + `}}`
			if err := os.WriteFile(filepath.Join(dir, "tasks.json"), []byte(payload), 0600); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(dir, "orca")
			if err := os.WriteFile(script, []byte("#!/bin/sh\nif [ \"$2\" = task-list ]; then cat \"$TASK_EVIDENCE\"; else echo '{\"ok\":true,\"result\":{}}'; fi\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TASK_EVIDENCE", filepath.Join(dir, "tasks.json"))
			e.Orca = &orca.Client{Path: script}
			if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one"}); err != nil {
				t.Fatal(err)
			}
			result, err := e.Start(context.Background(), ticket)
			if err == nil || result.State.Owner == machine.OwnerMerge || tools.gateCalls != 0 {
				t.Fatalf("incomplete evidence must stop recovery: result=%+v err=%v gates=%d", result, err, tools.gateCalls)
			}
		})
	}
}

func TestStartPendingCorrectionCannotHandoffExistingPR(t *testing.T) {
	e, _, tools, s, _, ticket := recoveryFixture(t)
	s.complete = true
	e.GitHub.(*fakeGitHub).existing = &github.PullRequest{Number: 7, HTMLURL: "https://example.test/pr/7"}
	o := &recordedTasks{tasks: []orca.Task{{ID: "child_task", RunID: "run_one", Status: "pending"}}}
	e.Orca = o
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "child_task", Pending: true, Corrections: 1, Worktree: "/original/worktree"}); err != nil {
		t.Fatal(err)
	}
	result, err := e.Start(context.Background(), ticket)
	if err == nil || result.State.Owner == machine.OwnerMerge || tools.gateCalls != 0 || o.releases != 0 {
		t.Fatalf("pending correction must precede PR handoff: result=%+v err=%v gates=%d releases=%d", result, err, tools.gateCalls, o.releases)
	}
}

func TestStartMissingCurrentTaskCannotUseCachedCompletion(t *testing.T) {
	e, _, tools, _, _, ticket := recoveryFixture(t)
	o := &recordedTasks{}
	e.Orca = o
	e.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Completed: true, Settled: true}); err != nil {
		t.Fatal(err)
	}
	result, err := e.Start(context.Background(), ticket)
	if err == nil || result.State.Owner == machine.OwnerMerge || tools.gateCalls != 0 || o.releases != 0 {
		t.Fatalf("missing current task cannot authorize completion or release: result=%+v err=%v gates=%d releases=%d", result, err, tools.gateCalls, o.releases)
	}
}

type recordedRecoveryOrca struct {
	*recoveryOrca
	client *orca.Client
}

func (o *recordedRecoveryOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	return o.client.TaskList(ctx, run)
}

func TestRecordedStartWithoutDispatchID(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	dir := t.TempDir()
	payload := filepath.Join(dir, "task-list.json")
	if err := os.WriteFile(payload, recorded(t, "orca-task-list.json"), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat \"$TASK_EVIDENCE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TASK_EVIDENCE", payload)
	e.Orca = &recordedRecoveryOrca{recoveryOrca: o, client: &orca.Client{Path: script}}
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_c2d32709f17c", Task: "task_984317b058ca", Pending: true, Worktree: "/original/worktree"}); err != nil {
		t.Fatal(err)
	}
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || result.DispatchID != "" || len(o.starts) != 0 || o.releases != 0 || tools.gateCalls != 1 {
		t.Fatalf("recorded task should recover without fabricated dispatch: result=%+v err=%v starts=%d releases=%d gates=%d", result, err, len(o.starts), o.releases, tools.gateCalls)
	}
}

func TestStartAmbiguousTaskEvidenceStops(t *testing.T) {
	e, _, tools, _, _, ticket := recoveryFixture(t)
	o := &recordedTasks{tasks: []orca.Task{
		{ID: "task_a", RunID: "run_one", Spec: "agent/ZII-861/executor"},
		{ID: "task_b", RunID: "run_one", Spec: "agent/ZII-861/executor"},
	}}
	e.Orca = o
	if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: "run_one", Pending: true}); err != nil {
		t.Fatal(err)
	}
	result, err := e.Start(context.Background(), ticket)
	if err == nil || result.State.Owner == machine.OwnerMerge || len(o.workerRequests) != 0 || o.releases != 0 || tools.gateCalls != 0 {
		t.Fatalf("ambiguous evidence must stop: result=%+v err=%v", result, err)
	}
}

type activeCorrectionOrca struct{ *recoveryOrca }

func (o *activeCorrectionOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	if len(o.starts) > 1 {
		return []orca.Task{{ID: "child_task", RunID: run}}, nil
	}
	return o.recoveryOrca.TaskList(ctx, run)
}
func TestExecuteCancellationDoesNotReleaseNewCorrection(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	tools.gateReds = 1
	e.Orca = &activeCorrectionOrca{o}
	e.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	result, err := e.Execute(context.Background(), ticket)
	if !errors.Is(err, context.Canceled) || len(o.starts) != 2 || o.releases != 1 || result.State.Owner == machine.OwnerMerge {
		t.Fatalf("only settled parent may be released: result=%+v err=%v starts=%d releases=%d", result, err, len(o.starts), o.releases)
	}
}
