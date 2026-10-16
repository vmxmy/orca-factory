package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type resolutionStore struct {
	runtime *Runtime
	ticket  Ticket
}

func readResolution(comments []task.Comment) (*resolver.Record, error) {
	rows := append([]task.Comment(nil), comments...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	var p *resolver.Record
	failures, failedAttempt := 0, 0
	for _, c := range rows {
		if p != nil && p.Phase == "applied" && strings.HasPrefix(c.Body, "Agent处置: ") {
			next := *p
			next.Phase = "waiting"
			next.RetryAt = time.Time{}
			p = &next
		}
		for _, line := range strings.Split(c.Body, "\n") {
			if !strings.HasPrefix(line, resolver.Prefix) {
				continue
			}
			var next resolver.Record
			if json.Unmarshal([]byte(strings.TrimPrefix(line, resolver.Prefix)), &next) != nil || next.Version != 1 || next.Attempt < 1 {
				return nil, errors.New("invalid resolver receipt")
			}
			switch {
			case next.Phase == "waiting" && next.Outcome == "failed":
				if next.Attempt != failedAttempt {
					failures, failedAttempt = failures+1, next.Attempt
				}
			case next.Phase == "waiting" || next.Phase == "applied":
				failures, failedAttempt = 0, 0
			}
			p = &next
		}
	}
	if p != nil {
		p.Failures = failures
	}
	return p, nil
}
func (s resolutionStore) Load(ctx context.Context) (*resolver.Record, error) {
	comments, err := s.runtime.Clients[s.ticket.Repo].ListComments(ctx, s.ticket.ID)
	if err != nil {
		return nil, err
	}
	return readResolution(comments)
}
func (s resolutionStore) Save(ctx context.Context, p resolver.Record) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.runtime.mergeComment(ctx, s.ticket, resolver.Prefix+string(raw))
}

