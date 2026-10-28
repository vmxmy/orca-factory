package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func mergeGateAdapter(install string, run func(context.Context, runx.Spec) ([]byte, error)) *mergeAdapter {
	h := validHandoffForResolver()
	env := map[string]string{}
	if install != "" {
		env["AGENT_INSTALL_CMD"] = install
	}
	r := &Runtime{Run: run, Repositories: map[string]config.Repository{"": {Env: env}}}
	return &mergeAdapter{runtime: r, ticket: Ticket{Handoff: &h}}
}

func runMergeGate(a *mergeAdapter, wt string) error {
	run, err := a.Prepare(context.Background(), merge.Candidate{Request: merge.Request{Path: "/repo"}, Main: strings.Repeat("c", 40)})
	if err != nil {
		return err
	}
	return run(context.Background(), wt)
}

func TestMergeGateRunsInstallCommand(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "wt")
	var calls []runx.Spec
	a := mergeGateAdapter("pnpm install --frozen-lockfile", func(_ context.Context, s runx.Spec) ([]byte, error) {
		calls = append(calls, s)
		if s.Path == "git" {
			return []byte("#!/bin/bash\n"), nil
		}
		if s.Args[0] == "-c" {
			return nil, nil
		}
		return []byte(validHandoffForResolver().GateOutput), nil
	})
	if err := runMergeGate(a, wt); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[1].Path != "bash" || calls[1].Args[0] != "-c" || calls[1].Args[1] != "pnpm install --frozen-lockfile" || calls[1].Dir != wt {
		t.Fatalf("install must run in the merge worktree before the gate (%d calls)", len(calls))
	}
	if !slices.Contains(calls[1].Env, "AGENT_INSTALL_CMD=pnpm install --frozen-lockfile") || calls[2].Args[0] == "-c" {
		t.Fatal("install env or gate order wrong")
	}

	t.Setenv("AGENT_INSTALL_CMD", "leak")
	calls = nil
	a = mergeGateAdapter("", a.runtime.Run)
	if err := runMergeGate(a, wt); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Path != "git" || calls[1].Args[0] == "-c" {
		t.Fatalf("unset repo install must skip install and ignore process env (%d calls)", len(calls))
	}
}

func TestMergeGateInstallFailureIsTicketInfra(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "wt")
	for _, installErr := range []error{errors.New("exit status 1: secret output"), context.DeadlineExceeded} {
		gateRan := false
		a := mergeGateAdapter("false", func(_ context.Context, s runx.Spec) ([]byte, error) {
			switch {
			case s.Path == "git":
				return []byte("#!/bin/bash\n"), nil
			case s.Args[0] == "-c":
				return []byte("secret output"), installErr
			}
			gateRan = true
			return nil, nil
		})
		err := runMergeGate(a, wt)
		var ticket *infra.Ticket
		if !errors.As(err, &ticket) || ticket.Err == nil || strings.Contains(ticket.Err.Error(), "secret") || errors.Is(err, installErr) || errors.Is(err, merge.ErrGateRed) {
			t.Fatalf("install error %v: want sanitized ticket infra error, got %v", installErr, err)
		}
		if gateRan {
			t.Fatalf("install error %v: gate ran after install failure", installErr)
		}
	}
}
