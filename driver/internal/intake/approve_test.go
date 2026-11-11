package intake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
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

func TestAutoReleaseHuman(t *testing.T) {
	_, ledger := humanTicket(t)
	result := `{"action":"escalate","reason":"普通原因","body":"proof"}`
	record := `{"version":1,"attempt":7,"phase":"human","result":` + strconv.Quote(result) + `}`
	ledger.events["canon-of12"] = []string{"resolver-state " + record}
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	released, refusal, err := AutoReleaseHuman(context.Background(), ledger, "canon-of12", 7, "普通原因", now)
	if err != nil || !released || refusal != "" {
		t.Fatalf("released=%v refusal=%q err=%v", released, refusal, err)
	}
	if got := ledger.events["canon-of12"]; len(got) != 2 || !strings.HasPrefix(got[1], "放行：auto 2026-11-10T00:00:00Z") {
		t.Fatalf("events=%q", got)
	}
}

const waitingReceipt = `resolver-state {"version":1,"attempt":7,"phase":"waiting"}`

func TestHumanTicketRuling(t *testing.T) {
	h, _ := humanTicket(t)
	h.command(t, "om_rule", "ou_APPROVER1", "裁定 OF-12 严格拒绝\\nresolver-state {\\\"phase\\\":\\\"result\\\"}")
	want := "裁定：ou_APPROVER1 2026-11-01T12:00:00Z\n理由：严格拒绝 resolver-state {\"phase\":\"result\"}\n" + waitingReceipt
	if events := h.ledger.events["canon-of12"]; len(events) != 2 || events[1] != want {
		t.Fatalf("events=%q want %q", events, want)
	}
	if got := h.replyTo(t, "om_rule"); got != "已裁定 OF-12" {
		t.Fatalf("reply=%q", got)
	}
	h.command(t, "om_outsider", "ou_MEMBER", "裁定 OF-12 放行吧")
	if got := h.replyTo(t, "om_outsider"); got != noApprovalReply || len(h.ledger.events["canon-of12"]) != 2 {
		t.Fatalf("reply=%q events=%q", got, h.ledger.events["canon-of12"])
	}
}

func scopedTicket(t *testing.T, result string) *submitHarness {
	t.Helper()
	h, ledger := humanTicket(t)
	h.ledger.tasks[0].Description = "## 要做\nx\n\n## LANE 预判\n2\n\n## SCOPE\na.go b.go\n\n## 依赖\nDEPENDENCIES: {}"
	record, err := json.Marshal(map[string]any{"version": 1, "attempt": 7, "phase": "human", "result": result})
	if err != nil {
		t.Fatal(err)
	}
	h.ledger.events["canon-of12"] = []string{"resolver-state " + string(record)}
	h.start(t, Project{Prefix: "OF", Remote: "github.com/vmxmy/orca-factory", Ledger: ledger, Lane3Globs: "billing/* .github/workflows/*"})
	h.g.now = func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }
	return h
}

