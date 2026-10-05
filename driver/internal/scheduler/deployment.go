package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// DeploymentBarrier is independent of issue state, labels and current ownership.
func DeploymentBarrier(t Ticket) bool {
	return (t.Handoff != nil && t.MergeProblem != "") || (t.Progress != nil && t.Progress.Deployment != nil && t.Progress.Deployment.State != "passed")
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
		return r.pauseDeployment(ctx, t, p, "deployment requires human reconciliation")
	}
	owner, repo, _ := strings.Cut(p.Repository, "/")
	if d.Workflow != "" {
		state, err := r.GitHub.Deployment(ctx, owner, repo, d.Workflow, p.MergeSHA)
		if err != nil {
			return errors.New("deployment query unavailable")
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
		if _, err := r.runSmoke(ctx, t, p); err != nil {
			return r.pauseDeployment(ctx, t, p, "smoke failed or execution result uncertain; reconcile before recovery")
		}
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
	body := "转人: " + d.Reason + "\n需要你: 核对部署与 smoke，使用 resume-deployment 显式恢复\n阶段: 转人\n归属: 人\n" + merge.ProgressPrefix + string(b)
	if err := r.mergeComment(ctx, t, body); err != nil {
		return err
	}
	return errors.New(d.Reason)
}

func (r *Runtime) runSmoke(ctx context.Context, t Ticket, p *merge.Progress) ([]byte, error) {
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

// ResumeDeployment only records an explicit human decision. The caller must
// exclude serve with the same process lock; dry-run does not require a lock.
// Queue and SHA are mandatory to prevent a stale operator command from acting
// on a new handoff. This never merges, queries workflows or executes smoke.
func (r *Runtime) ResumeDeployment(ctx context.Context, identifier, queue, sha, action, operator, reason string, dry bool) error {
	if strings.TrimSpace(operator) == "" || strings.TrimSpace(reason) == "" || strings.ContainsAny(operator+reason, "\r\n") || !commitID(sha) || queue == "" {
		return errors.New("recovery requires queue, squash SHA, operator and reconciliation reason")
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
				return errors.New("ambiguous recovery ticket")
			}
			target = &tickets[i]
		}
	}
	if target == nil {
		return errors.New("deployment barrier not found")
	}
	t := *target
	p := t.Progress
	h := t.Handoff
	if t.MergeProblem != "" || p == nil || h == nil || p.Deployment == nil || p.QueueID != queue || p.QueueID != h.QueueID || p.MergeSHA != sha || p.Version != 1 || p.TicketID != t.ID || p.Repository != h.Repository || p.Head != h.Head || p.PR != h.PR || p.Phase != "merged" {
		return errors.New("recovery identity mismatch")
	}
	d := p.Deployment
	if d.State != "paused" && d.State != "smoke-intent" {
		return errors.New("deployment is not awaiting human recovery")
	}
	switch action {
	case "confirm-passed":
		d.State = "passed"
	case "retry-workflow":
		if d.Workflow == "" || d.SmokeStarted {
			return errors.New("smoke may have executed; reconcile explicitly")
		}
		d.State = "pending"
	case "retry-smoke":
		if d.Smoke == "" || !d.SmokeStarted {
			return errors.New("no smoke execution to reconcile")
		}
		d.State = "pending"
		d.SmokeStarted = false
	default:
		return errors.New("invalid deployment recovery action")
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	d.Recovery = &merge.DeploymentRecovery{Action: action, Operator: operator, Reason: reason, At: now.UTC()}
	d.Reason = ""
	if dry {
		return nil
	}
	b, _ := json.Marshal(p)
	return r.mergeComment(ctx, t, "阶段: 排队合并\n归属: 合并通道\n"+merge.ProgressPrefix+string(b))
}

func deploymentIdentity(t Ticket) error {
	p, h := t.Progress, t.Handoff
	if p != nil && p.Deployment != nil {
		d := p.Deployment
		if (d.Workflow == "" && d.Smoke == "") || deploymentConfig(map[string]string{"AGENT_DEPLOY_WORKFLOW": d.Workflow, "AGENT_SMOKE_CMD": d.Smoke}) != nil || (d.State == "pending" && d.SmokeStarted) {
			return errors.New("invalid deployment policy or smoke intent")
		}
	}
	if p == nil || h == nil || p.Version != 1 || p.QueueID != h.QueueID || p.TicketID != t.ID || p.Repository != h.Repository || p.PR != h.PR || p.Head != h.Head || (p.Phase != "merge-intent" && p.Phase != "merged") || (p.Phase == "merged" && !commitID(p.MergeSHA)) || (p.MainUnknown && (p.Main != "" || p.Phase != "merged")) || (!p.MainUnknown && !commitID(p.Main)) {
		return errors.New("conflicting deployment identity")
	}
	return nil
}
