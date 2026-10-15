package intake

import (
	"slices"
	"strings"
	"testing"
)

const readyLabel = "ready-for-agent"

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
