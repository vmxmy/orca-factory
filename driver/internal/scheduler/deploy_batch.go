package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

const selfDeployBatchLimit = 3

type deployBatchKey struct{}
type deployBatch struct{ tickets []Ticket }

func deploymentSHA(p *merge.Progress) string {
	if p.Deployment != nil && p.Deployment.Batch != nil {
		return p.Deployment.Batch.SHA
	}
	return p.MergeSHA
}
func deploymentBatchBinding(d *merge.DeploymentProgress) *merge.DeploymentBatch {
	if d == nil {
		return nil
	}
	return d.Batch
}
func validDeploymentBatch(p *merge.Progress) bool {
	d := p.Deployment
	if d == nil || d.Batch == nil {
		return true
	}
	b := d.Batch
	if !d.Self || p.Phase != "merged" || !commitID(b.SHA) || len(b.Members) < 2 || len(b.Members) > selfDeployBatchLimit || b.Members[len(b.Members)-1].MergeSHA != b.SHA {
		return false
	}
	seen := map[string]bool{}
	queues := map[string]bool{}
	found := false
	for _, m := range b.Members {
		if m.TicketID == "" || m.QueueID == "" || !commitID(m.MergeSHA) || seen[m.TicketID] || queues[m.QueueID] {
			return false
		}
		seen[m.TicketID] = true
		queues[m.QueueID] = true
		if m.TicketID == p.TicketID {
			found = m.QueueID == p.QueueID && m.MergeSHA == p.MergeSHA
		}
	}
	return found
}

func (r *Runtime) refreshBatchTicket(ctx context.Context, t Ticket) (Ticket, error) {
	comments, err := r.Clients[t.Repo].ListComments(ctx, t.ID)
	if err != nil {
		return t, err
	}
	fresh := Ticket{ID: t.ID, Identifier: t.Identifier, Repo: t.Repo}
	if err = readOwnership(&fresh, comments); err != nil {
		return t, err
	}
	readMergeHandoff(&fresh, comments)
	fresh.Resolution, err = readResolution(comments)
	if err != nil {
		return t, err
	}
	if fresh.MergeProblem != "" {
		return t, errors.New(fresh.MergeProblem)
	}
	return fresh, nil
}

func (r *Runtime) advanceMergeQueue(ctx context.Context, rows []Ticket) error {
	head := rows[0]
	policy := deploymentFor(head, r.Repositories[head.Repo].Env)
	if policy == nil || !policy.Self || policy.State != "pending" && policy.Batch == nil {
		return r.advanceMerge(ctx, head)
	}
	// A partial batch binding is recovered from any member before more merges.
	for _, t := range rows {
		if t.Progress != nil && deploymentBatchBinding(t.Progress.Deployment) != nil {
			for _, member := range t.Progress.Deployment.Batch.Members {
				if member.TicketID == head.ID {
					return r.resumeDeployBatch(ctx, rows, t.Progress.Deployment.Batch)
				}
			}
		}
	}
	if head.Owner != machine.OwnerMerge || policy.State != "pending" || policy.Recovery != nil || policy.Reruns != 0 || !policy.StartedAt.IsZero() {
		return r.advanceMerge(ctx, head)
	}
	batch := &deployBatch{}
	for _, t := range rows[:min(len(rows), selfDeployBatchLimit)] {
		d := deploymentFor(t, r.Repositories[t.Repo].Env)
		if t.Owner != machine.OwnerMerge || d == nil || !d.Self || d.State != "pending" || d.Batch != nil || d.Recovery != nil || d.Reruns != 0 || !d.StartedAt.IsZero() || d.Smoke != policy.Smoke {
			break
		}
		if len(batch.tickets) > 0 {
			gh, err := r.githubClient()
			if err != nil {
				runx.LoggerFrom(ctx).Info("self deployment batch stopped at candidate", "ticket", t.Identifier, "err", err)
				break
			}
			owner, repo, _ := strings.Cut(t.Handoff.Repository, "/")
			checks, err := gh.MergeChecks(ctx, owner, repo, t.Handoff.Head)
			if err != nil {
				runx.LoggerFrom(ctx).Info("self deployment batch stopped at candidate", "ticket", t.Identifier, "err", err)
				break
			}
			if !checks.Green {
				break
			}
		}
		captured := false
		err := r.mergeTicket(ctx, t, func(p *merge.Progress) error {
			if err := r.deploymentReceipt(ctx, t, p); err != nil {
				return err
			}
			fresh, err := r.refreshBatchTicket(ctx, t)
			if err != nil {
				return err
			}
			batch.tickets = append(batch.tickets, fresh)
			captured = true
			return nil
		})
		if err != nil {
			if len(batch.tickets) == 0 {
				return err
			}
			runx.LoggerFrom(ctx).Info("self deployment batch stopped at candidate", "ticket", t.Identifier, "err", err)
			break
		}
		if !captured {
			break
		}
	}
	if len(batch.tickets) == 0 {
		return nil
	}
	if len(batch.tickets) == 1 {
		return r.advanceMerge(ctx, batch.tickets[0])
	}
	binding := &merge.DeploymentBatch{}
	for _, t := range batch.tickets {
		p := t.Progress
		binding.Members = append(binding.Members, merge.DeploymentMember{TicketID: t.ID, QueueID: p.QueueID, MergeSHA: p.MergeSHA})
		binding.SHA = p.MergeSHA
	}
	return r.resumeDeployBatch(ctx, batch.tickets, binding)
}

