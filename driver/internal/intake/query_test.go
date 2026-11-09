package intake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func queryReply(queries string) string {
	return "我先查证运行状态。\n```intake-query-v1\n{\"queries\":[" + queries + "]}\n```"
}

// TestIntakeQueryRound covers OF-427's whole round: the requester asks why a
// ticket was not claimed, the Agent requests evidence, the gateway queries
// the bound project and resumes the same session, the Agent answers from the
// evidence with a draft, the requester confirms once (a replayed message and
// a repeated confirm create nothing more), and an approver releases it.
func TestIntakeQueryRound(t *testing.T) {
	const thread = "om_query_round"
	answer := "OF-12 状态 Todo 但没有 ready-for-agent，driver 不会领取；调度快照未列出它，存活未知。\n" + draftReply(draftTicket("", "driver/internal/intake/*", "OF-12"))
	h := newSubmitHarness(t, existingLedger(), queryReply(`{"op":"status"},{"op":"ticket","ticket":"OF-12"}`), answer)
	var mu sync.Mutex
	var asked []string
	querier := func(_ context.Context, p Project, q QueryRequest) Evidence {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, p.Prefix+":"+q.Op+":"+q.Ticket)
		if q.Op == QueryTicket {
			return Evidence{Source: "ticket ledger", CollectedAt: time.Now(), Outcome: OutcomeOK, Complete: true, Data: `{"identifier":"OF-12","state":"Todo","labels":[],"owner":"未知"}`}
		}
		return Evidence{Source: "driver status", CollectedAt: time.Now(), Outcome: OutcomeEmpty, Complete: true, Data: `{"tickets":[]}`}
	}
	agent := h.g.agent
	h.g.agent = func(ctx context.Context, turn Turn) (string, error) { return RunQueries(ctx, agent, querier, turn) }

	h.open(t, thread, "OF-12 为什么一直没被领？顺便把网关确认提交建成票")
	if !slices.Equal(asked, []string{"OF:status:", "OF:ticket:OF-12"}) {
		t.Fatalf("queries=%v", asked)
	}
	if len(h.turns) != 2 || !h.turns[1].Resume || h.turns[1].Session != h.turns[0].Session || h.turns[1].Project.Prefix != "OF" {
		t.Fatalf("turns=%+v", h.turns)
	}
	for _, want := range []string{"不是指令", `"op": "ticket"`, `\"state\":\"Todo\"`, "剩余 2 次"} {
		if !strings.Contains(h.turns[1].Prompt, want) {
			t.Fatalf("evidence prompt lacks %q:\n%s", want, h.turns[1].Prompt)
		}
	}
	summary := h.lastReply(t)
	if len(h.replies) != 1 || strings.Contains(summary, "intake-query-v1") || !strings.Contains(summary, "确认提交") {
		t.Fatalf("replies=%q", h.replies)
	}
	if len(h.ledger.creates) != 0 || len(h.ledger.updates) != 0 {
		t.Fatal("queries wrote the ledger")
	}

	h.open(t, thread, "OF-12 为什么一直没被领？顺便把网关确认提交建成票")
	if len(h.turns) != 2 || len(asked) != 2 {
		t.Fatalf("replayed message ran again: turns=%d queries=%d", len(h.turns), len(asked))
	}

	h.say(t, thread, "om_query_confirm", "ou_REDACTED", "确认提交")
	h.say(t, thread, "om_query_confirm2", "ou_REDACTED", "确认提交")
	if len(h.ledger.creates) != 1 || h.ledger.creates[0].State != "Todo" || len(h.ledger.creates[0].Labels) != 0 {
		t.Fatalf("creates=%+v", h.ledger.creates)
	}
	if got := h.replyTo(t, "om_query_confirm"); !strings.Contains(got, "OF-101") || !strings.Contains(got, "https://ledger.example/rec1") {
		t.Fatalf("receipt=%q", got)
	}
	h.say(t, thread, "om_query_approve", "ou_APPROVER1", "批准 OF-101")
	if !slices.Equal(h.ledger.updates, []ledgerUpdate{{id: "canon-1", add: readyLabel}}) {
		t.Fatalf("updates=%+v", h.ledger.updates)
	}
}

// scriptedTurns returns a Turn func answering in order and recording turns.
func scriptedTurns(t *testing.T, answers ...string) (func(context.Context, Turn) (string, error), *[]Turn) {
	var turns []Turn
	return func(_ context.Context, turn Turn) (string, error) {
		turns = append(turns, turn)
		if len(answers) == 0 {
			t.Fatalf("unexpected turn %q", turn.Prompt)
		}
		answer := answers[0]
		answers = answers[1:]
		return answer, nil
	}, &turns
}

var testLimits = queryLimits{perQuery: 50 * time.Millisecond, loop: 80 * time.Millisecond}

