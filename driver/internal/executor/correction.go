package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Recovery isolates coordinator sessions and the extra read/child-task operations.
type Recovery interface {
	Correction(context.Context, CorrectionRequest) error
	ChildTask(context.Context, string, string, string, string) (string, error)
	Complete(context.Context, string, string) (bool, error)
	Worktree(context.Context, string, string) (string, error)
}
type CorrectionRequest struct {
	Repo, Path, Cause, Brief string
	Number                   int
}

func (e *Executor) recovery() Recovery {
	if e.Recovery != nil {
		return e.Recovery
	}
	if r, ok := e.Tools.(Recovery); ok {
		return r
	}
	return nil
}
func (c *Commands) Correction(ctx context.Context, req CorrectionRequest) error {
	role, err := readRole(req.Repo, "correction.md")
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("%s\n\nCorrection file: %q\nCorrection: %d/3\nFailure: %s\nBrief:\n%s", role, req.Path, req.Number, req.Cause, req.Brief)
	run := c.Run
	if run == nil {
		run = runx.Run
	}
	// A coordinator subprocess must not inherit this worker's Orca hooks.
	env := make([]string, 0)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "ORCA_PANE_KEY", "ORCA_AGENT_HOOK_PORT", "ORCA_AGENT_HOOK_TOKEN":
			continue
		}
		env = append(env, entry)
	}
	_, err = run(ctx, runx.Spec{
		Path: "claude",
		Args: []string{"-p", prompt, "--permission-mode", "acceptEdits", "--add-dir", filepath.Dir(req.Path),
			"--allowedTools", "Read,Grep,Glob,Write,Edit", "--tools", "Read,Grep,Glob,Write,Edit"},
		Dir: req.Repo, Env: env, Timeout: 10 * time.Minute,
	})
	return err
}
func (c *Commands) orcaCall(ctx context.Context, args ...string) (orca.Receipt, error) {
	path, err := runx.OrcaPath()
	if err != nil {
		return nil, errors.New("cannot resolve Orca binary")
	}
	b, err := c.run(ctx, "", path, append([]string{"orchestration"}, append(args, "--json")...)...)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		OK     bool         `json:"ok"`
		Result orca.Receipt `json:"result"`
	}
	if json.Unmarshal(b, &envelope) != nil || !envelope.OK || envelope.Result == nil {
		return nil, errors.New("invalid Orca recovery receipt")
	}
	return envelope.Result, nil
}
func (c *Commands) ChildTask(ctx context.Context, run, parent, spec, request string) (string, error) {
	// A child is a new Task even when the parent reported success. Never retry-of
	// a successful Dispatch or overwrite its authoritative worker report.
	receipt, err := c.orcaCall(ctx, "task-create", "--run", run, "--parent", parent, "--task-title", "correction", "--spec", spec, "--retry-request", request)
	if err != nil {
		return "", err
	}
	id := receiptID(receipt, "task")
	if id == "" {
		return "", errors.New("child task receipt missing ID")
	}
	return id, nil
}

type transitionFunc func(machine.Event, func([]machine.Effect) error) error