func (r *Runtime) resumeDeployBatch(ctx context.Context, rows []Ticket, binding *merge.DeploymentBatch) error {
	batch, err := r.loadDeployBatch(ctx, rows, binding)
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, deployBatchKey{}, batch)
	first := batch.tickets[0]
	d := *first.Progress.Deployment
	d.Batch = binding
	for _, t := range batch.tickets {
		if t.Progress.Deployment.State == "passed" {
			d = *t.Progress.Deployment
			break
		}
	}
	for _, t := range batch.tickets {
		other := *t.Progress.Deployment
		other.Batch = binding
		// Pending receipt times may differ if the batch binding write was interrupted.
		if d.State == "pending" && other.State == "pending" {
			other.StartedAt = d.StartedAt
		}
		if d.State == "passed" && other.State == "smoke-intent" && other.SmokeStarted {
			other.State = "passed"
			other.SmokePassed = d.SmokePassed
		}
		if !reflect.DeepEqual(d, other) {
			if d.State == "passed" {
				return errors.New("passed batch conflicts with another member; reconcile before completion")
			}
			unlock, err := r.deployLock()
			if err != nil {
				return err
			}
			defer unlock()
			return r.pauseSelf(ctx, first, first.Progress, "deployment batch receipts disagree; reconcile all members")
		}
	}
	p := *first.Progress
	p.Deployment = &d
	if err := r.deploymentReceipt(ctx, first, &p); err != nil {
		return err
	}
	first = batch.tickets[0]
	if err := r.advanceDeployment(ctx, first, &p); err != nil {
		return err
	}
	for _, t := range batch.tickets {
		fresh, err := r.refreshBatchTicket(ctx, t)
		if err != nil {
			return err
		}
		if fresh.Owner == machine.OwnerNone {
			issue, err := r.Clients[fresh.Repo].Read(ctx, fresh.ID)
			if err != nil {
				return err
			}
			if issue.State.Name == "Done" {
				continue
			}
		}
		if err := r.advanceMerge(context.WithValue(ctx, deployBatchKey{}, nil), fresh); err != nil {
			return err
		}
	}
	return nil
}

// All members acknowledge a state before install, smoke or Done can follow it.
func (r *Runtime) batchReceipt(ctx context.Context, batch *deployBatch, d *merge.DeploymentProgress, reason string) error {
	var problems []error
	for i, t := range batch.tickets {
		fresh, err := r.refreshBatchTicket(ctx, t)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if !reflect.DeepEqual(fresh.Handoff, t.Handoff) || fresh.Progress == nil || fresh.Progress.MergeSHA != t.Progress.MergeSHA {
			problems = append(problems, errors.New("batch identity changed before receipt"))
			continue
		}
		p := *fresh.Progress
		next := *d
		if reason != "" {
			next = *fresh.Progress.Deployment
			next.State = "paused"
			next.Reason = reason
		}
		p.Deployment = &next
		if !deploymentTransition(fresh.Progress, &p) {
			problems = append(problems, errors.New("invalid batch deployment transition"))
			continue
		}
		raw, _ := json.Marshal(p)
		body := merge.ProgressPrefix + string(raw)
		if reason != "" {
			body = "Agent处置: " + reason + "\n处置要求: 核对整批部署与 smoke，返回绑定队列和 SHA 的恢复决策\n阶段: Agent处置\n归属: 处置Agent\n" + body
		}
		if err = r.mergeComment(ctx, fresh, body); err != nil {
			problems = append(problems, err)
			continue
		}
		fresh.Progress = &p
		batch.tickets[i] = fresh
		if reason != "" {
			r.board(ctx, fresh, machine.PhaseResolver, reason)
		}
	}
	return errors.Join(problems...)
}

