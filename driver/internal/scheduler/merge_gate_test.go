package scheduler

import (
	"context"
	"errors"
	"path/filepath"
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
	return &mergeAdapter{runtime: r, ticket: Ticket{Handoff: &h}, main: strings.Repeat("c", 40)}
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
	if err := a.Run(context.Background(), merge.Request{Path: "/repo"}, wt); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[1].Path != "bash" || calls[1].Args[0] != "-c" || calls[1].Args[1] != "pnpm install --frozen-lockfile" || calls[1].Dir != wt {
		t.Fatalf("install must run in the merge worktree before the gate (%d calls)", len(calls))
	}
	if !strings.Contains(strings.Join(calls[1].Env, "\n"), "AGENT_INSTALL_CMD=pnpm install --frozen-lockfile") || calls[2].Args[0] == "-c" {
		t.Fatal("install env or gate order wrong")
	}
}

func TestMergeGateInstallFailureIsTicketInfra(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "wt")
	gateRan := false
	a := mergeGateAdapter("false", func(_ context.Context, s runx.Spec) ([]byte, error) {
		switch {
		case s.Path == "git":
			return []byte("#!/bin/bash\n"), nil
		case s.Args[0] == "-c":
			return []byte("secret output"), errors.New("exit status 1")
		}
		gateRan = true
		return nil, nil
	})
	err := a.Run(context.Background(), merge.Request{Path: "/repo"}, wt)
	var ticket *infra.Ticket
	if !errors.As(err, &ticket) || errors.Is(err, merge.ErrGateRed) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("want sanitized ticket infra error, got %v", err)
	}
	if gateRan {
		t.Fatal("gate ran after install failure")
	}
}
