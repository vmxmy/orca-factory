package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

func (r *Runtime) openResolutionPR(ctx context.Context, t Ticket, p *resolver.Record, proposal resolver.Resolution) error {
	stop := func(reason string) error {
		next := *p
		next.Phase, next.Outcome = "human", "failed"
		next.RetryAt = time.Time{}
		raw, _ := json.Marshal(next)
		body := fmt.Sprintf("转人: open-pr %s\n证据: branch=%s head=%s\n阶段: 转人\n归属: 人\n%s%s", reason, proposal.Branch, proposal.Head, resolver.Prefix, raw)
		return r.mergeComment(ctx, t, body)
	}
	if DeploymentBarrier(t) || t.MergeProblem != "" {
		return stop("existing deployment or merge evidence requires reconciliation")
	}
	owner, repo, ok := strings.Cut(r.Repositories[t.Repo].Env["AGENT_GH_REPO"], "/")
	if !ok || owner == "" || repo == "" || r.GitHub == nil {
		return stop("GitHub repository unavailable")
	}
	head, err := r.GitHub.BranchSHA(ctx, owner, repo, proposal.Branch)
	if err != nil {
		return stop("remote branch read failed")
	}
	if head != proposal.Head {
		return stop("remote branch head differs: " + head)
	}
	url := t.URL
	if store, ok := r.Clients[t.Repo].(issues.Store); ok {
		task, err := store.Read(ctx, t.ID)
		if err != nil {
			return stop("ticket reference read failed")
		}
		if task.ID != t.ID || task.Identifier != t.Identifier {
			return stop("ticket reference identity differs")
		}
		url = task.URL
	}
	if url == "" {
		return stop("ticket reference unavailable")
	}
	pr, err := r.GitHub.FindOpenPR(ctx, owner, repo, proposal.Branch)
	if err != nil {
		return stop("PR lookup failed")
	}
	if pr == nil {
		body := fmt.Sprintf("关联票: %s\n\nCanonical identifier: %s\nCanonical ID: %s\n\n%s", url, t.Identifier, t.ID, proposal.Body)
		created, err := r.GitHub.CreatePR(ctx, owner, repo, github.CreatePRRequest{Head: proposal.Branch, Base: "main", Title: proposal.Title, Body: body})
		if err != nil {
			return stop("PR creation failed; reconcile branch before resuming")
		}
		pr = &created
	}
	if pr.Number < 1 || pr.Head.SHA != proposal.Head {
		return stop(fmt.Sprintf("PR receipt differs: pr=%d head=%s", pr.Number, pr.Head.SHA))
	}
	next := *p
	next.Phase, next.Outcome = "waiting", "succeeded"
	next.RetryAt = time.Time{}
	raw, _ := json.Marshal(next)
	body := fmt.Sprintf("处置证据: open-pr pr=%d head=%s branch=%s\n处置要求: 等待 CI 并提交 merge handoff\n阶段: Agent处置\n归属: 处置Agent\n%s%s", pr.Number, proposal.Head, proposal.Branch, resolver.Prefix, raw)
	return r.mergeComment(ctx, t, body)
}
