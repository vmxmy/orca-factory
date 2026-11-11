package scheduler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// NotifyFeishu reserves an event before sending. A failed or uncertain send
// keeps the reservation, including across driver restarts.
func (r *Runtime) NotifyFeishu(ctx context.Context, t Ticket) error {
	env := r.Repositories[t.Repo].Env
	if env["AGENT_ISSUE_BACKEND"] != "feishu" {
		return nil
	}
	category, reason := feishuNotification(t)
	if category == "" {
		return nil
	}
	human := category == "转人" && t.Resolution != nil && t.Resolution.Phase == "human"
	reason = strings.Join(strings.Fields(reason), " ")
	identity := reason
	if human {
		identity = fmt.Sprintf("%s attempt=%d", reason, t.Resolution.Attempt)
	}
	digest := sha256.Sum256([]byte(identity))
	marker := fmt.Sprintf("通知: %s %x", category, digest[:6])
	// A notified request already had its auto review; skip the ledger reread.
	if t.NotifyEvents[marker] {
		return nil
	}
	var refusals []string
	enabled := func(name string) bool {
		return human && !t.Resolution.TopUsed && !t.Resolution.Top && autoReviewEnabled(env, name)
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if enabled("scope") {
		refusal, err := intake.AutoApproveScope(ctx, r.Clients[t.Repo], t.ID, t.Resolution.Attempt, env["AGENT_LANE3_GLOBS"], now)
		if err != nil {
			return fmt.Errorf("auto scope review: %w", err)
		}
		if refusal == "" {
			return nil
		}
		refusals = append(refusals, "扩SCOPE："+refusal)
	}
	if enabled("release") {
		released, refusal, err := intake.AutoReleaseHuman(ctx, r.Clients[t.Repo], t.ID, t.Resolution.Attempt, reason, now)
		if err != nil {
			return fmt.Errorf("auto release review: %w", err)
		}
		if released || refusal == "" {
			return nil
		}
		refusals = append(refusals, "放行："+refusal)
	}
	autoRefusal := strings.Join(refusals, "；")
	settings, err := config.FeishuNotifySettings(env)
	if err != nil {
		return err
	}
	chat := strings.TrimSpace(settings["AGENT_FEISHU_NOTIFY_CHAT"])
	if chat == "" {
		return nil
	}
	client := r.Clients[t.Repo]
	sender, err := feishu.New(feishu.Options{Endpoint: settings["AGENT_FEISHU_ENDPOINT"], AppID: settings["AGENT_FEISHU_APP_ID"], AppSecret: settings["AGENT_FEISHU_APP_SECRET"]})
	if err != nil {
		return err
	}
	if _, err := client.CreateComment(ctx, t.ID, marker); err != nil {
		return fmt.Errorf("notification event write: %w", err)
	}
	if human {
		card := feishu.HumanCard{Ticket: t.Identifier, Title: t.Title, Reason: reason, URL: t.URL, Attempt: t.Resolution.Attempt, AutoReviewRefusal: autoRefusal}
		card.Scope = notificationScope(t)
		return sender.SendCard(ctx, chat, card.JSON())
	}
	body := fmt.Sprintf("%s %s\n类别: %s\n原因: %s\n%s", t.Identifier, t.Title, category, reason, t.URL)
	if category == "转人" {
		body += fmt.Sprintf("\n可用命令：@机器人 放行 %s 或 @机器人 取消 %s <理由>", t.Identifier, t.Identifier)
	}
	return sender.SendText(ctx, chat, body)
}

func feishuNotification(t Ticket) (string, string) {
	switch {
	case t.MergeProblem != "":
		return "证据冲突", t.MergeProblem
	case t.Progress != nil && t.Progress.Deployment != nil && t.Progress.Deployment.State == "paused":
		reason := t.Progress.Deployment.Reason
		if reason == "" {
			reason = t.NotifyReason
		}
		return "部署失败", reason
	case t.Parked != "":
		return "搁置", t.Parked
	case t.Owner == machine.OwnerHuman && (t.Resolution == nil || t.Resolution.Phase != "waiting"):
		return "转人", t.NotifyReason
	default:
		return "", ""
	}
}

func readFeishuNotification(t *Ticket, comments []task.Comment) {
	rows := append([]task.Comment(nil), comments...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	t.NotifyReason = ""
	t.NotifyEvents = map[string]bool{}
	for _, c := range rows {
		if strings.HasPrefix(c.Body, "通知: ") {
			t.NotifyEvents[c.Body] = true
		}
		for _, prefix := range []string{"转人: ", "Agent处置: "} {
			if strings.HasPrefix(c.Body, prefix) {
				t.NotifyReason = strings.SplitN(strings.TrimPrefix(c.Body, prefix), "\n", 2)[0]
			}
		}
	}
	if t.NotifyReason == "" && t.Progress != nil {
		t.NotifyReason = t.Progress.Reason
	}
	if t.NotifyReason == "" {
		t.NotifyReason = "等待处置"
	}
}

func notificationScope(t Ticket) []string {
	if t.Resolution == nil {
		return nil
	}
	decision, err := resolver.ParseResult(t.Resolution.Result)
	if err != nil {
		return nil
	}
	return decision.ScopeRequest()
}