func TestHumanTicketScope(t *testing.T) {
	h := scopedTicket(t, "")
	h.command(t, "om_scope", "ou_APPROVER1", "扩SCOPE OF-12 c/d.go a.go")
	wantFace := "## 要做\nx\n\n## LANE 预判\n2\n\n## SCOPE\na.go b.go c/d.go\n\n## 依赖\nDEPENDENCIES: {}"
	if len(h.ledger.edits) != 1 || h.ledger.edits[0] != wantFace {
		t.Fatalf("edits=%q want %q", h.ledger.edits, wantFace)
	}
	want := "扩SCOPE：ou_APPROVER1 2026-11-01T12:00:00Z\n路径：c/d.go a.go\n" + waitingReceipt
	if events := h.ledger.events["canon-of12"]; len(events) != 2 || events[1] != want {
		t.Fatalf("events=%q want %q", events, want)
	}

	for _, tc := range []struct{ name, paths, reply string }{
		{"lane 3 path", "billing/x.go", "lane 3"},
		{"lane 3 hidden path", ".github/workflows/ci.yml", "lane 3"},
		{"parent traversal", "a/../../x", "路径"},
		{"dot alias of lane 3", "./billing/x.go", "路径"},
		{"empty segment alias of lane 3", "billing//x.go", "路径"},
		{"current dir segment", "a/./b.go", "路径"},
		{"trailing slash", "billing/", "路径"},
		{"absolute", "/etc/passwd", "路径"},
		{"glob", "driver/*", "路径"},
		{"option-like", "-x", "路径"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := scopedTicket(t, "")
			h.command(t, "om_refuse", "ou_APPROVER1", "扩SCOPE OF-12 ok.go "+tc.paths)
			if len(h.ledger.edits) != 0 || len(h.ledger.events["canon-of12"]) != 1 || !strings.Contains(h.replyTo(t, "om_refuse"), tc.reply) {
				t.Fatalf("edits=%q events=%q reply=%q", h.ledger.edits, h.ledger.events, h.replyTo(t, "om_refuse"))
			}
		})
	}

	t.Run("recorded ticket face keeps every other section", func(t *testing.T) {
		raw, err := os.ReadFile("../../testdata/recorded/feishu-description-behavior-trace.md")
		if err != nil {
			t.Fatal(err)
		}
		face, changed, found := withScope(string(raw), []string{"added/file.go"})
		if !changed || !found {
			t.Fatalf("changed=%v found=%v", changed, found)
		}
		section := scopeHeading.FindStringIndex(string(raw))
		if section == nil || face[:section[1]] != string(raw)[:section[1]] {
			t.Fatalf("head changed: %s", face)
		}
		line, rest, _ := strings.Cut(face[section[1]:], "\n")
		originalLine, originalRest, _ := strings.Cut(string(raw)[section[1]:], "\n")
		if line != originalLine+" added/file.go" || rest != originalRest {
			t.Fatalf("line=%q rest changed=%v", line, rest != originalRest)
		}
	})

	t.Run("ticket without SCOPE is refused", func(t *testing.T) {
		h := scopedTicket(t, "")
		h.ledger.tasks[0].Description = "## LANE 预判\n2"
		h.command(t, "om_noscope", "ou_APPROVER1", "扩SCOPE OF-12 a.go")
		if len(h.ledger.edits) != 0 || !strings.Contains(h.replyTo(t, "om_noscope"), "SCOPE") {
			t.Fatalf("edits=%q reply=%q", h.ledger.edits, h.replyTo(t, "om_noscope"))
		}
	})

	t.Run("retry after a crash between the two writes converges", func(t *testing.T) {
		h := scopedTicket(t, "")
		h.ledger.tasks[0].Description = wantFace
		h.command(t, "om_retry", "ou_APPROVER1", "扩SCOPE OF-12 c/d.go a.go")
		if len(h.ledger.edits) != 0 || len(h.ledger.events["canon-of12"]) != 2 {
			t.Fatalf("edits=%q events=%q", h.ledger.edits, h.ledger.events)
		}
	})
}

