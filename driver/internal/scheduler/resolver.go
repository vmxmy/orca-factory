package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/executor"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/mergechannel"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

const resolverTransientRetryWindow = 5 * time.Minute

type resolutionStore struct {
	runtime *Runtime
	ticket  Ticket
}

func readResolution(comments []task.Comment) (*resolver.Record, error) {
	rows := append([]task.Comment(nil), comments...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	var p *resolver.Record
	failures, failedAttempt := 0, 0
	topUsed := false
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
			case next.Phase == "waiting" && (next.Outcome == resolver.Undispatched || next.Outcome == resolver.ModelUnavailable):
			case next.Phase == "waiting" || next.Phase == "applied":
				failures, failedAttempt = 0, 0
			}
			topUsed = topUsed || next.Top || next.TopUsed
			next.TopUsed = topUsed
			if next.Phase == "applied" || (next.Phase == "waiting" && next.Outcome == "succeeded") {
				next.Top = false
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

// Save compares only with the latest resolver record: a record can return to
// an earlier value (OF-116 clears a refused run), and matching that older copy
// would leave the newer record in force.
func (s resolutionStore) Save(ctx context.Context, p resolver.Record) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	line := resolver.Prefix + string(raw)
	client := s.runtime.Clients[s.ticket.Repo]
	comments, err := client.ListComments(ctx, s.ticket.ID)
	if err != nil {
		return errors.New("resolver receipt read failed")
	}
	before := resolutionLines(comments)
	if len(before) > 0 && before[len(before)-1] == line {
		return nil
	}
	body := line
	if p.TopModelAccepted && (len(before) == 0 || !strings.Contains(before[len(before)-1], `"top_model_accepted":true`)) {
		body = fmt.Sprintf("终审模型：%s 已接受 %s effort=%s；依据 worker-start launch.effective 与 worker 回报，dispatch=%s\n%s", p.Agent, p.Model, p.Effort, p.Dispatch, line)
	}
	if _, err = client.CreateComment(ctx, s.ticket.ID, body); err == nil {
		return nil
	}
	// A newer record from another writer may follow ours; finding ours after
	// the records read before the write still confirms it landed.
	if comments, err = client.ListComments(ctx, s.ticket.ID); err == nil {
		if after := resolutionLines(comments); len(after) > len(before) && slices.Contains(after[len(before):], line) {
			return nil
		}
	}
	return errors.New("resolver receipt write unconfirmed")
}

// resolutionLines lists every resolver record line in ledger time order.
func resolutionLines(comments []task.Comment) []string {
	rows := append([]task.Comment(nil), comments...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	var lines []string
	for _, c := range rows {
		for _, line := range strings.Split(c.Body, "\n") {
			if strings.HasPrefix(line, resolver.Prefix) {
				lines = append(lines, line)
			}
		}
	}
	return lines
}

// ResolveTicket runs in its own terminal; only the scheduler applies proposals.
func (r *Runtime) ResolveTicket(ctx context.Context, t Ticket, snapshot []Ticket, terminal string, heartbeat *orca.Heartbeat) error {
	session, err := maintenance.Session()
	if err != nil {
		return err
	}
	defer session()
	if err := r.verifyFreshDecision(ctx, t, Resolve); err != nil {
		return err
	}
	if p := t.Resolution; p != nil && p.Top && p.Phase == "waiting" && p.Failures >= topLimit(r.Repositories[t.Repo].Env) {
		return r.escalateResolution(ctx, t, p, "终审自己的第二次失败", p.Result)
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
	contextComments, omitted := newestEvidence(contextComments)
	env := r.Repositories[t.Repo].Env
	evidence, err := json.Marshal(struct {
		Repository      string
		Ticket          Ticket
		Comments        []task.Comment
		OmittedComments int             `json:",omitempty"`
		Related         []relatedTicket `json:",omitempty"`
	}{env["AGENT_GH_REPO"], contextTicket, contextComments, omitted, relatedTickets(t, snapshot)})
	if err != nil {
		return err
	}
	tierKey := "AGENT_ROLE_RESOLVER"
	tier := env[tierKey]
	if tier == "" {
		tierKey = "AGENT_TIER_STRONG"
		tier = env[tierKey]
	}
	if tier == "" {
		tierKey, tier = "", "codex"
	}
	top := t.Resolution != nil && t.Resolution.Top
	if top {
		tierKey = "AGENT_ROLE_RESOLVER_TOP"
		tier = env[tierKey]
		if t.Resolution.Agent != "" {
			recorded := strings.Join([]string{t.Resolution.Agent, t.Resolution.Model, t.Resolution.Effort}, ":")
			if t.Resolution.Phase != "waiting" {
				tier = recorded
			} else if recorded != tier {
				runx.LoggerFrom(ctx).Info("top resolver tier follows changed configuration", "ticket", t.Identifier, "previous", recorded, "configured", tier)
			}
		}
		role = append(role, []byte("\n\n"+topResolverPrompt)...)
	}
	// A dispatched round keeps its frozen record; only a new round selects.
	frozen := t.Resolution != nil && t.Resolution.Phase != "" && t.Resolution.Phase != "waiting"
	if frozen && t.Resolution.Agent != "" {
		tier = strings.Join([]string{t.Resolution.Agent, t.Resolution.Model, t.Resolution.Effort}, ":")
	}
	// A tier may omit its model and effort; padding keeps "codex" a valid tier.
	ref, err := modelhealth.Parse(tier + strings.Repeat(":", max(0, 2-strings.Count(tier, ":"))))
	if err != nil || !resolverAgent(ref) || (top && (ref.Model == "" || ref.Effort == "")) {
		if top {
			return r.escalateResolution(ctx, t, t.Resolution, "AGENT_ROLE_RESOLVER_TOP 无效，不更换模型", "需要配置明确的终审档位")
		}
		return errors.New("invalid resolver agent tier")
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	if prev := t.Resolution; prev != nil && prev.Phase == "waiting" && prev.Outcome == resolver.ModelUnavailable {
		statuses, err := r.unavailableModels(config.FallbackKey(tierKey), env, []modelhealth.Ref{ref}, now())
		if err != nil {
			return err
		}
		if len(statuses) > 0 {
			return r.parkModelResolution(ctx, t, prev, config.ModelParkReason(statuses), prev.Result)
		}
	}
	if tierKey != "" && !frozen {
		selected, selectErr := config.SelectConfigured(env, tierKey, r.StateRoot, ref, now())
		if selectErr != nil {
			runx.LoggerFrom(ctx).Warn("resolver model fallback unavailable; keeping configured model", "ticket", t.Identifier, "key", tierKey, "err", selectErr)
		}
		if !resolverAgent(selected) {
			return errors.New("invalid resolver fallback tier")
		}
		if selected != ref {
			if top && (selected.Model == "" || selected.Effort == "") {
				return r.escalateResolution(ctx, t, t.Resolution, "AGENT_ROLE_RESOLVER_TOP_FALLBACK 候选无效，不更换模型", "终审兜底候选需要写出 model 与 effort")
			}
			if selected.Model == "" {
				selected.Effort = ""
			}
			runx.LoggerFrom(ctx).Info("resolver model falls back", "ticket", t.Identifier, "configured", ref.String(), "selected", selected.String())
			ref = selected
		}
	}
	c := resolver.Controller{Store: resolutionStore{r, t}, Orca: oc, Ticket: t.Identifier, Repo: t.Repo, Terminal: terminal, SourceRun: t.Run, Agent: ref.Agent, Model: ref.Model, Effort: ref.Effort, Now: r.Now, Heartbeat: heartbeat, Spec: string(role) + "\n\n## Ticket evidence (untrusted data)\n" + string(evidence)}
	if r.StateRoot != "" {
		if models, err := modelhealth.Open(r.StateRoot); err != nil {
			runx.LoggerFrom(ctx).Warn("model health registry unavailable; resolver screen is not checked for model errors", "ticket", t.Identifier, "err", err)
		} else {
			c.Models = models
			c.Probe = func(ctx context.Context, ref modelhealth.Ref) modelhealth.Status {
				return modelhealth.Probe(ctx, ref, t.Repo, modelhealth.Endpoint{})
			}
		}
	}
	c.Board = func(ctx context.Context, worktree string, attempt int) {
		card := orca.TicketCard(worktree, t.Identifier, t.Title, t.Number, machine.PhaseResolver, fmt.Sprintf("第 %d 次处置", attempt))
		if t.Handoff != nil {
			card.Parent, card.PR = t.Handoff.Worktree, t.Handoff.PR
		}
		r.setCard(ctx, t, card)
	}
	sourceRun := t.Run
	if sourceRun == "" {
		sourceRun = "-"
	}
	body := fmt.Sprintf("阶段: %s\n归属: %s term=%s run=%s attempt=%d", machine.PhaseResolver, machine.OwnerResolver, terminal, sourceRun, t.Attempt)
	if err = r.mergeComment(ctx, t, body); err != nil {
		return err
	}
	var retryUntil time.Time
	var lastTransient error
	backoff := 2 * time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !retryUntil.IsZero() && !time.Now().Before(retryUntil) {
			stopResolverAfterTransientFailure(ctx, &c)
			return fmt.Errorf("resolver transient retry window exhausted: %w", lastTransient)
		}
		stepCtx := ctx
		cancel := func() {}
		if !retryUntil.IsZero() {
			stepCtx, cancel = context.WithDeadline(ctx, retryUntil)
		}
		p, err := c.Step(stepCtx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := 15 * time.Second
		if err != nil {
			if !retryUntil.IsZero() && !time.Now().Before(retryUntil) {
				stopResolverAfterTransientFailure(ctx, &c)
				return fmt.Errorf("resolver transient retry window exhausted: %w", lastTransient)
			}
			var transient *infra.Transient
			if !errors.As(err, &transient) {
				return err
			}
			lastTransient = err
			if retryUntil.IsZero() {
				retryUntil = time.Now().Add(resolverTransientRetryWindow)
			}
			delay = min(backoff, time.Until(retryUntil))
			backoff = min(backoff*2, 30*time.Second)
			runx.LoggerFrom(ctx).Warn("resolver transient failure; retrying", "ticket", t.Identifier, "err", err, "delay", delay)
		} else {
			retryUntil = time.Time{}
			backoff = 2 * time.Second
			if p.Phase == "result" || p.Phase == "waiting" || p.Phase == "applied" {
				r.wakeFor(ctx, t, *p)
				return nil
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func stopResolverAfterTransientFailure(ctx context.Context, c *resolver.Controller) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	logger := runx.LoggerFrom(ctx)
	p, err := c.Store.Load(ctx)
	if err != nil {
		logger.Warn("resolver cleanup receipt read failed", "ticket", c.Ticket, "err", err)
		return
	}
	// Only an active round still owns the dispatch; later records already released it or belong to a prior round.
	if p == nil || p.Dispatch == "" {
		return
	}
	switch p.Phase {
	case "pending", "running", "settled":
	default:
		return
	}
	request := func(step string) string {
		return orca.RequestID(c.Repo+"/"+c.Ticket, "resolver", step+"-"+c.Terminal, p.Attempt)
	}
	if _, err := c.Orca.WorkerStop(ctx, request("stop"), p.Dispatch); err != nil {
		logger.Warn("resolver worker stop failed after transient retry exhaustion", "dispatch", p.Dispatch, "err", err)
	}
	if _, err := c.Orca.WorkerRelease(ctx, request("release"), p.Dispatch); err != nil {
		logger.Warn("resolver worker release failed after transient retry exhaustion", "dispatch", p.Dispatch, "err", err)
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
	topScope := err == nil && p.Top && len(decision.ScopeRequest()) > 0
	if err == nil && decision.Action == "escalate" && !topScope {
		// Auto mode keeps the final resolver on its tier up to the retry limit.
		if p.Top && autoMode(r.Repositories[t.Repo].Env) {
			human, humanErr := r.mustHuman(ctx, t, decision.Class)
			if humanErr != nil {
				return humanErr
			}
			if !human {
				return r.retryResolution(ctx, t, p, decision.Reason)
			}
		}
		return r.escalateResolution(ctx, t, p, decision.Reason, decision.Body)
	}
	if p.Outcome == "failed" && !topScope {
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
	if p.Top && (decision.Action == "ruling" || len(decision.ScopeRequest()) > 0) {
		return r.applyTopResolution(ctx, t, p, decision)
	}
	if decision.Action == "ruling" {
		return r.retryResolution(ctx, t, p, "ruling 仅限终审处置")
	}
	if decision.Action == "open-pr" {
		return r.openResolutionPR(ctx, t, p, decision)
	}
	if decision.Action == "landed-done" {
		return r.applyLandedDone(ctx, t, p, decision)
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
	gh, err := r.githubClient()
	if err != nil {
		return err
	}
	owner, repo, _ := strings.Cut(h.Repository, "/")
	pr, err := gh.GetPR(ctx, owner, repo, h.PR)
	if err != nil {
		return err
	}
	if !mergechannel.PRMatchesHandoff(pr, h) {
		return r.retryResolution(ctx, t, p, fmt.Sprintf("PR #%d does not match the handoff: the merge lane accepts only an open PR from branch %s at head %s into main of %s; push that head to that branch and open a new PR", h.PR, mergechannel.HandoffBranch(h), h.Head, h.Repository))
	}
	description, err := r.Clients[t.Repo].Description(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("read ticket face for resolver gate_spec: %w", err)
	}
	face, err := executor.ParseTicket(t.ID, t.Identifier, t.Title, description)
	if err != nil {
		return r.retryResolution(ctx, t, p, "cannot validate gate_spec: ticket requires 要做, CHECK and SCOPE")
	}
	if problem := resolverGateSpecProblem(h.GateSpec, h.Unit, face); problem != "" {
		return r.retryResolution(ctx, t, p, problem)
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
	} else {
		rounds, err := laneCatchupRounds(t)
		if err != nil {
			return r.retryResolution(ctx, t, p, err.Error())
		}
		requeued := *h
		requeued.CatchupRounds = rounds
		h = &requeued
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
	// deployment barriers were rejected above; correction and infra budgets in
	// the new evidence remain, catch-up rounds come from the lane.
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

func (r *Runtime) applyLandedDone(ctx context.Context, t Ticket, p *resolver.Record, proposal resolver.Resolution) error {
	reject := func(reason string) error {
		return r.retryResolution(ctx, t, p, "landed-done rejected: "+reason)
	}
	if t.Handoff != nil && t.Handoff.Lane == 3 {
		return reject("lane 3 tickets cannot be completed by resolver")
	}
	if t.Handoff != nil || t.Progress != nil || t.MergeProblem != "" {
		return reject("ticket still has a merge handoff or frozen queue identity")
	}
	gh, err := r.githubClient()
	if err != nil {
		return err
	}
	owner, repo, ok := strings.Cut(r.Repositories[t.Repo].Env["AGENT_GH_REPO"], "/")
	if !ok || owner == "" || repo == "" {
		return reject("GitHub repository unavailable")
	}
	pr, err := gh.GetPR(ctx, owner, repo, proposal.PR)
	if err != nil {
		return err
	}
	if !pr.Merged || pr.MergeSHA != proposal.SHA || pr.Base.Ref != "main" {
		return reject(fmt.Sprintf("PR #%d is not merged to main at %s", proposal.PR, proposal.SHA))
	}
	mainSHA, err := gh.BranchSHA(ctx, owner, repo, "main")
	if err != nil {
		return err
	}
	comparison, err := gh.Compare(ctx, owner, repo, proposal.SHA, "main")
	if err != nil {
		return err
	}
	if comparison.MergeBaseCommit.SHA != proposal.SHA {
		return reject("merge SHA is not an ancestor of origin/main")
	}
	if mainSHA == "" {
		return reject("origin/main SHA is unavailable")
	}
	if !commitID(pr.Head.SHA) {
		return reject("merged PR head SHA is unavailable")
	}
	changed, err := gh.Compare(ctx, owner, repo, "main", pr.Head.SHA)
	if err != nil {
		return err
	}
	if len(changed.Files) == 0 || len(changed.Files) >= 300 {
		return reject(fmt.Sprintf("merged change file list is incomplete (%d files)", len(changed.Files)))
	}
	lane3 := strings.Fields(r.Repositories[t.Repo].Env["AGENT_LANE3_GLOBS"])
	driverChange := false
	for _, file := range changed.Files {
		for _, name := range []string{file.Filename, file.PreviousFilename} {
			if name == "" {
				continue
			}
			for _, pattern := range lane3 {
				if task.ScopeMatch(pattern, name) {
					return reject("merged change touches lane 3 path " + name)
				}
			}
			if strings.HasPrefix(name, "driver/") {
				driverChange = true
			}
		}
	}
	installed := "not involved"
	if driverChange {
		if !r.runningContains(ctx, t.Repo, proposal.SHA) {
			return reject("installed driver does not contain the merge SHA")
		}
		installed = "contains sha"
	}
	evidence := fmt.Sprintf("已落地: PR #%d squash %s\n核对: PR merged=true base=main merge_sha=%s; origin/main=%s ancestor=true; merge handoff=none; lane!=3; driver=%s", proposal.PR, proposal.SHA, proposal.SHA, mainSHA, installed)
	if err := r.mergeComment(ctx, t, evidence); err != nil {
		return err
	}
	if err := r.markDone(ctx, t); err != nil {
		return err
	}
	next := *p
	next.Phase = "applied"
	raw, _ := json.Marshal(next)
	return r.mergeComment(ctx, t, fmt.Sprintf("阶段: %s\n归属: %s\n%s", machine.PhaseMerged, machine.OwnerNone, resolver.Prefix+string(raw)))
}

// landedDonePending lets a confirmed Done ticket finish its ownership release
// when that last receipt failed after the state write.
func landedDonePending(t Ticket) bool {
	if t.Owner != machine.OwnerResolver || t.Resolution == nil || t.Resolution.Phase != "result" {
		return false
	}
	decision, err := resolver.ParseResult(t.Resolution.Result)
	return err == nil && decision.Action == "landed-done"
}

const resolverFailureLimit = 3

func (r *Runtime) retryResolution(ctx context.Context, t Ticket, p *resolver.Record, reason string) error {
	limit := resolverFailureLimit
	if p.Top {
		limit = topLimit(r.Repositories[t.Repo].Env)
	}
	if failures := p.Failures + 1; failures >= limit {
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
// stops automatic dispatch until a person writes a new resolver receipt. Auto
// mode parks the ticket instead.
func (r *Runtime) escalateResolution(ctx context.Context, t Ticket, p *resolver.Record, reason, evidence string) error {
	env := r.Repositories[t.Repo].Env
	class := ""
	if decision, err := resolver.ParseResult(p.Result); err == nil && decision.Action == "escalate" {
		class = decision.Class
	}
	if autoMode(env) {
		human, err := r.mustHuman(ctx, t, class)
		if err != nil {
			return err
		}
		if human {
			return r.parkResolution(ctx, t, p, reason, evidence)
		}
	}
	next := *p
	if !p.TopUsed && !p.Top && autoReviewEnabled(env, "top") {
		next.Top, next.TopUsed, next.TopReason, next.TopClass = true, true, reason+"\n"+evidence, class
		next.Agent, next.Model, next.Effort = "", "", ""
		next.Phase, next.Outcome, next.Result = "waiting", "", ""
		next.RetryAt = time.Time{}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		return r.mergeComment(ctx, t, "Agent处置: 终审处置\n阶段: Agent处置\n归属: 处置Agent\n"+resolver.Prefix+string(raw))
	}
	if autoMode(env) {
		return r.parkResolution(ctx, t, p, reason, evidence)
	}
	next.Phase, next.Outcome = "human", "failed"
	next.RetryAt = time.Time{}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	body := fmt.Sprintf("转人: %s\n证据: %s\n阶段: %s\n归属: %s\n%s%s", ledgerLine(reason), ledgerLine(evidence), machine.PhaseHuman, machine.OwnerHuman, resolver.Prefix, raw)
	return r.mergeComment(ctx, t, body)
}

const parkedLabel = "parked"

// unavailableModels returns each candidate's status when the registry marks
// primary and every candidate under fallbackKey unavailable, else nil.
func (r *Runtime) unavailableModels(fallbackKey string, env map[string]string, candidates []modelhealth.Ref, now time.Time) ([]modelhealth.Status, error) {
	fallback, err := config.ParseFallback(env[fallbackKey])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fallbackKey, err)
	}
	registry, err := modelhealth.Open(r.StateRoot)
	if err != nil {
		return nil, fmt.Errorf("model health registry: %w", err)
	}
	var statuses []modelhealth.Status
	for _, ref := range append(candidates, fallback...) {
		status := registry.Status(ref, now)
		if status.State != modelhealth.Unavailable {
			return nil, nil
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// ResumeModelParks restores tickets parked for model unavailability once the
// registry no longer marks every candidate named in the 搁置 line unavailable,
// as a person does: ready-for-agent returns and parked goes.
func (r *Runtime) ResumeModelParks(ctx context.Context, tickets []Ticket) error {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	var problems []error
	for _, t := range tickets {
		candidates, err := modelParkCandidates(t.Parked)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: model park candidate: %w", t.Identifier, err))
			continue
		}
		if len(candidates) == 0 {
			continue
		}
		statuses, err := r.unavailableModels("", nil, candidates, now())
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", t.Identifier, err))
			continue
		}
		if len(statuses) > 0 {
			continue
		}
		client := r.Clients[t.Repo]
		// The snapshot may predate a newer 搁置, such as lane 3 or a paused
		// deployment; only the ledger read now may lift this one.
		issue, err := client.FreshIssue(ctx, t.ID)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: model park reread: %w", t.Identifier, err))
			continue
		}
		comments, err := client.ListComments(ctx, t.ID)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: model park reread: %w", t.Identifier, err))
			continue
		}
		protocol := protocolComments(comments)
		if current, _ := parkedState(issue, protocol); current != t.Parked || !modelParkMarked(comments, t.Parked) {
			continue
		}
		recovery, err := client.CreateComment(ctx, t.ID, "恢复：模型候选已恢复可用，自动解除「"+config.ModelParked+"」搁置")
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: resume comment: %w", t.Identifier, err))
			continue
		}
		if _, err := client.UpdateIssue(ctx, t.ID, "", []string{"ready-for-agent"}, []string{parkedLabel}); err != nil {
			problems = append(problems, fmt.Errorf("%s: resume labels unconfirmed: %w", t.Identifier, err))
			if _, restoreErr := client.CreateComment(ctx, t.ID, "搁置："+t.Parked); restoreErr != nil {
				problems = append(problems, fmt.Errorf("%s: model park marker not restored: %w", t.Identifier, restoreErr))
			}
			continue
		}
		// The ledger has no compare-and-set: a person may park the ticket again
		// between the reread and the label write. Read once more and put the park
		// back if a newer 搁置 arrived.
		comments, err = client.ListComments(ctx, t.ID)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: resume verification failed; manual recovery required: %w", t.Identifier, err))
		}
		// Our recovery ends the old marker; exclude it only while checking for
		// another writer's intervening park or recovery.
		comments = slices.DeleteFunc(slices.Clone(comments), func(c task.Comment) bool {
			return c.ID == recovery.ID && c.Body == recovery.Body
		})
		if err != nil || latestPark(protocolComments(comments)) != t.Parked || !modelParkMarked(comments, t.Parked) {
			// An unverified resume is undone too: the next tick sees no park to retry.
			if _, restoreErr := client.UpdateIssue(ctx, t.ID, "", []string{parkedLabel}, []string{"ready-for-agent"}); restoreErr != nil {
				problems = append(problems, fmt.Errorf("%s: park not restored after unverified resume: %w", t.Identifier, errors.Join(err, restoreErr)))
			}
			continue
		}
	}
	return errors.Join(problems...)
}

// modelParkCandidates reads the candidates back from a config.ModelParkReason
// line; any other 搁置 reason names none.
func modelParkCandidates(reason string) ([]modelhealth.Ref, error) {
	listed, ok := strings.CutPrefix(reason, config.ModelParked+"：")
	if !ok {
		return nil, nil
	}
	var candidates []modelhealth.Ref
	for _, part := range strings.Split(listed, "；") {
		ref, err := modelhealth.Parse(strings.SplitN(part, " ", 2)[0])
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, ref)
	}
	return candidates, nil
}

// parkResolution parks with an applied resolver receipt; a later Agent处置
// event reopens it as a new attempt.
func (r *Runtime) parkResolution(ctx context.Context, t Ticket, p *resolver.Record, reason, evidence string) error {
	receipt, err := appliedReceipt(p)
	if err != nil {
		return err
	}
	return r.park(ctx, t, reason, evidence, receipt, false)
}

// parkModelResolution parks for the driver-built config.ModelParkReason that
// ResumeModelParks lifts once a candidate recovers.
func (r *Runtime) parkModelResolution(ctx context.Context, t Ticket, p *resolver.Record, reason, evidence string) error {
	receipt, err := appliedReceipt(p)
	if err != nil {
		return err
	}
	return r.park(ctx, t, reason, evidence, receipt, true)
}

func appliedReceipt(p *resolver.Record) (string, error) {
	next := *p
	next.Phase, next.Outcome = "applied", "failed"
	next.RetryAt = time.Time{}
	raw, err := json.Marshal(next)
	if err != nil {
		return "", err
	}
	return resolver.Prefix + string(raw), nil
}

// parkTicket stops automatic work without a human owner. Labels change first:
// a ticket must never lose its owner while it can still be claimed. A person
// resumes it by restoring ready-for-agent and removing parked.
func (r *Runtime) parkTicket(ctx context.Context, t Ticket, reason, evidence, receipt string) error {
	return r.park(ctx, t, reason, evidence, receipt, false)
}

// park writes a 搁置. Only a model park may start its line with the model
// prefix: other reasons carry agent text, which must never pass for a model
// park that the driver lifts by itself. A model line is kept whole, since
// ResumeModelParks reads every candidate back from it, and lands before the
// labels without an owner line, so a parked label never lacks the reason.
func (r *Runtime) park(ctx context.Context, t Ticket, reason, evidence, receipt string, model bool) error {
	line := ledgerLine(reason)
	if model {
		line = strings.Join(strings.Fields(reason), " ")
		if _, err := r.Clients[t.Repo].CreateComment(ctx, t.ID, "搁置："+line); err != nil {
			return fmt.Errorf("model park reason: %w", err)
		}
	} else if strings.HasPrefix(line, config.ModelParked) {
		line = "处置原因：" + line
	}
	if _, err := r.Clients[t.Repo].UpdateIssue(ctx, t.ID, "Todo", []string{parkedLabel}, []string{"ready-for-agent", "auto-impl", "auto-failed"}); err != nil {
		return fmt.Errorf("park labels unconfirmed: %w", err)
	}
	body := fmt.Sprintf("搁置：%s\n已完成：%s\n阶段: %s\n归属: %s\n%s", line, ledgerLine(evidence), machine.PhaseUnclaimed, machine.OwnerNone, receipt)
	if err := r.mergeComment(ctx, t, body); err != nil {
		return err
	}
	r.board(ctx, t, machine.PhaseUnclaimed, "搁置："+line)
	return nil
}

// modelParkMarked reports the reason-only 搁置 comment that only a model park
// writes before its labels, newer than the last 恢复 that lifted a park: a
// full 搁置 alone, as an older escalation reason reading like a model park, or
// a person parking again after a resume, never qualifies.
func modelParkMarked(comments []task.Comment, reason string) bool {
	rows := slices.Clone(comments)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	marked := false
	for _, c := range rows {
		switch {
		case strings.HasPrefix(c.Body, "恢复："):
			marked = false
		case c.CarriesProtocol() && c.Body == "搁置："+reason:
			marked = true
		}
	}
	return marked
}

// latestPark returns the reason of the newest 搁置 comment.
func latestPark(comments []task.Comment) string {
	reason, _ := parkedState(task.Issue{Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: parkedLabel}}}}, comments)
	return reason
}

// parkedState returns the latest 搁置 reason of a ticket labeled parked. Without
// the label, resumed reports that 搁置 is still the latest ownership event: a
// person restored the ticket.
func parkedState(issue task.Issue, comments []task.Comment) (reason string, resumed bool) {
	rows := append([]task.Comment(nil), comments...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	parkedLast := false
	for _, c := range rows {
		line, park := strings.CutPrefix(c.Body, "搁置：")
		if park {
			reason = strings.SplitN(line, "\n", 2)[0]
		}
		if park || strings.Contains("\n"+c.Body, "\n归属: ") {
			parkedLast = park
		}
	}
	if !slices.ContainsFunc(issue.Labels.Nodes, func(l task.NamedID) bool { return l.Name == parkedLabel }) {
		return "", parkedLast
	}
	if reason == "" {
		reason = "parked label"
	}
	return reason, false
}

// mustHuman reports what only a person may decide: a class from the table shared
// with the final resolver, lane 3 from the handoff or the ticket face, paused
// deployment or conflicting merge evidence.
func (r *Runtime) mustHuman(ctx context.Context, t Ticket, class string) (bool, error) {
	if intake.TopMustHuman(class) || t.MergeProblem != "" || (t.Handoff != nil && t.Handoff.Lane == 3) || (t.Progress != nil && t.Progress.Deployment != nil && t.Progress.Deployment.State == "paused") {
		return true, nil
	}
	description, err := r.Clients[t.Repo].Description(ctx, t.ID)
	if err != nil {
		return false, fmt.Errorf("read ticket face: %w", err)
	}
	return intake.FaceLaneThree(description), nil
}

// ledgerLine keeps agent text on one line so it cannot forge ledger fields.
func ledgerLine(s string) string {
	runes := []rune(strings.Join(strings.Fields(s), " "))
	if len(runes) > 300 {
		return string(runes[:300]) + "…"
	}
	return string(runes)
}

// resolverGateSpecProblem explains why a resolver gate_spec is not the one the
// executor renders from the ticket face, or returns "" when it is.
func resolverGateSpecProblem(spec, unit string, face executor.Ticket) string {
	newTest := "no"
	if face.NewTest {
		newTest = "yes"
	}
	expected := map[string]string{
		"UNIT": unit, "SCOPE": face.Scope, "CHECK": face.Check,
		"CHECK_IS_NEW_TEST": newTest, "CHECK_TEST_FILES": face.TestFiles,
		"CHECK_ASSERTION": face.Assertion, "LANE": strconv.Itoa(face.Lane),
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(spec, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// gate.sh reads `KEY:` only at line start and compares values exactly, so
		// lines must match the executor rendering byte for byte, not after trimming.
		key, value, ok := strings.Cut(line, ":")
		want, known := expected[key]
		if !ok || !known || seen[key] || value != " "+want && (want != "" || value != "") {
			return "gate_spec differs from driver-parsed ticket fields; use the ticket's gate spec"
		}
		seen[key] = true
	}
	for key, value := range expected {
		if !seen[key] && value != "" && (key != "CHECK_IS_NEW_TEST" || value != "no") {
			return "gate_spec missing driver-parsed ticket field: " + key
		}
	}
	return ""
}

func resolverApproved(t Ticket) bool {
	if t.Resolution == nil || t.Resolution.Phase != "applied" || t.Handoff == nil {
		return false
	}
	p, err := resolver.ParseResult(t.Resolution.Result)
	return err == nil && p.Action == "merge" && p.Handoff.QueueID == t.Handoff.QueueID && p.Handoff.Head == t.Handoff.Head && p.Handoff.PR == t.Handoff.PR
}

// relatedTicket indexes another ticket the resolver may read on demand; its
// full history stays out of this spec.
type relatedTicket struct {
	Identifier, Title string
	Phase             machine.Phase
	Owner             machine.Owner
	Reason            string
}

const relatedTicketLimit = 10

// relatedTickets lists the repository's other tickets held by the resolver or
// a human, newest first, so a shared cause is visible across tickets.
func relatedTickets(t Ticket, snapshot []Ticket) []relatedTicket {
	held := []Ticket{}
	for _, other := range snapshot {
		if other.Repo == t.Repo && other.ID != t.ID && (other.Owner == machine.OwnerResolver || other.Owner == machine.OwnerHuman) {
			held = append(held, other)
		}
	}
	sort.SliceStable(held, func(i, j int) bool { return held[i].UpdatedAt.After(held[j].UpdatedAt) })
	related := []relatedTicket{}
	for _, other := range held[:min(len(held), relatedTicketLimit)] {
		related = append(related, relatedTicket{other.Identifier, ledgerLine(other.Title), other.Phase, other.Owner, ledgerLine(other.NotifyReason)})
	}
	return related
}

// resolverEvidenceRunes bounds the comment evidence embedded in the resolver
// spec. Every resolver record persists that spec in one ledger cell, so an
// unbounded ticket history would overflow it.
const resolverEvidenceRunes = 40000

// newestEvidence keeps the newest non-empty comments that fit the evidence
// budget and reports how many it left out.
func newestEvidence(comments []task.Comment) ([]task.Comment, int) {
	kept := []task.Comment{}
	budget := resolverEvidenceRunes
	for i := len(comments) - 1; i >= 0; i-- {
		if strings.TrimSpace(comments[i].Body) == "" {
			continue
		}
		raw, _ := json.Marshal(comments[i])
		if budget -= utf8.RuneCount(raw); budget < 0 {
			break
		}
		kept = append(kept, comments[i])
	}
	slices.Reverse(kept)
	return kept, len(comments) - len(kept)
}

func resolverAgent(ref modelhealth.Ref) bool { return ref.Agent == "codex" || ref.Agent == "claude" }
