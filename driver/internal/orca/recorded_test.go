package orca

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

func recordedClient(t *testing.T, name string) *Client {
	t.Helper()
	fixture, err := filepath.Abs("../../testdata/recorded/" + name)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Client{Path: script}
}

func TestRecordedRequestMismatch(t *testing.T) {
	c := recordedClient(t, "orca-run-create-request-mismatch.json")
	_, err := c.RunCreate(context.Background(), "6e2f5bc0-7a6f-53e0-bff8-b8cdf5d2e070", "objective")
	var ticket *infra.Ticket
	if !errors.As(err, &ticket) || !strings.Contains(err.Error()+ticket.Err.Error(), "request_mismatch") {
		t.Fatalf("err=%v, want a non-retryable error naming request_mismatch", err)
	}
}

func TestRecordedTerminalSendTurnStarted(t *testing.T) {
	for name, want := range map[string]bool{
		"orca-prompt-receipt-codex-turn-started.json": true,
		"orca-terminal-send-wait-submit-shell.json":   false,
	} {
		receipt, err := recordedClient(t, name).TerminalSend(context.Background(), "term_one", "continue")
		if err != nil || TurnStarted(receipt) != want {
			t.Fatalf("%s: turn started=%v err=%v; want %v", name, TurnStarted(receipt), err, want)
		}
	}
}

func TestRecordedWorktreeSet(t *testing.T) {
	if _, err := recordedClient(t, "orca-worktree-set.json").WorktreeSet(context.Background(), "/worktrees/ZII-1-impl", "ZII-1", "in-progress", "gate"); err != nil {
		t.Fatalf("recorded success err=%v", err)
	}
	_, err := recordedClient(t, "orca-worktree-set-not-found.json").WorktreeSet(context.Background(), "/worktrees/ZII-1-impl", "ZII-1", "in-progress", "gate")
	var ticket *infra.Ticket
	if !errors.As(err, &ticket) || !strings.Contains(ticket.Err.Error(), "selector_not_found") {
		t.Fatalf("missing worktree err=%v", err)
	}
}

func TestRecordedWorkerListLiveness(t *testing.T) {
	workers, err := recordedClient(t, "orca-worker-list-liveness.json").Workers(context.Background(), ListOptions{Run: "run_9714ea9b8576", IncludeRemote: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		dispatch, verdict, reason, status, stage, detail, path, terminal string
	}{
		{"ctx_f9e3c9ac102a", "exited", "", "failed", "failed", "settled", "/worktrees/ZII-886-executor", "term_7acbe42d-9151-4fcc-8360-e87907f74698"},
		{"ctx_2ec61f77c499", "unverifiable", "stale_status", "completed", "succeeded", "settled", "/worktrees/ZII-886-executor-2", "term_8d26a5e7-7490-424c-9369-ad478e72ccbd"},
	} {
		t.Run(want.verdict, func(t *testing.T) {
			for _, w := range workers {
				if w.DispatchID != want.dispatch {
					continue
				}
				if w.Projection.Liveness.Verdict != want.verdict || w.Projection.Liveness.Reason != want.reason || w.DispatchStatus != want.status || w.Projection.Stage.Worker != want.stage || w.Projection.Stage.Detail != want.detail || w.WorktreePath() != want.path || w.AgentTerminalHandle != want.terminal {
					t.Fatalf("worker=%+v path=%q; want=%+v", w, w.WorktreePath(), want)
				}
				return
			}
			t.Fatalf("missing recorded dispatch %s", want.dispatch)
		})
	}
}

// worker-stop fences the dispatch and closes its agent terminal. The receipt below
// is a real one (a probe codex worker stopped right after start-ready).
func TestRecordedWorkerStop(t *testing.T) {
	receipt, err := recordedClient(t, "orca-worker-stop.json").WorkerStop(context.Background(), "c9a47146-5e87-470f-89ef-4fac6b8fec2e", "ctx_142789068719")
	if err != nil {
		t.Fatalf("recorded worker-stop err=%v", err)
	}
	var state string
	var settled bool
	if json.Unmarshal(receipt["state"], &state) != nil || state != "stopped" || json.Unmarshal(receipt["alreadySettled"], &settled) != nil || settled {
		t.Fatalf("state=%q alreadySettled=%v; want a freshly stopped dispatch", state, settled)
	}
}
