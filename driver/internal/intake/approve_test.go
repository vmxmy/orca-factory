package intake

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/task"
)

const readyLabel = "ready-for-agent"

type humanLedger struct {
	*fakeLedger
	comments                       []task.Comment
	readErr, commentErr, updateErr error
}

func (l *humanLedger) ListComments(ctx context.Context, id string) ([]task.Comment, error) {
	if l.readErr != nil {
		return nil, l.readErr
	}
	rows, err := l.fakeLedger.ListComments(ctx, id)
	return append(slices.Clone(l.comments), rows...), err
}

func (l *humanLedger) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	if l.commentErr != nil {
		return task.Comment{}, l.commentErr
	}
	return l.fakeLedger.CreateComment(ctx, id, body)
}

func (l *humanLedger) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (task.Issue, error) {
	if len(l.events[id]) == 0 {
		return task.Issue{}, errors.New("state write before event")
	}
	if l.updateErr != nil {
		return task.Issue{}, l.updateErr
	}
	return l.fakeLedger.UpdateIssue(ctx, id, state, add, remove)
}

func humanTicket(t *testing.T) (*submitHarness, *humanLedger) {
	t.Helper()
	h := newSubmitHarness(t, existingLedger())
	h.ledger.tasks[0].Description = "## LANE 预判\n2"
	h.ledger.tasks[0].Metadata.State = "In Progress"
	h.ledger.events = map[string][]string{"canon-of12": {`resolver-state {"version":1,"attempt":7,"phase":"human"}`}}
	ledger := &humanLedger{fakeLedger: h.ledger}
	h.start(t, Project{Prefix: "OF", Remote: "github.com/vmxmy/orca-factory", Ledger: ledger})
	h.g.now = func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }
	return h, ledger
}

func TestHumanTicketRelease(t *testing.T) {
	h, _ := humanTicket(t)
	h.command(t, "om_release", "ou_APPROVER1", "放行 OF-12")
	events := h.ledger.events["canon-of12"]
	if len(events) != 2 || events[1] != "放行：ou_APPROVER1 2026-11-01T12:00:00Z\nresolver-state {\"version\":1,\"attempt\":7,\"phase\":\"waiting\"}" {
		t.Fatalf("events=%q", events)
	}
	if len(h.ledger.updates) != 0 || h.ledger.tasks[0].Metadata.State != "In Progress" || len(h.ledger.tasks[0].Labels.Nodes) != 0 {
		t.Fatalf("release changed ticket: %+v", h.ledger)
	}
	h.command(t, "om_repeat", "ou_APPROVER2", "放行 OF-12")
	if len(h.ledger.events["canon-of12"]) != 2 || !strings.Contains(h.replyTo(t, "om_repeat"), "waiting") {
		t.Fatalf("repeat events=%q reply=%q", h.ledger.events, h.replyTo(t, "om_repeat"))
	}
}

func TestHumanTicketReleaseRefusals(t *testing.T) {
	for _, tc := range []struct{ name, description, evidence, reply string }{
		{"lane3", "## LANE 预判\n3", "", "lane 3"},
		{"missing lane", "no lane", "", "LANE 预判"},
		{"handoff lane3", "", `merge-handoff-v1: {"version":1,"lane":3}`, "lane 3"},
		{"resolver corrupt", "", `resolver-state {`, "证据冲突"},
		{"resolver null", "", `resolver-state null`, "证据冲突"},
		{"handoff corrupt", "", `merge-handoff-v1: {`, "证据冲突"},
		{"progress corrupt", "", `merge-progress-v1: {`, "证据冲突"},
		{"paused", "", `merge-progress-v1: {"version":1,"deployment":{"state":"paused"}}`, "resume-deployment"},
		{"waiting", "", `resolver-state {"version":1,"attempt":8,"phase":"waiting"}`, "waiting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := humanTicket(t)
			if tc.description != "" {
				h.ledger.tasks[0].Description = tc.description
			}
			if tc.evidence != "" {
				h.ledger.events["canon-of12"] = append(h.ledger.events["canon-of12"], tc.evidence)
			}
			before := slices.Clone(h.ledger.events["canon-of12"])
			h.command(t, "om_refuse", "ou_APPROVER1", "放行 OF-12")
			if !slices.Equal(before, h.ledger.events["canon-of12"]) || len(h.ledger.updates) != 0 || !strings.Contains(h.replyTo(t, "om_refuse"), tc.reply) {
				t.Fatalf("events=%q updates=%v reply=%q", h.ledger.events, h.ledger.updates, h.replyTo(t, "om_refuse"))
			}
		})
	}
}

