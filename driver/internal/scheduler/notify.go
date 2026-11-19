package scheduler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/status"
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
	if category == "搁置" {
		identity = t.ID
	}
	digest := sha256.Sum256([]byte(identity))
	marker := fmt.Sprintf("通知: %s %x", category, digest[:6])
	// A notified request already had its auto review; skip the ledger reread.
	if t.NotifyEvents[marker] {
		return nil
	}
	var refusals []string
	// Auto mode never hands a ticket to a human, so a human ticket here predates
	// the switch and keeps its manual handling: only explicit flags apply.
	enabled := func(name string) bool {
		return human && !t.Resolution.TopUsed && !t.Resolution.Top && explicitAutoReview(env, name)
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
	case t.Parked != "":
		return "搁置", t.Parked
	case t.MergeProblem != "":
		return "证据冲突", t.MergeProblem
	case t.Progress != nil && t.Progress.Deployment != nil && t.Progress.Deployment.State == "paused":
		reason := t.Progress.Deployment.Reason
		if reason == "" {
			reason = t.NotifyReason
		}
		return "部署失败", reason
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

func (r *Runtime) NotifyOrcaRuntime(ctx context.Context, ticket Ticket) error {
	return r.sendOrcaRuntimeText(ctx, ticket, "Orca 运行时故障：terminal create 连续返回 runtime_error，已暂停认领与派工，保留票据 attempt。暂停满 10 分钟后 driver 自动重启 orca-serve 服务，之后每 30 分钟最多一次、共 3 次；调度器探测成功后自动恢复。请查看 orca-serve 日志里的 profile 写入超时。")
}

// NotifyOrcaRestart reports one automatic orca-serve restart and its exit code.
func (r *Runtime) NotifyOrcaRestart(ctx context.Context, ticket Ticket, restart int, restartErr error) error {
	return r.sendOrcaRuntimeText(ctx, ticket, fmt.Sprintf("Orca 运行时故障：driver 第 %d/%d 次自动重启 orca-serve 服务，systemctl 退出码 %d；driver 将继续读取自动化状态或探测终端创建，确认恢复后继续调度。", restart, orcaRestartLimit, restartExitCode(restartErr)))
}

// restartExitCode is systemctl's exit code, or -1 when it did not exit on its own.
func restartExitCode(restartErr error) int {
	if restartErr == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(restartErr, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func (r *Runtime) NotifyOrcaRestartExhausted(ctx context.Context, ticket Ticket) error {
	return r.sendOrcaRuntimeText(ctx, ticket, fmt.Sprintf("Orca 自动重启 %d 次仍未恢复，需人工处理：Orca 仍不可达或 terminal create 探测仍失败，driver 不再重启 orca-serve，只继续读取自动化状态或探测。请查看 orca-serve 日志里的 profile 写入超时。", orcaRestartLimit))
}

// errNoticeNotSent marks a notice that failed before any request reached
// Feishu, so sending it again cannot duplicate it.
var errNoticeNotSent = errors.New("notice not sent")

func (r *Runtime) sendOrcaRuntimeText(ctx context.Context, ticket Ticket, text string) error {
	return sendNoticeText(ctx, r.Repositories[ticket.Repo].Env, text)
}

// sendNoticeText holds the maintenance write lock through the send, so a
// frozen driver sends nothing and a freeze waits for a send in flight.
func sendNoticeText(ctx context.Context, env map[string]string, text string) error {
	release, err := maintenance.Write()
	if err != nil {
		return fmt.Errorf("%w: %w", errNoticeNotSent, err)
	}
	defer release()
	settings, err := config.FeishuNotifySettings(env)
	if err != nil {
		return fmt.Errorf("%w: %w", errNoticeNotSent, err)
	}
	chat := strings.TrimSpace(settings["AGENT_FEISHU_NOTIFY_CHAT"])
	if chat == "" {
		return nil
	}
	sender, err := feishu.New(feishu.Options{Endpoint: settings["AGENT_FEISHU_ENDPOINT"], AppID: settings["AGENT_FEISHU_APP_ID"], AppSecret: settings["AGENT_FEISHU_APP_SECRET"]})
	if err != nil {
		return fmt.Errorf("%w: %w", errNoticeNotSent, err)
	}
	return sender.SendText(ctx, chat, text)
}

// NotifyHostMemory uses the machine notification chat because the hold spans repositories.
func (r *Runtime) NotifyHostMemory(ctx context.Context, memory status.HostMemory) error {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	return sendNoticeText(ctx, nil, fmt.Sprintf("内存吃紧，已暂停认领与新建终端 %s：可用量 %d bytes，some avg60 %.2f，限制来源 %s；用 systemd-cgls 与该切片的 memory.peak 找占用大户，读数恢复后自动放开。", now.Sub(memory.Since).Round(time.Second), memory.Reading.Available, memory.Reading.SomeAvg60, memory.Reading.Source))
}