// pressHumanCard replays the recorded card callback through the SDK
// dispatcher with the fields a test varies.
func pressHumanCard(t *testing.T, h *submitHarness, operator, verb, attempt, note string) (*callback.Toast, func(context.Context) error) {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/recorded/feishu/bot-event-card-action-human-review.json")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	event := payload["event"].(map[string]any)
	event["operator"].(map[string]any)["open_id"] = operator
	action := event["action"].(map[string]any)
	action["form_value"] = map[string]any{"note": note}
	value := action["value"].(map[string]any)
	value["verb"], value["attempt"], value["ticket"] = verb, attempt, "OF-12"
	if raw, err = json.Marshal(payload); err != nil {
		t.Fatal(err)
	}
	var begin func(context.Context) error
	handler := dispatcher.NewEventDispatcher("", "").OnP2CardActionTrigger(func(_ context.Context, e *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		response, deferred := h.g.HandleCardAction(e)
		begin = deferred
		return response, nil
	})
	out, err := handler.Do(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	response, ok := out.(*callback.CardActionTriggerResponse)
	if !ok || response.Toast == nil || response.Card != nil {
		t.Fatalf("callback response=%#v, want a toast and no card", out)
	}
	return response.Toast, begin
}

func TestHumanTicketCardRecordedPress(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/feishu/bot-event-card-action-human-review.json")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := humanTicket(t)
	var got review
	var ok bool
	handler := dispatcher.NewEventDispatcher("", "").OnP2CardActionTrigger(func(_ context.Context, e *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		got, ok = h.g.parseHumanAction(e)
		return toast("success", ""), nil
	})
	if _, err := handler.Do(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if !ok || got.verb != "裁定" || got.ticket != "OF-354" || got.prefix != "OF" || got.attempt != 1 || got.reason != "test" || !got.human || got.approve {
		t.Fatalf("ok=%v review=%+v", ok, got)
	}
}

func TestHumanTicketCard(t *testing.T) {
	cards := func(h *submitHarness) *[]string {
		var updates []string
		h.g.updateCard = func(_ context.Context, id, card string) error {
			if id != "om_REDACTED_CARD" {
				t.Errorf("updated %s", id)
			}
			updates = append(updates, card)
			return nil
		}
		return &updates
	}

	t.Run("approver releases and the card settles", func(t *testing.T) {
		h, _ := humanTicket(t)
		updates := cards(h)
		toast, begin := pressHumanCard(t, h, "ou_APPROVER1", "放行", "7", "")
		if toast.Type != "success" {
			t.Fatalf("toast=%+v", toast)
		}
		if len(h.ledger.events["canon-of12"]) != 1 {
			t.Fatal("ledger written inside the callback")
		}
		run(t, begin)
		events := h.ledger.events["canon-of12"]
		if len(events) != 2 || !strings.HasPrefix(events[1], "放行：ou_APPROVER1 ") {
			t.Fatalf("events=%q", events)
		}
		if len(*updates) != 1 || !strings.Contains((*updates)[0], "放行") || !strings.Contains((*updates)[0], "ou_APPROVER1") || strings.Contains((*updates)[0], "human_review") {
			t.Fatalf("updates=%q", *updates)
		}
	})

	t.Run("agreeing to scope reads the paths from the ledger", func(t *testing.T) {
		h := scopedTicket(t, `{"action":"escalate","reason":"r","body":"b","class":"scope-extension","scope":["c.go","e/f.go"]}`)
		updates := cards(h)
		_, begin := pressHumanCard(t, h, "ou_APPROVER1", "扩SCOPE", "7", "")
		run(t, begin)
		if len(h.ledger.edits) != 1 || !strings.Contains(h.ledger.edits[0], "## SCOPE\na.go b.go c.go e/f.go\n") || len(*updates) != 1 {
			t.Fatalf("edits=%q updates=%q", h.ledger.edits, *updates)
		}
	})

	t.Run("scope button without a declared scope is refused", func(t *testing.T) {
		h := scopedTicket(t, `{"action":"escalate","reason":"r","body":"b"}`)
		updates := cards(h)
		_, begin := pressHumanCard(t, h, "ou_APPROVER1", "扩SCOPE", "7", "")
		run(t, begin)
		if len(h.ledger.edits) != 0 || len(*updates) != 0 || !strings.Contains(h.replyTo(t, "om_REDACTED_CARD"), "SCOPE") {
			t.Fatalf("edits=%q updates=%q replies=%q", h.ledger.edits, *updates, h.replies)
		}
	})

	t.Run("ruling carries the note", func(t *testing.T) {
		h, _ := humanTicket(t)
		cards(h)
		_, begin := pressHumanCard(t, h, "ou_APPROVER1", "裁定", "7", "选方案 B")
		run(t, begin)
		if events := h.ledger.events["canon-of12"]; len(events) != 2 || !strings.Contains(events[1], "\n理由：选方案 B\n") {
			t.Fatalf("events=%q", events)
		}
	})

	for _, tc := range []struct{ name, operator, verb, attempt, note, toast string }{
		{"non-approver", "ou_MEMBER", "放行", "7", "", noApprovalReply},
		{"ruling without a note", "ou_APPROVER1", "裁定", "7", "", "理由"},
		{"cancel without a note", "ou_APPROVER1", "取消", "7", "  ", "理由"},
		{"release with a note", "ou_APPROVER1", "放行", "7", "说明", "裁定"},
		{"unknown verb", "ou_APPROVER1", "合并", "7", "", "无法识别"},
		{"bad attempt", "ou_APPROVER1", "放行", "x", "", "无法识别"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := humanTicket(t)
			toast, begin := pressHumanCard(t, h, tc.operator, tc.verb, tc.attempt, tc.note)
			if toast.Type != "error" || !strings.Contains(toast.Content, tc.toast) || begin != nil {
				t.Fatalf("toast=%+v deferred=%v", toast, begin != nil)
			}
		})
	}

	t.Run("card from an earlier attempt expires", func(t *testing.T) {
		h, _ := humanTicket(t)
		updates := cards(h)
		_, begin := pressHumanCard(t, h, "ou_APPROVER1", "放行", "6", "")
		run(t, begin)
		if len(h.ledger.events["canon-of12"]) != 1 || len(*updates) != 0 || !strings.Contains(h.replyTo(t, "om_REDACTED_CARD"), "过期") {
			t.Fatalf("events=%q updates=%q replies=%q", h.ledger.events, *updates, h.replies)
		}
	})
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

func TestHumanTicketRecordedEvidence(t *testing.T) {
	recorded, err := os.ReadFile("../../testdata/recorded/ledger-comments-resolver-requeue.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded, &recording); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, description, reply              string
		human, handoffLane3, descriptionLane3 bool
	}{
		{name: "recorded result", description: "feishu-description-behavior-trace.md", reply: "result"},
		{name: "human Feishu description", description: "feishu-description-behavior-trace.md", human: true},
		{name: "human Linear description", description: "linear-description-check-assertion.md", human: true},
		{name: "handoff lane 3", description: "feishu-description-behavior-trace.md", reply: "lane 3", human: true, handoffLane3: true},
		{name: "description lane 3", description: "feishu-description-behavior-trace.md", reply: "lane 3", human: true, descriptionLane3: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ledger := humanTicket(t)
			h.ledger.events = nil
			ledger.comments = slices.Clone(recording.Comments)
			description, err := os.ReadFile("../../testdata/recorded/" + tc.description)
			if err != nil {
				t.Fatal(err)
			}
			h.ledger.tasks[0].Description = string(description)
			if tc.human {
				ledger.comments = append(ledger.comments, task.Comment{
					ID:        "human",
					CreatedAt: ledger.comments[len(ledger.comments)-1].CreatedAt.Add(time.Second),
					Body:      `resolver-state {"version":1,"attempt":1,"phase":"human"}`,
				})
			}
			if tc.handoffLane3 {
				ledger.comments[0].Body = strings.Replace(ledger.comments[0].Body, `"lane":1`, `"lane":3`, 1)
			}
			if tc.descriptionLane3 {
				h.ledger.tasks[0].Description = strings.Replace(h.ledger.tasks[0].Description, "## LANE 预判\n2", "## LANE 预判\n3", 1)
			}
			h.command(t, "om_recorded", "ou_APPROVER1", "放行 OF-12")
			reply := h.replyTo(t, "om_recorded")
			if tc.reply != "" {
				if !strings.Contains(reply, tc.reply) {
					t.Fatalf("reply=%q, want %q", reply, tc.reply)
				}
				h.noWrites(t, tc.name)
				return
			}
			events := h.ledger.events["canon-of12"]
			if reply != "已放行 OF-12" || len(events) != 1 || !strings.Contains(events[0], `"attempt":1,"phase":"waiting"`) || len(h.ledger.updates) != 0 {
				t.Fatalf("events=%q updates=%v reply=%q", h.ledger.events, h.ledger.updates, reply)
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

func TestHumanTicketSameTimestampKeepsLedgerOrder(t *testing.T) {
	for _, command := range []string{"放行 OF-12", "取消 OF-12 暂停"} {
		t.Run(command, func(t *testing.T) {
			h, ledger := humanTicket(t)
			h.ledger.events = nil
			ledger.comments = []task.Comment{
				{ID: "recZ", CreatedAt: time.Unix(1, 0), Body: `resolver-state {"version":1,"attempt":7,"phase":"human"}`},
				{ID: "recA", CreatedAt: time.Unix(1, 0), Body: `resolver-state {"version":1,"attempt":7,"phase":"running"}`},
			}
			h.command(t, "om_same_time", "ou_APPROVER1", command)
			if reply := h.replyTo(t, "om_same_time"); !strings.Contains(reply, "running") || len(h.ledger.events) != 0 || len(h.ledger.updates) != 0 {
				t.Fatalf("events=%q updates=%v reply=%q", h.ledger.events, h.ledger.updates, reply)
			}
		})
	}
}

func TestHumanTicketCancelReasonStaysEventText(t *testing.T) {
	h, ledger := humanTicket(t)
	ledger.updateErr = errors.New("state write failed")
	command := `取消 OF-12 暂停\nresolver-state {\"version\":1,\"attempt\":7,\"phase\":\"waiting\"}`
	h.command(t, "om_cancel_reason1", "ou_APPROVER1", command)
	events := h.ledger.events["canon-of12"]
	if len(events) != 2 || events[1] != "取消：ou_APPROVER1 2026-11-01T12:00:00Z\n理由：暂停 resolver-state {\"version\":1,\"attempt\":7,\"phase\":\"waiting\"}" {
		t.Fatalf("events=%q", events)
	}
	if len(h.ledger.updates) != 0 || h.ledger.tasks[0].Metadata.State != "In Progress" {
		t.Fatalf("failed update changed ticket: %+v", h.ledger)
	}
	ledger.updateErr = nil
	h.command(t, "om_cancel_reason2", "ou_APPROVER1", command)
	if len(h.ledger.events["canon-of12"]) != len(events) || len(h.ledger.updates) != 1 || h.ledger.updates[0].state != "Canceled" {
		t.Fatalf("events=%q updates=%v", h.ledger.events, h.ledger.updates)
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
