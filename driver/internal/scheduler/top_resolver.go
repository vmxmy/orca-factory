package scheduler

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

const topFailureLimit = 2

// autoMode never hands a ticket to a human: it implies every automatic review
// and parks what the final resolver cannot settle.
func autoMode(env map[string]string) bool {
	return env["AGENT_MODE"] == "auto"
}

// topLimit keeps auto mode's final resolver on its tier up to the ordinary
// resolver retry limit before the ticket parks.
func topLimit(env map[string]string) int {
	if autoMode(env) {
		return resolverFailureLimit
	}
	return topFailureLimit
}

const topResolverPrompt = "你是该票的最终决策者，只有下列情形才转人：" + intake.TopMustEscalate + "。可提交 ruling（reason 为单行裁定理由）；扩 SCOPE 使用 scope-extension 提案并遵守原限制。不能写 Done、合并或批准取消。"

func (r *Runtime) applyTopResolution(ctx context.Context, t Ticket, p *resolver.Record, decision resolver.Resolution) error {
	refusal := ""
	switch {
	case decision.Action == "ruling" && p.Outcome != "succeeded":
		refusal = "终审提案未成功完成"
	case t.MergeProblem != "":
		refusal = "证据冲突"
	case p.Failures >= topLimit(r.Repositories[t.Repo].Env):
		refusal = "终审自己的第二次失败"
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if refusal == "" {
		var err error
		refusal, err = intake.ApplyTopReview(ctx, r.Clients[t.Repo], t.ID, p.Attempt, r.Repositories[t.Repo].Env["AGENT_LANE3_GLOBS"], now)
		if err != nil {
			return err
		}
	}
	if refusal != "" {
		return r.escalateResolution(ctx, t, p, "终审拒绝裁定："+refusal, decision.Reason)
	}
	return nil
}

func autoReviewEnabled(env map[string]string, name string) bool {
	if autoMode(env) && slices.Contains([]string{"scope", "release", "top"}, name) {
		return true
	}
	for _, flag := range strings.Split(env["AGENT_AUTO_REVIEW"], ",") {
		if strings.TrimSpace(flag) == name {
			return true
		}
	}
	return false
}
