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

// Run invokes the whole original gate, with its project hooks and base pinned to
// the validated main. The frozen spec lives beside, never inside, the diff.
func (a *mergeAdapter) Run(ctx context.Context, req merge.Request, wt string) error {
	h := a.ticket.Handoff
	if err := h.Validated(); err != nil {
		return err
	}
	script, err := a.runtime.command(ctx, req.Path, "git", "show", a.main+":scripts/agent-gates/gate.sh")
	if err != nil {
		return safeMergeGateError(err)
	}
	if len(script) == 0 {
		return errors.New("original merge gate unavailable")
	}
	parent := filepath.Dir(wt)
	gatePath, specPath := filepath.Join(parent, "gate.sh"), filepath.Join(parent, "gate.spec")
	if err := os.WriteFile(gatePath, script, 0600); err != nil {
		return errors.New("cannot freeze merge gate")
	}
	if err := os.WriteFile(specPath, []byte(h.GateSpec), 0600); err != nil {
		return errors.New("cannot freeze merge spec")
	}
	env := map[string]string{}
	for _, entry := range os.Environ() {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			env[k] = v
		}
	}
	for k, v := range a.runtime.Repositories[a.ticket.Repo].Env {
		env[k] = v
	}
	env["GATE_BASE"], env["GATE_PROJECT_SRC"] = a.main, a.main
	entries := make([]string, 0, len(env))
	for k, v := range env {
		entries = append(entries, k+"="+v)
	}
	run := a.runtime.Run
	if run == nil {
		run = runx.Run
	}
	if install := a.runtime.Repositories[a.ticket.Repo].Env["AGENT_INSTALL_CMD"]; install != "" {
		if _, err := run(ctx, runx.Spec{Path: "bash", Args: []string{"-c", install}, Dir: wt, Env: entries, Timeout: 30 * time.Minute}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return safeMergeGateError(&infra.Ticket{Err: errors.New("merge install failed")})
		}
	}
	output, err := run(ctx, runx.Spec{Path: "bash", Args: []string{gatePath, wt, specPath}, Dir: req.Path, Env: entries, Timeout: 30 * time.Minute})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	verdict, parseErr := h.MergeGateVerdict(string(output))
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