func (r *Runtime) pauseBatchBeforeRollback(ctx context.Context, t Ticket, p *merge.Progress, reason string) error {
	if p.Deployment.Batch == nil {
		return nil
	}
	if err := r.stopGuard(ctx, deploymentSHA(p)); err != nil {
		return err
	}
	return r.pauseDeployment(ctx, t, p, reason)
}

func (r *Runtime) loadDeployBatch(ctx context.Context, rows []Ticket, binding *merge.DeploymentBatch) (*deployBatch, error) {
	batch := &deployBatch{}
	for _, m := range binding.Members {
		t := Ticket{ID: m.TicketID, Repo: rows[0].Repo}
		for _, row := range rows {
			if row.ID == m.TicketID {
				t = row
				break
			}
		}
		if t.Identifier == "" {
			issue, err := r.Clients[t.Repo].Read(ctx, t.ID)
			if err != nil {
				return nil, err
			}
			t.Identifier = issue.Identifier
		}
		fresh, err := r.refreshBatchTicket(ctx, t)
		if err != nil {
			return nil, err
		}
		p := fresh.Progress
		if p == nil || p.QueueID != m.QueueID || p.MergeSHA != m.MergeSHA || p.Repository != rows[0].Handoff.Repository || p.Deployment == nil || !p.Deployment.Self || deploymentBatchBinding(p.Deployment) != nil && !reflect.DeepEqual(p.Deployment.Batch, binding) {
			return nil, errors.New("batch member identity changed or missing")
		}
		batch.tickets = append(batch.tickets, fresh)
	}
	return batch, nil
}

func (r *Runtime) recoverDeployBatch(ctx context.Context, t Ticket, p *merge.Progress, body string, dry bool) error {
	batch, err := r.loadDeployBatch(ctx, []Ticket{t}, p.Deployment.Batch)
	if err != nil {
		return err
	}
	if p.Deployment.Recovery.Action == "retry-smoke" {
		reserved := false
		for _, member := range batch.tickets {
			d := member.Progress.Deployment
			reserved = reserved || d.SmokeStarted || d.Recovery != nil && d.Recovery.Action == "retry-smoke"
		}
		if !reserved {
			return rejectDeploymentProposal("no batch smoke execution to reconcile")
		}
	}
	for _, member := range batch.tickets {
		next := *member.Progress
		next.Deployment = p.Deployment
		if !deploymentTransition(member.Progress, &next) {
			return rejectDeploymentProposal("batch member does not authorize this recovery")
		}
	}
	if dry {
		return nil
	}
	var problems []error
	for _, member := range batch.tickets {
		next := *member.Progress
		next.Deployment = p.Deployment
		raw, _ := json.Marshal(next)
		receipt := "阶段: 排队合并\n归属: 合并通道\n" + merge.ProgressPrefix + string(raw)
		if member.ID == t.ID {
			receipt = body
		}
		problems = append(problems, r.mergeComment(ctx, member, receipt))
	}
	return errors.Join(problems...)
}

// Reuse the durable decision when its fanout was interrupted. A new timestamp
// cannot authorize another transition of a member that already applied it.
func (r *Runtime) replayBatchRecovery(ctx context.Context, t Ticket, action, operator, reason string) (*merge.DeploymentProgress, error) {
	batch, err := r.loadDeployBatch(ctx, []Ticket{t}, t.Progress.Deployment.Batch)
	if err != nil {
		return nil, err
	}
	state := ""
	switch action {
	case "confirm-passed":
		state = "passed"
	case "retry-smoke":
		state = "deploy-intent"
	case "retry-workflow":
		state = "pending"
	default:
		return nil, nil
	}
	var replay *merge.DeploymentProgress
	for _, member := range batch.tickets {
		d := member.Progress.Deployment
		recovery := d.Recovery
		if d.State != state || recovery == nil || recovery.Action != action || recovery.Operator != operator || recovery.Reason != reason {
			continue
		}
		if replay != nil && !reflect.DeepEqual(replay, d) {
			return nil, rejectDeploymentProposal("conflicting batch recovery decisions")
		}
		copy := *d
		replay = &copy
	}
	return replay, nil
}
