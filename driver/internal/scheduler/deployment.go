package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// DeploymentBarrier is independent of issue state, labels and current ownership.
func DeploymentBarrier(t Ticket) bool {
	return (t.Handoff != nil && t.MergeProblem != "") || (t.Progress != nil && t.Progress.Deployment != nil && t.Progress.Deployment.State != "passed" && t.Progress.Phase != "correction" && t.Progress.Phase != "human")
}
func deploymentConfig(env map[string]string) error {
	w, s := env["AGENT_DEPLOY_WORKFLOW"], env["AGENT_SMOKE_CMD"]
	if (w != "" && !github.ValidWorkflow(w)) || (s != "" && strings.TrimSpace(s) == "") || strings.ContainsRune(s, 0) {
		return errors.New("invalid deployment configuration")
	}
	return nil
}
func newDeployment(env map[string]string) *merge.DeploymentProgress {
	w, s := env["AGENT_DEPLOY_WORKFLOW"], env["AGENT_SMOKE_CMD"]
	if w == "" && s == "" {
		return nil
	}
	return &merge.DeploymentProgress{Workflow: w, Smoke: s, State: "pending"}
}

// A same-queue intent freezes even the absence of deployment configuration.
// Environment changes apply to new queues, never an in-progress squash retry.
func deploymentFor(t Ticket, env map[string]string) *merge.DeploymentProgress {
	if t.Progress != nil && t.Handoff != nil && t.Progress.QueueID == t.Handoff.QueueID {
		if t.Progress.Deployment != nil || t.deploymentReservedQueue == t.Handoff.QueueID || t.Progress.Phase == "merge-intent" || t.Progress.Phase == "merged" {
			return t.Progress.Deployment
		}
	}
	return newDeployment(env)
}

func (r *Runtime) deploymentReceipt(ctx context.Context, t Ticket, p *merge.Progress) error {
	b, _ := json.Marshal(p)
	return r.mergeComment(ctx, t, merge.ProgressPrefix+string(b))
}
func (r *Runtime) advanceDeployment(ctx context.Context, t Ticket, p *merge.Progress) error {
	d := p.Deployment
	if err := r.deploymentReceipt(ctx, t, p); err != nil {
		return err
	}
	if d.State == "passed" {
		return nil
	}
	if d.State != "pending" {
		return r.pauseDeployment(ctx, t, p, "deployment requires agent reconciliation")
	}
	owner, repo, _ := strings.Cut(p.Repository, "/")
	if d.Workflow != "" {
		state, err := r.GitHub.Deployment(ctx, owner, repo, d.Workflow, p.MergeSHA)
		if err != nil {
			return fmt.Errorf("deployment query unavailable: %w", err)
		}
		if state == "failed" {
			return r.pauseDeployment(ctx, t, p, "deployment workflow failed")
		}
		if state != "passed" {
			return errors.New("deployment pending")
		}
	}
	if d.Smoke != "" {
		d.State = "smoke-intent"
		d.SmokeStarted = true
		if err := r.deploymentReceipt(ctx, t, p); err != nil {
			return err
		}
		observed := t
		observed.Progress = p
		if _, err := r.runSmoke(ctx, observed, p); err != nil {
			safe := safeDeploymentFailure(err)
			if ctx.Err() != nil {
				return errors.Join(safe, ctx.Err())
			}
			return errors.Join(safe, r.pauseDeployment(ctx, t, p, "smoke failed or execution result uncertain; reconcile before recovery"))
		}
		d.SmokePassed = true
	}
	d.State = "passed"
	return r.deploymentReceipt(ctx, t, p)
}

func (r *Runtime) pauseDeployment(ctx context.Context, t Ticket, p *merge.Progress, reason string) error {
	d := p.Deployment
	if d.State != "paused" {
		d.State = "paused"
		d.Reason = reason
	}
	b, _ := json.Marshal(p)
	body := "Agent处置: " + d.Reason + "\n处置要求: 核对部署与 smoke，返回绑定队列和 SHA 的恢复决策\n阶段: Agent处置\n归属: 处置Agent\n" + merge.ProgressPrefix + string(b)
	if err := r.mergeComment(ctx, t, body); err != nil {
		return err
	}
	return errors.New(d.Reason)
}

func (r *Runtime) runSmoke(ctx context.Context, t Ticket, p *merge.Progress) ([]byte, error) {
	release, err := maintenance.Write()
	if err != nil {
		return nil, err
	}
	defer release()
	if err = r.verifyGitHubDecision(ctx, t, ""); err != nil {
		return nil, err
	}
	env := []string{}
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "AGENT_MERGE_SHA=") && !strings.HasPrefix(item, "AGENT_GH_REPO=") {
			env = append(env, item)
		}
	}
	env = append(env, "AGENT_MERGE_SHA="+p.MergeSHA, "AGENT_GH_REPO="+p.Repository)
	run := r.Run
	if run == nil {
		run = runx.Run
	}
	return run(ctx, runx.Spec{Path: "bash", Args: []string{"-c", p.Deployment.Smoke}, Dir: t.Repo, Env: env, Timeout: 30 * time.Second})
}

// ResumeDeployment records an explicit reconciled decision. The caller must
// exclude serve with the same process lock; dry-run does not require a lock.
// Queue and SHA are mandatory to prevent a stale operator command from acting
// on a new handoff. This never merges, queries workflows or executes smoke.
var errDeploymentProposal = errors.New("deployment proposal rejected")

