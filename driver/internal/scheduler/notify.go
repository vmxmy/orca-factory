package scheduler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
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
	settings, err := config.FeishuNotifySettings(env)
	if err != nil {
		return err
	}
	chat := strings.TrimSpace(settings["AGENT_FEISHU_NOTIFY_CHAT"])
	if chat == "" {
		return nil
	}
	reason = strings.Join(strings.Fields(reason), " ")
	digest := sha256.Sum256([]byte(reason))
	marker := fmt.Sprintf("通知: %s %x", category, digest[:6])
	if t.NotifyEvents[marker] {
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
	return sender.SendText(ctx, chat, fmt.Sprintf("%s %s\n类别: %s\n原因: %s\n%s", t.Identifier, t.Title, category, reason, t.URL))
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
	case t.Owner == machine.OwnerHuman:
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
