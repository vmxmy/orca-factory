package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

func (r *Runtime) openResolutionPR(ctx context.Context, t Ticket, p *resolver.Record, proposal resolver.Resolution) error {
	stop := func(reason string) error {
		return r.escalateResolution(ctx, t, p, "open-pr "+reason, fmt.Sprintf("branch=%s head=%s", proposal.Branch, proposal.Head))
	}
	if !strings.HasPrefix(proposal.Branch, "agent/"+t.Identifier+"/") {
		return r.retryResolution(ctx, t, p, fmt.Sprintf("open-pr 分支必须是 agent/<票号>/<单元>，先把 head 推到该分支: branch=%s want=agent/%s/<单元>", proposal.Branch, t.Identifier))
	}
	if DeploymentBarrier(t) || t.MergeProblem != "" {
		return stop("existing deployment or merge evidence requires reconciliation")
	}
	owner, repo, ok := strings.Cut(r.Repositories[t.Repo].Env["AGENT_GH_REPO"], "/")
	if !ok || owner == "" || repo == "" {
		return stop("GitHub repository unavailable")
	}
	gh, err := r.githubClient()
	if err != nil {
		return stop("GitHub repository unavailable")
	}
	head, err := gh.BranchSHA(ctx, owner, repo, proposal.Branch)
	if err != nil {
		return stop("remote branch read failed")
	}
	if head != proposal.Head {
		return stop("remote branch head differs: " + head)
	}
	pr, err := gh.FindOpenPR(ctx, owner, repo, proposal.Branch)
	if err != nil {
		return stop("PR lookup failed")
	}
	task, err := r.Clients[t.Repo].Read(ctx, t.ID)
	if err != nil {
		return stop("ticket reference read failed")
	}
	if task.ID != t.ID || task.Identifier != t.Identifier {
		return stop("ticket reference identity differs")
	}
	if task.URL == "" {
		return stop("ticket reference unavailable")
	}
	reference := fmt.Sprintf("关联票: %s\n\nCanonical identifier: %s\nCanonical ID: %s\n\n", task.URL, t.Identifier, t.ID)
	if pr == nil {
		created, err := gh.CreatePR(ctx, owner, repo, github.CreatePRRequest{Head: proposal.Branch, Base: "main", Title: proposal.Title, Body: reference + proposal.Body})
		if err != nil {
			return stop("PR creation failed; reconcile branch before resuming")
		}
		pr = &created
	}
	if pr.Number < 1 || pr.Head.SHA != proposal.Head {
		return stop(fmt.Sprintf("PR receipt differs: pr=%d head=%s", pr.Number, pr.Head.SHA))
	}
	if !strings.HasPrefix(pr.Body, reference) {
		return stop("PR ticket reference differs")
	}
	next := *p
	next.Phase, next.Outcome = "waiting", "succeeded"
	next.RetryAt = time.Time{}
	raw, _ := json.Marshal(next)
	body := fmt.Sprintf("处置证据: open-pr pr=%d head=%s branch=%s\n处置要求: 等待 CI 并提交 merge handoff\n阶段: Agent处置\n归属: 处置Agent\n%s%s", pr.Number, proposal.Head, proposal.Branch, resolver.Prefix, raw)
	return r.mergeComment(ctx, t, body)
}