func (e *Executor) correct(ctx context.Context, c Config, t Ticket, result *Result, cp *checkpoint, brief, cause, summary string, transition transitionFunc) (bool, error) {
	e.logger().Info("correction", "reason", cause, "fail_summary", summary)
	var err error
	brief, err = failHistoryBrief(c, t, brief)
	if err != nil {
		return false, err
	}
	r := e.recovery()
	if r == nil {
		return false, errors.New("correction adapter is required")
	}
	path := filepath.Join(c.Root, c.Repository, t.Identifier, "correction.md")
	for result.State.Phase == machine.PhaseCorrection {
		// An earlier session's artifact must never authorize a new child.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("cannot prepare correction artifact")
		}
		sessionErr := r.Correction(ctx, CorrectionRequest{Repo: c.Repo, Path: path, Cause: cause, Brief: brief, Number: result.State.Corrections})
		if stop := dependencyStop(ctx, sessionErr); stop != nil {
			return false, stop
		}
		info, statErr := os.Lstat(path)
		if sessionErr != nil || statErr != nil || !info.Mode().IsRegular() {
			if err := transition(machine.EvSessionEmpty, nil); err != nil {
				return false, err
			}
			continue
		}
		correction, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(correction)) == "" {
			if err := transition(machine.EvSessionEmpty, nil); err != nil {
				return false, err
			}
			continue
		}
		if cp.Worktree == "" {
			err = e.retryRead(ctx, t, result, "correction worktree lookup", func() error {
				var readErr error
				cp.Worktree, readErr = r.Worktree(ctx, c.Repo, "agent/"+t.Identifier+"/"+c.Unit)
				return readErr
			})
			if err != nil {
				return false, err
			}
		}
		if !filepath.IsAbs(cp.Worktree) {
			return false, errors.New("original worker worktree is unavailable")
		}
		tier, agent, model, effort, err := correctionTier(c, result.State.Corrections)
		if err != nil {
			return false, err
		}
		// Same tier + a live worker terminal means the settled worker can be reused
		// instead of launching a fresh agent (ZII-904).
		sameTier := agent == c.Agent && model == c.Model && effort == c.Effort
		handle := cp.Terminal
		if sameTier && handle == "" && result.DispatchID != "" {
			workers, listErr := e.Orca.Workers(ctx, orca.ListOptions{Run: result.RunID})
			if listErr == nil {
				for _, w := range workers {
					if w.DispatchID == result.DispatchID && w.AgentTerminalHandle != "" {
						handle = w.AgentTerminalHandle
						break
					}
				}
			}
		}
		canReuse := sameTier && handle != ""
		if tierEscalation(c) && !sameTier {
			if err = e.comment(ctx, t.ID, fmt.Sprintf("correction %d → 升至 %s", result.State.Corrections, tier)); err != nil {
				return false, errors.New("tier comment failed")
			}
		}
		err = transition(machine.EvSessionOK, func(fx []machine.Effect) error {
			if !hasEffect(fx, "worker-start-child-task-reuse-worktree") {
				return errors.New("missing correction worker effect")
			}
			cp.Corrections = result.State.Corrections
			childSpec, err := workerTaskSpec(c.Repo, fmt.Sprintf("correction %d/3\n%s\n%s", cp.Corrections, correction, brief))
			if err != nil {
				return err
			}
			if canReuse {
				// startDispatch creates the child once and clears PendingParent before
				// attempting the terminal-bound worker start.
				reuseErr := e.startCorrectionDispatch(ctx, c, t, cp, childSpec, orca.StartOptions{Run: result.RunID, Worktree: "path:" + cp.Worktree, Terminal: handle})
				if reuseErr == nil {
					cp.Terminal = handle
					result.DispatchID = cp.Dispatch
					return nil
				}
				if !orca.IsTerminalStale(reuseErr) {
					return reuseErr
				}
				// The terminal is gone: reuse the already-created child task and
				// start it with a fresh agent under a distinct request scope.
				freshOpts := orca.StartOptions{Run: result.RunID, Worktree: "path:" + cp.Worktree, Agent: agent, Model: model, Effort: effort}
				freshOpts.Task = cp.Task
				if err := e.startDispatchWithRequest(ctx, c, t, cp, freshOpts, workerRequest(t, c, cp, "worker-fallback")); err != nil {
					return err
				}
				cp.Terminal = ""
				result.DispatchID = cp.Dispatch
				return nil
			}
			if err := e.startCorrectionDispatch(ctx, c, t, cp, childSpec, orca.StartOptions{Run: result.RunID, Worktree: "path:" + cp.Worktree, Agent: agent, Model: model, Effort: effort}); err != nil {
				return err
			}
			cp.Terminal = ""
			result.DispatchID = cp.Dispatch
			return nil
		})
		return err == nil, err
	}
	return false, nil
}

func correctionTier(c Config, n int) (tier, agent, model, effort string, err error) {
	tiers := []string{"default", "strong"}
	index := 0
	if c.Tier == "strong" {
		index = 1
	} else if c.Tier != "" && c.Tier != "default" {
		err = errors.New("invalid worker tier")
		return
	}
	agent, model, effort = c.Agent, c.Model, c.Effort
	if tierEscalation(c) {
		index = min(index+n, len(tiers)-1)
		value := c.TierSettings[tiers[index]]
		if value == "" {
			value = os.Getenv("AGENT_TIER_" + strings.ToUpper(tiers[index]))
		}
		parts := strings.Split(value, ":")
		if len(parts) != 3 || parts[0] == "" {
			err = errors.New("escalated tier requires agent:model:effort configuration")
			return
		}
		agent, model, effort = parts[0], parts[1], parts[2]
	}
	if model == "" {
		effort = ""
	}
	return tiers[index], agent, model, effort, nil
}

func tierEscalation(c Config) bool {
	value := c.Escalate
	if value == "" {
		value = os.Getenv("AGENT_TIER_ESCALATE")
	}
	return value == "1"
}