func TestHumanTicketLatestEvidence(t *testing.T) {
	h, ledger := humanTicket(t)
	h.ledger.events = nil
	ledger.comments = []task.Comment{
		{ID: "later", CreatedAt: time.Unix(2, 0), Body: `resolver-state {"version":1,"attempt":9,"phase":"human"}`},
		{ID: "earlier", CreatedAt: time.Unix(1, 0), Body: `resolver-state {"version":1,"attempt":8,"phase":"waiting"}`},
	}
	h.command(t, "om_latest", "ou_APPROVER1", "放行 OF-12")
	if events := h.ledger.events["canon-of12"]; len(events) != 1 || !strings.Contains(events[0], `"attempt":9,"phase":"waiting"`) {
		t.Fatalf("events=%q", events)
	}
}

func TestHumanTicketCancel(t *testing.T) {
	h, ledger := humanTicket(t)
	ledger.updateErr = errors.New("state write failed")
	h.command(t, "om_cancel1", "ou_APPROVER1", "取消 OF-12 暂不实施")
	if h.ledger.tasks[0].Metadata.State != "In Progress" {
		t.Fatal("failed update changed state")
	}
	ledger.updateErr = nil
	h.command(t, "om_cancel2", "ou_APPROVER2", "取消 OF-12 重发理由")
	h.command(t, "om_cancel3", "ou_APPROVER1", "取消 OF-12 暂不实施")
	events := h.ledger.events["canon-of12"]
	if len(events) != 2 || events[1] != "取消：ou_APPROVER1 2026-11-01T12:00:00Z\n理由：暂不实施" || len(h.ledger.updates) != 1 || h.ledger.updates[0].state != "Canceled" {
		t.Fatalf("events=%q updates=%v", events, h.ledger.updates)
	}
	if !strings.Contains(h.replyTo(t, "om_cancel3"), "Canceled") {
		t.Fatal(h.replyTo(t, "om_cancel3"))
	}
}

func TestHumanTicketReadAndWriteFailures(t *testing.T) {
	for _, command := range []string{"放行 OF-12", "取消 OF-12 暂停"} {
		for _, failure := range []string{"missing", "read", "write"} {
			t.Run(command+failure, func(t *testing.T) {
				h, ledger := humanTicket(t)
				switch failure {
				case "missing":
					h.ledger.events = nil
				case "read":
					ledger.readErr = errors.New("unavailable")
				case "write":
					ledger.commentErr = errors.New("unavailable")
				}
				before := slices.Clone(h.ledger.events["canon-of12"])
				h.command(t, "om_fail", "ou_APPROVER1", command)
				if !slices.Equal(before, h.ledger.events["canon-of12"]) || len(h.ledger.updates) != 0 {
					t.Fatal("failure wrote ledger")
				}
				if failure == "missing" && !strings.Contains(h.replyTo(t, "om_fail"), "resolver-state") {
					t.Fatal(h.replyTo(t, "om_fail"))
				}
			})
		}
	}
}

func TestHumanTicketAuthorization(t *testing.T) {
	for _, command := range []string{"放行 OF-12", "取消 OF-12 暂停"} {
		h, _ := humanTicket(t)
		h.command(t, "om_denied", "ou_MEMBER", command)
		if h.replyTo(t, "om_denied") != noApprovalReply || len(h.ledger.events["canon-of12"]) != 1 || len(h.ledger.updates) != 0 {
			t.Fatal("unauthorized command applied")
		}
	}
}