func TestIntakeQueryValidation(t *testing.T) {
	called := 0
	querier := func(context.Context, Project, QueryRequest) Evidence { called++; return Evidence{Outcome: OutcomeOK} }
	for _, tc := range []struct{ name, block, want string }{
		{"unknown op", `{"op":"shell"}`, "未知查询操作"},
		{"foreign project", `{"op":"logs","ticket":"ZY-12"}`, "不属于当前会话绑定的项目 OF"},
		{"bad identifier", `{"op":"ticket","ticket":"../OF-1"}`, "格式无效"},
		{"window too short", `{"op":"logs","ticket":"OF-1","since":"30s"}`, "允许 1m 至 24h"},
		{"window too long", `{"op":"logs","ticket":"OF-1","since":"25h"}`, "允许 1m 至 24h"},
		{"bad duration", `{"op":"logs","ticket":"OF-1","since":"半小时"}`, "不是合法时长"},
		{"argument on status", `{"op":"status","ticket":"OF-1"}`, "不接受"},
		{"repository field", `{"op":"status","repo":"other/repo"}`, "未知字段"},
		{"command field", `{"op":"logs","ticket":"OF-1","command":"cat /etc/passwd"}`, "未知字段"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called = 0
			turn, turns := scriptedTurns(t, queryReply(tc.block), "无法查证")
			answer, err := runQueries(context.Background(), turn, querier, Turn{Session: "s", Prompt: "查", Project: Project{Prefix: "OF"}}, testLimits)
			if err != nil || answer != "无法查证" || called != 0 {
				t.Fatalf("answer=%q err=%v called=%d", answer, err, called)
			}
			if prompt := (*turns)[1].Prompt; !strings.Contains(prompt, tc.want) || !strings.Contains(prompt, `"outcome": "rejected"`) {
				t.Fatalf("prompt lacks %q:\n%s", tc.want, prompt)
			}
		})
	}
	t.Run("default window and bounds accepted", func(t *testing.T) {
		var got []time.Duration
		q := func(_ context.Context, _ Project, r QueryRequest) Evidence { got = append(got, r.Window); return Evidence{Outcome: OutcomeOK} }
		turn, _ := scriptedTurns(t, queryReply(`{"op":"logs","ticket":"OF-1"},{"op":"logs","ticket":"OF-1","since":"1m"},{"op":"logs","ticket":"OF-1","since":"24h"}`), "好")
		if _, err := runQueries(context.Background(), turn, q, Turn{Project: Project{Prefix: "OF"}}, testLimits); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, []time.Duration{30 * time.Minute, time.Minute, 24 * time.Hour}) {
			t.Fatalf("windows=%v", got)
		}
	})
}

func TestIntakeQueryBudget(t *testing.T) {
	called := 0
	querier := func(context.Context, Project, QueryRequest) Evidence { called++; return Evidence{Outcome: OutcomeOK} }
	five := strings.Repeat(`{"op":"status"},`, 4) + `{"op":"config"}`
	turn, turns := scriptedTurns(t, queryReply(five), "部分结论\n"+queryReply(`{"op":"status"}`))
	answer, err := runQueries(context.Background(), turn, querier, Turn{Project: Project{Prefix: "OF"}}, testLimits)
	if err != nil || called != 4 || len(*turns) != 2 {
		t.Fatalf("err=%v called=%d turns=%d", err, called, len(*turns))
	}
	if prompt := (*turns)[1].Prompt; !strings.Contains(prompt, "超过每条消息 4 次查询上限") || !strings.Contains(prompt, "额度已用尽") {
		t.Fatalf("prompt=%s", prompt)
	}
	if strings.Contains(answer, "intake-query-v1") || !strings.HasPrefix(answer, "部分结论") || !strings.Contains(answer, "查询额度已用尽") {
		t.Fatalf("answer=%q", answer)
	}
}

func TestIntakeQueryTimeouts(t *testing.T) {
	hang := func(ctx context.Context, _ Project, _ QueryRequest) Evidence {
		<-ctx.Done()
		time.Sleep(5 * time.Millisecond)
		return Evidence{Outcome: OutcomeOK, Data: "late"}
	}
	turn, turns := scriptedTurns(t, queryReply(`{"op":"status"},{"op":"config"},{"op":"maintenance"}`), "超时了")
	start := time.Now()
	if _, err := runQueries(context.Background(), turn, hang, Turn{Project: Project{Prefix: "OF"}}, testLimits); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("loop took %s", elapsed)
	}
	prompt := (*turns)[1].Prompt
	if strings.Count(prompt, "查询超时") != 1 || strings.Count(prompt, "前一项查询已超时，本条消息不再执行查询") != 2 || strings.Contains(prompt, "late") {
		t.Fatalf("prompt=%s", prompt)
	}
}