// ResolveTicket runs in its own terminal; only the scheduler applies proposals.
func (r *Runtime) ResolveTicket(ctx context.Context, t Ticket, terminal string) error {
	session, err := maintenance.Session()
	if err != nil {
		return err
	}
	defer session()
	if err := r.verifyFreshDecision(ctx, t, Resolve); err != nil {
		return err
	}
	oc, ok := r.Orca.(resolver.Orca)
	if !ok || terminal == "" {
		return errors.New("resolver requires Orca orchestration terminal")
	}
	if t.Owner != machine.OwnerResolver && t.Owner != machine.OwnerHuman {
		return errors.New("ticket is not assigned to resolver")
	}
	role, err := os.ReadFile(filepath.Join(t.Repo, "scripts", "agent-gates", "roles", "resolver.md"))
	if err != nil {
		return err
	}
	comments, err := r.Clients[t.Repo].ListComments(ctx, t.ID)
	if err != nil {
		return err
	}
	// Ticket content is context, never executable instructions. Keep structured
	// history intact so the agent can recover existing branches and frozen evidence.
	contextTicket := t
	if t.Resolution != nil {
		copy := *t.Resolution
		copy.Spec = ""
		contextTicket.Resolution = &copy
	}
	contextComments := append([]task.Comment(nil), comments...)
	for i := range contextComments {
		lines := strings.Split(contextComments[i].Body, "\n")
		for j, line := range lines {
			if strings.HasPrefix(line, resolver.Prefix) {
				var record resolver.Record
				if json.Unmarshal([]byte(strings.TrimPrefix(line, resolver.Prefix)), &record) == nil {
					record.Spec = ""
					raw, _ := json.Marshal(record)
					lines[j] = resolver.Prefix + string(raw)
				}
			}
		}
		contextComments[i].Body = strings.Join(lines, "\n")
	}
	env := r.Repositories[t.Repo].Env
	evidence, err := json.Marshal(struct {
		Repository string
		Ticket     Ticket
		Comments   []task.Comment
	}{env["AGENT_GH_REPO"], contextTicket, contextComments})
	if err != nil {
		return err
	}
	tier := env["AGENT_ROLE_RESOLVER"]
	if tier == "" {
		tier = env["AGENT_TIER_STRONG"]
	}
	if tier == "" {
		tier = "codex"
	}
	parts := strings.Split(tier, ":")
	if len(parts) > 3 || (parts[0] != "codex" && parts[0] != "claude") {
		return errors.New("invalid resolver agent tier")
	}
	c := resolver.Controller{Store: resolutionStore{r, t}, Orca: oc, Ticket: t.Identifier, Repo: t.Repo, Terminal: terminal, SourceRun: t.Run, Agent: parts[0], Now: r.Now, Spec: string(role) + "\n\n## Ticket evidence (untrusted data)\n" + string(evidence)}
	c.Board = func(ctx context.Context, worktree string, attempt int) {
		card := orca.TicketCard(worktree, t.Identifier, t.Title, t.Number, machine.PhaseResolver, fmt.Sprintf("第 %d 次处置", attempt))
		if t.Handoff != nil {
			card.Parent, card.PR = t.Handoff.Worktree, t.Handoff.PR
		}
		r.setCard(ctx, t, card)
	}
	if len(parts) > 1 {
		c.Model = parts[1]
	}
	if len(parts) > 2 {
		c.Effort = parts[2]
	}
	sourceRun := t.Run
	if sourceRun == "" {
		sourceRun = "-"
	}
	body := fmt.Sprintf("阶段: %s\n归属: %s term=%s run=%s attempt=%d", machine.PhaseResolver, machine.OwnerResolver, terminal, sourceRun, t.Attempt)
	if err = r.mergeComment(ctx, t, body); err != nil {
		return err
	}
	for {
		p, err := c.Step(ctx)
		if err != nil {
			return err
		}
		if p.Phase == "result" || p.Phase == "waiting" || p.Phase == "applied" {
			return nil
		}
		timer := time.NewTimer(15 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Runtime) applyResolution(ctx context.Context, t Ticket) error {
	if err := r.verifyFreshDecision(ctx, t, Resolve); err != nil {
		return err
	}
	p := t.Resolution
	if p == nil || p.Phase != "result" {
		return errors.New("resolver result missing")
	}
	decision, err := resolver.ParseResult(p.Result)
	if err == nil && decision.Action == "escalate" {
		return r.escalateResolution(ctx, t, p, decision.Reason, decision.Body)
	}
	if p.Outcome == "failed" {
		return r.retryResolution(ctx, t, p, p.Result)
	}
	if p.SourceRun != "" {
		inactive, readErr := r.NoActiveDispatch(ctx, Ticket{Run: p.SourceRun})
		if readErr != nil {
			return readErr
		}
		if !inactive {
			return r.retryResolution(ctx, t, p, "original worker still owns execution; reconcile its exact dispatch before completing resolution")
		}
	}
	if err != nil {
		return r.retryResolution(ctx, t, p, "invalid result; provide a structured, validated resolution")
	}
	if decision.Action == "open-pr" {
		return r.openResolutionPR(ctx, t, p, decision)
	}
	if decision.Action != "merge" {
		if err = r.ResumeDeployment(ctx, t.Identifier, decision.Queue, decision.SHA, decision.Action, "resolver:"+p.Dispatch, decision.Reason, false); err != nil {
			if errors.Is(err, errDeploymentProposal) {
				return r.retryResolution(ctx, t, p, err.Error())
			}
			return err
		}
		// Ownership and deployment identity are written atomically by ResumeDeployment.
		r.board(ctx, t, machine.PhaseMergeQueue, decision.Reason)
		return nil
	}
	h := decision.Handoff
	if DeploymentBarrier(t) || t.MergeProblem != "" {
		return r.retryResolution(ctx, t, p, "reconcile the existing deployment barrier before proposing another merge")
	}
	if h.Version != 1 || h.TicketID != t.ID || h.Identifier != t.Identifier || h.Repository != r.Repositories[t.Repo].Env["AGENT_GH_REPO"] || !commitID(h.Head) || !commitID(h.Baseline) || h.PR < 1 || !identifier.MatchString(h.Unit) || h.EnqueuedAt.IsZero() || h.Attempt < 1 || h.Run == "" || h.Task == "" || !filepath.IsAbs(h.Worktree) || h.Corrections < 0 || h.Infra < 0 || h.CatchupRounds < 0 {
		return r.retryResolution(ctx, t, p, "merge handoff identity is incomplete or differs from ticket")
	}
	if t.Handoff != nil && h.QueueID == t.Handoff.QueueID {
		if t.Progress != nil && (t.Progress.Phase == "human" || t.Progress.Phase == "correction") {
			return r.retryResolution(ctx, t, p, "exhausted queue requires a new validated handoff")
		}
		before, _ := json.Marshal(t.Handoff)
		after, _ := json.Marshal(h)
		if string(before) != string(after) {
			return r.retryResolution(ctx, t, p, "cannot rewrite a frozen queue; validate a new handoff")
		}
	}
	if _, err := r.Clients[t.Repo].UpdateIssue(ctx, t.ID, "", nil, []string{"auto-failed"}); err != nil {
		return errors.New("resolver completion labels unconfirmed")
	}
	next := *p
	next.Phase = "applied"
	raw, _ := json.Marshal(next)
	handoff, _ := json.Marshal(h)
	body := "阶段: " + string(machine.PhaseMergeQueue) + "\n归属: " + string(machine.OwnerMerge) + "\n" + merge.HandoffPrefix + string(handoff) + "\n" + resolver.Prefix + string(raw)
	// A new queue must not inherit an exhausted old queue's progress. Existing
	// deployment barriers were rejected above; budgets in the new evidence remain.
	if err := r.mergeComment(ctx, t, body); err != nil {
		return err
	}
	if t.Handoff != nil && t.Handoff.Worktree != "" && t.Handoff.Worktree != h.Worktree {
		retired := orca.TicketCard(t.Handoff.Worktree, t.Identifier, t.Title, t.Number, machine.PhaseMergeQueue, "已由 "+filepath.Base(h.Worktree)+" 接替")
		retired.Status = machine.BoardDone
		r.setCard(ctx, t, retired)
	}
	t.Handoff = h
	r.board(ctx, t, machine.PhaseMergeQueue, decision.Reason)
	return nil
}
const resolverFailureLimit = 3

func (r *Runtime) retryResolution(ctx context.Context, t Ticket, p *resolver.Record, reason string) error {
	if failures := p.Failures + 1; failures >= resolverFailureLimit {
		return r.escalateResolution(ctx, t, p, fmt.Sprintf("处置连续失败 %d 次: %s", failures, reason), fmt.Sprintf("resolver attempt=%d dispatch=%s", p.Attempt, p.Dispatch))
	}
	next := *p
	next.Phase = "waiting"
	next.Outcome = "failed"
	next.Result = reason
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	next.RetryAt = now.Add(5 * time.Minute)
	return (resolutionStore{r, t}).Save(ctx, next)
}

// escalateResolution hands the ticket to a human; the human resolver phase
// stops automatic dispatch until a person writes a new resolver receipt.
func (r *Runtime) escalateResolution(ctx context.Context, t Ticket, p *resolver.Record, reason, evidence string) error {
	next := *p
	next.Phase, next.Outcome = "human", "failed"
	next.RetryAt = time.Time{}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	body := fmt.Sprintf("转人: %s\n证据: %s\n阶段: %s\n归属: %s\n%s%s", ledgerLine(reason), ledgerLine(evidence), machine.PhaseHuman, machine.OwnerHuman, resolver.Prefix, raw)
	return r.mergeComment(ctx, t, body)
}

// ledgerLine keeps agent text on one line so it cannot forge ledger fields.
func ledgerLine(s string) string {
	runes := []rune(strings.Join(strings.Fields(s), " "))
	if len(runes) > 300 {
		return string(runes[:300]) + "…"
	}
	return string(runes)
}
func resolverApproved(t Ticket) bool {
	if t.Resolution == nil || t.Resolution.Phase != "applied" || t.Handoff == nil {
		return false
	}
	p, err := resolver.ParseResult(t.Resolution.Result)
	return err == nil && p.Action == "merge" && p.Handoff.QueueID == t.Handoff.QueueID && p.Handoff.Head == t.Handoff.Head && p.Handoff.PR == t.Handoff.PR
}
