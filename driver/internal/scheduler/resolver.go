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

	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

type resolutionStore struct {
	runtime *Runtime
	ticket  Ticket
}

func readResolution(comments []linear.Comment) (*resolver.Record, error) {
	rows := append([]linear.Comment(nil), comments...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	var p *resolver.Record
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
			p = &next
		}
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
	contextComments := append([]linear.Comment(nil), comments...)
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
	evidence, err := json.Marshal(struct {
		Ticket   Ticket
		Comments []linear.Comment
	}{contextTicket, contextComments})
	if err != nil {
		return err
	}
	env := r.Repositories[t.Repo].Env
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
	if p.SourceRun != "" {
		inactive, readErr := r.NoActiveDispatch(ctx, Ticket{Run: p.SourceRun})
		if readErr != nil {
			return readErr
		}
		if !inactive {
			return r.retryResolution(ctx, t, p, "original worker still owns execution; reconcile its exact dispatch before completing resolution")
		}
	}
	decision, err := resolver.ParseResult(p.Result)
	if err != nil {
		return r.retryResolution(ctx, t, p, "invalid result; provide a structured, validated resolution")
	}
	if decision.Action != "merge" {
		if err = r.ResumeDeployment(ctx, t.Identifier, decision.Queue, decision.SHA, decision.Action, "resolver:"+p.Dispatch, decision.Reason, false); err != nil {
			if errors.Is(err, errDeploymentProposal) {
				return r.retryResolution(ctx, t, p, err.Error())
			}
			return err
		}
		// Ownership and deployment identity are written atomically by ResumeDeployment.
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
	return r.mergeComment(ctx, t, body)
}
func (r *Runtime) retryResolution(ctx context.Context, t Ticket, p *resolver.Record, reason string) error {
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
func resolverApproved(t Ticket) bool {
	if t.Resolution == nil || t.Resolution.Phase != "applied" || t.Handoff == nil {
		return false
	}
	p, err := resolver.ParseResult(t.Resolution.Result)
	return err == nil && p.Action == "merge" && p.Handoff.QueueID == t.Handoff.QueueID && p.Handoff.Head == t.Handoff.Head && p.Handoff.PR == t.Handoff.PR
}