func TestHumanTicketCommandSyntax(t *testing.T) {
	for _, text := range []string{"放行 OF-12", "取消 OF-12 暂停", "取消 OF-12 多行\n理由"} {
		if _, ok := parseReview(text); !ok {
			t.Errorf("command ignored: %q", text)
		}
	}
	for _, text := range []string{"放行 OF-12 继续", "放行 OF-12\n继续", "取消 OF-12", "取消 OF-12   ", "取消 OF-12理由", "请放行 OF-12"} {
		if _, ok := parseReview(text); ok {
			t.Errorf("ordinary message consumed: %q", text)
		}
	}
}

// submitted runs a thread through draft and “确认提交”, leaving one intake
// ticket OF-101 (canon-1) in the ledger.
func submitted(t *testing.T, thread, scope string) *submitHarness {
	t.Helper()
	h := newSubmitHarness(t, existingLedger(), draftReply(draftTicket("", scope)))
	h.open(t, thread, "网关要能提交需求")
	h.say(t, thread, thread+"_confirm", "ou_REDACTED", "确认提交")
	if len(h.ledger.creates) != 1 {
		t.Fatalf("creates=%d", len(h.ledger.creates))
	}
	return h
}

// command sends text as a top-level message that @s the bot.
func (h *submitHarness) command(t *testing.T, id, sender, text string) {
	t.Helper()
	event := recordedEvent(t, "bot-event-message-at")
	event.Event.Message.MessageId = ptr(id)
	event.Event.Sender.SenderId.OpenId = ptr(sender)
	event.Event.Message.Content = ptr(`{"text":"@_user_1 ` + text + `"}`)
	h.handle(t, event)
}

// replyTo is the latest reply sent to message id.
func (h *submitHarness) replyTo(t *testing.T, id string) string {
	t.Helper()
	for i, target := range slices.Backward(h.targets) {
		if target == id {
			return h.replies[i]
		}
	}
	t.Fatalf("no reply to %s; replies=%q", id, h.replies)
	return ""
}

func (h *submitHarness) noWrites(t *testing.T, what string) {
	t.Helper()
	if len(h.ledger.updates) != 0 || len(h.ledger.events) != 0 {
		t.Fatalf("%s wrote the ledger: updates=%+v events=%v", what, h.ledger.updates, h.ledger.events)
	}
}