func rejectDeploymentProposal(reason string) error {
	return fmt.Errorf("%w: %s", errDeploymentProposal, reason)
}

func (r *Runtime) ResumeDeployment(ctx context.Context, identifier, queue, sha, action, operator, reason string, dry bool) error {
	if strings.TrimSpace(operator) == "" || strings.TrimSpace(reason) == "" || strings.ContainsAny(operator+reason, "\r\n") || !commitID(sha) || queue == "" {
		return rejectDeploymentProposal("recovery requires queue, squash SHA, operator and reconciliation reason")
	}
	repos := []config.Repository{}
	for _, repo := range r.Repositories {
		repos = append(repos, repo)
	}
	tickets, err := r.Snapshot(ctx, repos)
	if err != nil {
		return err
	}
	var target *Ticket
	for i := range tickets {
		if tickets[i].Identifier == identifier {
			if target != nil {
				return rejectDeploymentProposal("ambiguous recovery ticket")
			}
			target = &tickets[i]
		}
	}
	if target == nil {
		return rejectDeploymentProposal("deployment barrier not found")
	}
	t := *target
	p := t.Progress
	h := t.Handoff
	if t.MergeProblem != "" || p == nil || h == nil || p.Deployment == nil || p.QueueID != queue || p.QueueID != h.QueueID || p.MergeSHA != sha || p.Version != 1 || p.TicketID != t.ID || p.Repository != h.Repository || p.Head != h.Head || p.PR != h.PR || p.Phase != "merged" {
		return rejectDeploymentProposal("recovery identity mismatch")
	}
	next := *p
	deployment := *p.Deployment
	next.Deployment = &deployment
	p = &next
	d := p.Deployment
	if d.State != "paused" && d.State != "smoke-intent" {
		return rejectDeploymentProposal("deployment is not awaiting recovery")
	}
	switch action {
	case "confirm-passed":
		d.State = "passed"
	case "retry-workflow":
		if d.Workflow == "" || d.SmokeStarted {
			return rejectDeploymentProposal("smoke may have executed; reconcile explicitly")
		}
		d.State = "pending"
	case "retry-smoke":
		if d.Smoke == "" || !d.SmokeStarted {
			return rejectDeploymentProposal("no smoke execution to reconcile")
		}
		d.State = "pending"
		d.SmokeStarted = false
		d.SmokePassed = false
	default:
		return rejectDeploymentProposal("invalid deployment recovery action")
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	d.Recovery = &merge.DeploymentRecovery{Action: action, Operator: operator, Reason: reason, At: now.UTC()}
	d.Reason = ""
	if !deploymentTransition(t.Progress, p) {
		return rejectDeploymentProposal("invalid or stale deployment recovery")
	}
	if dry {
		return nil
	}
	b, _ := json.Marshal(p)
	body := "阶段: 排队合并\n归属: 合并通道\n" + merge.ProgressPrefix + string(b)
	if t.Resolution != nil && t.Resolution.Phase == "result" && operator == "resolver:"+t.Resolution.Dispatch {
		settled := *t.Resolution
		settled.Phase = "applied"
		raw, _ := json.Marshal(settled)
		body += "\n" + resolver.Prefix + string(raw)
	}
	return r.mergeComment(ctx, t, body)
}

func deploymentIdentity(t Ticket) error {
	p, h := t.Progress, t.Handoff
	if p != nil && p.Deployment != nil {
		d := p.Deployment
		if (d.Workflow == "" && d.Smoke == "") || deploymentConfig(map[string]string{"AGENT_DEPLOY_WORKFLOW": d.Workflow, "AGENT_SMOKE_CMD": d.Smoke}) != nil || (d.State == "pending" && d.SmokeStarted) {
			return errors.New("invalid deployment policy or smoke intent")
		}
	}
	if p == nil || h == nil || p.Version != 1 || p.QueueID != h.QueueID || p.TicketID != t.ID || p.Repository != h.Repository || p.PR != h.PR || p.Head != h.Head || (p.Phase != "merge-intent" && p.Phase != "merged" && !unmergedDeploymentDecision(p)) || (p.Phase == "merged" && !commitID(p.MergeSHA)) || (p.MainUnknown && (p.Main != "" || p.Phase != "merged")) || (!p.MainUnknown && !commitID(p.Main)) {
		return errors.New("conflicting deployment identity")
	}
	return nil
}

// A merge decision may retain a policy reserved before an unsuccessful squash.
// It is not evidence that deployment or smoke has started.
func unmergedDeploymentDecision(p *merge.Progress) bool {
	return p != nil && (p.Phase == "waiting" || p.Phase == "correction" || p.Phase == "human") && p.MergeSHA == "" && p.Deployment != nil && p.Deployment.State == "pending" && !p.Deployment.SmokeStarted
}

// Retain safe control-flow facts, not subprocess messages or credential-bearing
// underlying errors. Even errors.As callers receive a sanitized cause.
func safeDeploymentFailure(err error) error {
	cause := errors.New("smoke execution failed")
	if errors.Is(err, context.Canceled) {
		cause = context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		cause = errors.Join(cause, context.DeadlineExceeded)
	}
	var global *infra.Global
	var transient *infra.Transient
	var ticket *infra.Ticket
	classified := infra.Classify(err)
	switch {
	case errors.As(classified, &global):
		return &infra.Global{Dep: global.Dep, Err: cause}
	case errors.As(classified, &transient):
		return &infra.Transient{Err: cause}
	case errors.As(classified, &ticket):
		return &infra.Ticket{Err: cause}
	default:
		return cause
	}
}