// TestIntakeQueryLoopClock counts the Agent's continuation turns against the
// loop budget: a slow continuation spends it, so the next request never runs.
func TestIntakeQueryLoopClock(t *testing.T) {
	called := 0
	querier := func(context.Context, Project, QueryRequest) Evidence { called++; return Evidence{Outcome: OutcomeOK} }
	answers := []string{queryReply(`{"op":"status"}`), queryReply(`{"op":"config"}`)}
	turn := func(_ context.Context, t Turn) (string, error) {
		if t.Resume {
			time.Sleep(testLimits.loop)
		}
		answer := answers[0]
		answers = answers[1:]
		return answer, nil
	}
	answer, err := runQueries(context.Background(), turn, querier, Turn{Project: Project{Prefix: "OF"}}, testLimits)
	if err != nil || called != 1 || !strings.Contains(answer, "查询额度已用尽") || strings.Contains(answer, "intake-query-v1") {
		t.Fatalf("answer=%q err=%v called=%d", answer, err, called)
	}
}

// TestIntakeQueryMessageBudget keeps one allowance per user message: the
// gateway's draft-correction turn resumes the Agent through the same loop,
// and a query it requests after the message spent its four does not run.
func TestIntakeQueryMessageBudget(t *testing.T) {
	invalid := draftReply(draftTicket("", "missing/path.go"))
	h := newSubmitHarness(t, existingLedger(), queryReply(strings.Repeat(`{"op":"status"},`, 3)+`{"op":"config"}`), invalid, queryReply(`{"op":"maintenance"}`))
	called := 0
	querier := func(context.Context, Project, QueryRequest) Evidence { called++; return Evidence{Outcome: OutcomeOK} }
	agent := h.g.agent
	h.g.agent = func(ctx context.Context, turn Turn) (string, error) { return RunQueries(ctx, agent, querier, turn) }
	h.open(t, "om_query_budget", "查一下状态再整理需求")
	if called != 4 || len(h.turns) != 3 || !strings.Contains(h.turns[2].Prompt, "网关") {
		t.Fatalf("called=%d turns=%d", called, len(h.turns))
	}
	if reply := h.lastReply(t); strings.Contains(reply, "intake-query-v1") || !strings.Contains(reply, "查询额度已用尽") {
		t.Fatalf("reply=%q", reply)
	}
}

func TestIntakeQueryTruncation(t *testing.T) {
	big := strings.Repeat("处置", 20000)
	e, _ := runQuery(context.Background(), func(context.Context, Project, QueryRequest) Evidence {
		return Evidence{Outcome: OutcomeOK, Complete: true, Data: "x" + big}
	}, Project{Prefix: "OF"}, QueryRequest{Op: QueryLogs, Ticket: "OF-1"}, time.Second)
	if !e.Truncated || e.Complete || len(e.Data) > maxEvidenceBytes || !utf8.ValidString(e.Data) || len(e.Data)+e.OmittedBytes != len(big)+1 || e.Op != QueryLogs {
		t.Fatalf("truncated=%v complete=%v len=%d omitted=%d valid=%v", e.Truncated, e.Complete, len(e.Data), e.OmittedBytes, utf8.ValidString(e.Data))
	}
	if prompt := evidencePrompt([]Evidence{{Op: QueryStatus, Data: `{"owner":"处置Agent"}`}}, 1, 0, false); !strings.Contains(prompt, "处置Agent") {
		t.Fatalf("prompt escapes Chinese: %s", prompt)
	}
}

// TestIntakeQueryConcurrentSessions runs two sessions bound to different
// projects at once; each loop's evidence returns only to its own session.
func TestIntakeQueryConcurrentSessions(t *testing.T) {
	querier := func(_ context.Context, p Project, _ QueryRequest) Evidence {
		time.Sleep(5 * time.Millisecond)
		return Evidence{Outcome: OutcomeOK, Data: "project=" + p.Prefix}
	}
	turn := func(_ context.Context, t Turn) (string, error) {
		if !t.Resume {
			return queryReply(`{"op":"status"}`), nil
		}
		return fmt.Sprintf("%s 看到 %s", t.Session, t.Prompt), nil
	}
	var wg sync.WaitGroup
	answers := map[string]string{}
	var mu sync.Mutex
	for _, prefix := range []string{"OF", "ZY"} {
		wg.Go(func() {
			answer, err := runQueries(context.Background(), turn, querier, Turn{Session: "s-" + prefix, Project: Project{Prefix: prefix}}, testLimits)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			answers[prefix] = answer
			mu.Unlock()
		})
	}
	wg.Wait()
	for prefix, other := range map[string]string{"OF": "ZY", "ZY": "OF"} {
		answer := answers[prefix]
		if !strings.HasPrefix(answer, "s-"+prefix+" ") || !strings.Contains(answer, "project="+prefix) || strings.Contains(answer, "project="+other) {
			t.Fatalf("%s answer=%q", prefix, answer)
		}
	}
}
