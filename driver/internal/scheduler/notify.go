package scheduler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

type textSender interface {
	SendText(context.Context, string, string) error
}

// NotifyFeishu reserves an event before sending. A failed or uncertain send
// keeps the reservation, including across driver restarts.
func (r *Runtime) NotifyFeishu(ctx context.Context, t Ticket) error {
	env := r.Repositories[t.Repo].Env
	if env["AGENT_ISSUE_BACKEND"] != "feishu" {
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
	category, reason := "", ""
	switch {
	case t.MergeProblem != "":
		category, reason = "证据冲突", t.MergeProblem
	case t.Progress != nil && t.Progress.Deployment != nil && t.Progress.Deployment.State == "paused":
		category, reason = "部署失败", t.Progress.Deployment.Reason
	case t.Owner == machine.OwnerHuman || t.Owner == machine.OwnerResolver:
		category = "转人"
	default:
		return nil
	}
	client := r.Clients[t.Repo]
	comments, err := client.ListComments(ctx, t.ID)
	if err != nil && category != "证据冲突" {
		return fmt.Errorf("notification event read: %w", err)
	}
	conflicted := err != nil
	if reason == "" {
		rows := append([]linear.Comment(nil), comments...)
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
		for _, c := range rows {
			for _, prefix := range []string{"转人: ", "Agent处置: "} {
				if strings.HasPrefix(c.Body, prefix) {
					reason = strings.SplitN(strings.TrimPrefix(c.Body, prefix), "\n", 2)[0]
				}
			}
		}
		if reason == "" && t.Progress != nil {
			reason = t.Progress.Reason
		}
		if reason == "" {
			reason = "等待处置"
		}
	}
	reason = strings.Join(strings.Fields(reason), " ")
	digest := sha256.Sum256([]byte(reason))
	marker := fmt.Sprintf("通知: %s %x", category, digest[:6])
	for _, c := range comments {
		if c.Body == marker {
			return nil
		}
	}
	if r.notifyClient == nil {
		r.notifyClient, err = feishu.New(feishu.Options{Endpoint: settings["AGENT_FEISHU_ENDPOINT"], AppID: settings["AGENT_FEISHU_APP_ID"], AppSecret: settings["AGENT_FEISHU_APP_SECRET"]})
		if err != nil {
			return err
		}
	}
	if conflicted {
		reader, ok := r.notifyClient.(interface {
			NotificationRecorded(context.Context, string, string, string) (bool, error)
		})
		if !ok {
			return errors.New("notification evidence reader unavailable")
		}
		exists, err := reader.NotificationRecorded(ctx, env["AGENT_FEISHU_BASE"], t.ID, marker)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
	}
	if _, err := client.CreateComment(ctx, t.ID, marker); err != nil {
		return fmt.Errorf("notification event write: %w", err)
	}
	return r.notifyClient.SendText(ctx, chat, fmt.Sprintf("%s %s\n类别: %s\n原因: %s\n%s", t.Identifier, t.Title, category, reason, t.URL))
}

type feishuEvidenceConflict struct {
	tickets []Ticket
	err     error
}

func (e *feishuEvidenceConflict) Error() string { return e.err.Error() }
func (e *feishuEvidenceConflict) Unwrap() error { return e.err }

func (s *Scheduler) notifyFeishu(ctx context.Context, t Ticket) {
	notifier, ok := s.Backend.(interface {
		NotifyFeishu(context.Context, Ticket) error
	})
	if !ok {
		return
	}
	if err := notifier.NotifyFeishu(ctx, t); err != nil {
		logger := s.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("Feishu notification failed", "ticket", t.Identifier, "error", err)
	}
}