// TestIntakeApprove covers OF-16: an approver's exact “批准 OF-N” adds
// ready-for-agent once and records one approval event; a non-approver, a
// lane-3 ticket or a ticket the gateway did not submit is refused; “驳回
// OF-N <理由>” cancels the ticket; both notify the requester in the thread.
func TestIntakeApprove(t *testing.T) {
	t.Run("approver releases an intake ticket exactly once", func(t *testing.T) {
		const thread = "om_review1"
		h := submitted(t, thread, "driver/internal/intake/*")

		h.say(t, thread, "om_member_approve", "ou_MEMBER", "批准 OF-101")
		if got := h.replyTo(t, "om_member_approve"); got != "无审批权限" {
			t.Fatalf("member reply=%q", got)
		}
		h.noWrites(t, "member approval")

		h.say(t, thread, "om_approve1", "ou_APPROVER1", "批准 OF-101")
		if !slices.Equal(h.ledger.updates, []ledgerUpdate{{id: "canon-1", add: readyLabel}}) {
			t.Fatalf("updates=%+v", h.ledger.updates)
		}
		events := h.ledger.events["canon-1"]
		if len(events) != 1 || !strings.HasPrefix(events[0], "批准：ou_APPROVER1 ") {
			t.Fatalf("events=%q", events)
		}
		if got := h.replyTo(t, "om_approve1"); !strings.Contains(got, "已放行 OF-101") {
			t.Fatalf("approver reply=%q", got)
		}
		if got := h.replyTo(t, thread); !strings.Contains(got, `<at user_id="ou_REDACTED"></at>`) || !strings.Contains(got, "OF-101") || !strings.Contains(got, "放行") {
			t.Fatalf("requester notice=%q", got)
		}

		notices := len(h.replies)
		h.command(t, "om_approve2", "ou_APPROVER2", "批准 OF-101")
		if len(h.ledger.updates) != 1 || len(h.ledger.events["canon-1"]) != 1 {
			t.Fatalf("repeat approval wrote: updates=%+v events=%q", h.ledger.updates, h.ledger.events["canon-1"])
		}
		if got := h.replyTo(t, "om_approve2"); !strings.Contains(got, "OF-101 已放行") || len(h.replies) != notices+1 {
			t.Fatalf("repeat reply=%q replies=%q", got, h.replies[notices:])
		}

		// A crash after the event but before the label: retrying adds the
		// label without a second event.
		h.ledger.tasks[1].Labels.Nodes = nil
		h.say(t, thread, "om_approve3", "ou_APPROVER1", "批准 OF-101")
		if len(h.ledger.updates) != 2 || len(h.ledger.events["canon-1"]) != 1 {
			t.Fatalf("retry: updates=%+v events=%q", h.ledger.updates, h.ledger.events["canon-1"])
		}
	})

	t.Run("lane 3 is not released", func(t *testing.T) {
		h := submitted(t, "om_review2", ".env.example")
		h.command(t, "om_lane3", "ou_APPROVER1", "批准 OF-101")
		if got := h.replyTo(t, "om_lane3"); got != "lane 3 需人工实现，不放行" {
			t.Fatalf("reply=%q", got)
		}
		h.noWrites(t, "lane 3 approval")
	})

	t.Run("ticket without the intake marker is not released", func(t *testing.T) {
		h := submitted(t, "om_review3", "driver/*")
		h.command(t, "om_foreign", "ou_APPROVER1", "批准 OF-12")
		if got := h.replyTo(t, "om_foreign"); !strings.Contains(got, "不是需求助手提交的票") {
			t.Fatalf("reply=%q", got)
		}
		h.noWrites(t, "foreign approval")
	})

	t.Run("reject cancels and tells the requester why", func(t *testing.T) {
		const thread = "om_review4"
		h := submitted(t, thread, "driver/*")

		h.command(t, "om_member_reject", "ou_REDACTED", "驳回 OF-101 不想要了")
		if got := h.replyTo(t, "om_member_reject"); got != "无审批权限" {
			t.Fatalf("requester reject reply=%q", got)
		}
		h.noWrites(t, "requester reject")

		h.say(t, thread, "om_reject1", "ou_APPROVER1", "驳回 OF-101 范围太大，请拆票")
		if !slices.Equal(h.ledger.updates, []ledgerUpdate{{id: "canon-1", state: "Canceled"}}) {
			t.Fatalf("updates=%+v", h.ledger.updates)
		}
		events := h.ledger.events["canon-1"]
		if len(events) != 1 || !strings.HasPrefix(events[0], "驳回：ou_APPROVER1 ") || !strings.Contains(events[0], "范围太大，请拆票") {
			t.Fatalf("events=%q", events)
		}
		if got := h.replyTo(t, thread); !strings.Contains(got, `<at user_id="ou_REDACTED"></at>`) || !strings.Contains(got, "范围太大，请拆票") {
			t.Fatalf("requester notice=%q", got)
		}

		h.command(t, "om_reject2", "ou_APPROVER2", "驳回 OF-101 重复")
		h.command(t, "om_approve_canceled", "ou_APPROVER2", "批准 OF-101")
		if len(h.ledger.updates) != 1 || len(h.ledger.events["canon-1"]) != 1 {
			t.Fatalf("after reject: updates=%+v events=%q", h.ledger.updates, h.ledger.events["canon-1"])
		}
		if got := h.replyTo(t, "om_reject2"); !strings.Contains(got, "OF-101 已驳回") {
			t.Fatalf("repeat reject reply=%q", got)
		}
	})
}
