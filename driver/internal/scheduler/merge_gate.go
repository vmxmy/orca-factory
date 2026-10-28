package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Prepare freezes the whole original gate for c on the inspection goroutine:
// the script and project hooks from c's main, the frozen spec and the
// repository env. The gate runs later on a validation goroutine, where it must
// not read the runtime, which the next inspection updates.
func (a *mergeAdapter) Prepare(ctx context.Context, c merge.Candidate) (merge.GateRun, error) {
	h := *a.ticket.Handoff
	if err := h.Validated(); err != nil {
		return nil, err
	}
	script, err := a.runtime.command(ctx, c.Request.Path, "git", "show", c.Main+":scripts/agent-gates/gate.sh")
	if err != nil {
		return nil, safeMergeGateError(err)
	}
	if len(script) == 0 {
		return nil, errors.New("original merge gate unavailable")
	}
	repoEnv := a.runtime.Repositories[a.ticket.Repo].Env
	env := map[string]string{}
	for _, entry := range os.Environ() {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			env[k] = v
		}
	}
	for k, v := range repoEnv {
		env[k] = v
	}
	env["GATE_BASE"], env["GATE_PROJECT_SRC"] = c.Main, c.Main
	entries := make([]string, 0, len(env))
	for k, v := range env {
		entries = append(entries, k+"="+v)
	}
	run := a.runtime.Run
	if run == nil {
		run = runx.Run
	}
	g := mergeGate{handoff: h, script: script, checkout: c.Request.Path, install: repoEnv["AGENT_INSTALL_CMD"], env: entries, run: run}
	return g.Run, nil
}

// mergeGate holds copies only, so it runs safely beside later inspections.
type mergeGate struct {
	handoff  merge.Handoff
	script   []byte
	checkout string
	install  string
	env      []string
	run      func(context.Context, runx.Spec) ([]byte, error)
}

// Run invokes the whole original gate, with its project hooks and base pinned to
// the validated main. The frozen spec lives beside, never inside, the diff.
func (g mergeGate) Run(ctx context.Context, wt string) error {
	parent := filepath.Dir(wt)
	gatePath, specPath := filepath.Join(parent, "gate.sh"), filepath.Join(parent, "gate.spec")
	if err := os.WriteFile(gatePath, g.script, 0600); err != nil {
		return errors.New("cannot freeze merge gate")
	}
	if err := os.WriteFile(specPath, []byte(g.handoff.GateSpec), 0600); err != nil {
		return errors.New("cannot freeze merge spec")
	}
	if g.install != "" {
		if _, err := g.run(ctx, runx.Spec{Path: "bash", Args: []string{"-c", g.install}, Dir: wt, Env: g.env, Timeout: 30 * time.Minute}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return safeMergeGateError(&infra.Ticket{Err: errors.New("merge install failed")})
		}
	}
	output, err := g.run(ctx, runx.Spec{Path: "bash", Args: []string{gatePath, wt, specPath}, Dir: g.checkout, Env: g.env, Timeout: 30 * time.Minute})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	verdict, parseErr := g.handoff.MergeGateVerdict(string(output))
	if parseErr != nil {
		return parseErr
	}
	if verdict == "green" && err == nil {
		return nil
	}
	var code interface{ ExitCode() int }
	if verdict == "red" && (err == nil || (errors.As(err, &code) && code.ExitCode() == 1)) {
		return merge.ErrGateRed
	}
	return errors.New("merge gate execution unavailable")
}

// Preserve recognized dependency identities without leaking command output from
// adapter errors. This is also safe when callers inspect the unwrapped chain.
func safeMergeGateError(err error) error {
	var global *infra.Global
	if errors.As(err, &global) {
		return &infra.Global{Dep: global.Dep, Err: safeMergeGateError(global.Err)}
	}
	var ticket *infra.Ticket
	if errors.As(err, &ticket) {
		return &infra.Ticket{Err: safeMergeGateError(ticket.Err)}
	}
	var transient *infra.Transient
	if errors.As(err, &transient) {
		return &infra.Transient{Err: safeMergeGateError(transient.Err)}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("original merge gate unavailable")
}
