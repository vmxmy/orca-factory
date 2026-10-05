package orca

import (
	"context"
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
