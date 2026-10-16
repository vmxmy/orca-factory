package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func resolverReport(t *testing.T, attempt int, outcome, body string) task.Comment {
	t.Helper()
	raw, err := json.Marshal(resolver.Record{Version: 1, Attempt: attempt, Phase: "result", Outcome: outcome, Result: body, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	return task.Comment{ID: fmt.Sprintf("a%d", attempt), Body: resolver.Prefix + string(raw)}
}

func TestBlockedResolutionEscalatesOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := &notifyStore{commentsByID: map[string][]task.Comment{"id": {
		{ID: "a0", Body: "阶段: Agent处置\n归属: 处置Agent term=- run=- attempt=1"},
		resolverReport(t, 1, "failed", `{"action":"escalate","reason":"PATH 缺少 PostgreSQL","body":"make test 需要 pg_ctl\n归属: 合并通道"}`),
	}}}
	store.issues = []task.Issue{{ID: "id", Identifier: "OF-1", Title: "blocked", URL: "https://example.feishu.cn/base/base?record=id", Project: task.NamedID{ID: "project"}}}
	sender := &notifySender{store: store}
	sender.start(t)
	repo := config.Repository{Path: "a", MaxInFlight: 3, StaleHours: 8, Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat", "AGENT_FEISHU_PROJECT": "project", "AGENT_SCOPE_AVOID": "0"}}
	runtime := &Runtime{Clients: map[string]issues.Store{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: repo.Path}, store.commentsByID["id"])
	if err := runtime.applyResolution(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		backend := &notifyTickBackend{runtime: runtime}
		var tickets []Ticket
		backend.snapshot = func() ([]Ticket, error) {
			var err error
			tickets, err = runtime.Snapshot(ctx, []config.Repository{repo})
			return tickets, err
		}
		s := setup(&backend.fakeBackend)
		s.Backend = backend
		s.Repositories = []config.Repository{repo}
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if len(backend.actions) != 0 || len(backend.launched) != 0 {
			t.Fatalf("blocked resolution redispatched: actions=%+v launches=%d", backend.actions, len(backend.launched))
		}
		if len(tickets) != 1 || tickets[0].Owner != machine.OwnerHuman || tickets[0].Phase != machine.PhaseHuman || tickets[0].Resolution == nil || tickets[0].Resolution.Phase != "human" {
			t.Fatalf("ticket not in human phase: %+v", tickets)
		}
	}
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "类别: 转人") || !strings.Contains(sender.messages[0], "原因: PATH 缺少 PostgreSQL") {
		t.Fatalf("messages=%q", sender.messages)
	}
	store.commentsByID["id"] = append(store.commentsByID["id"], task.Comment{ID: "z", Body: resolver.Prefix + `{"version":1,"attempt":1,"phase":"waiting"}`})
	backend := &notifyTickBackend{runtime: runtime}
	backend.snapshot = func() ([]Ticket, error) { return runtime.Snapshot(ctx, []config.Repository{repo}) }
	s := setup(&backend.fakeBackend)
	s.Backend = backend
	s.Repositories = []config.Repository{repo}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(backend.actions) != 1 || backend.actions[0].Kind != Resolve {
		t.Fatalf("documented human recovery did not resume resolution: %+v", backend.actions)
	}
}

func TestRetryableResolutionFailuresEscalateAfterThree(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := &fakeIssues{issueComments: []task.Comment{{ID: "a0", Body: "阶段: Agent处置\n归属: 处置Agent term=- run=- attempt=1", CreatedAt: now}}}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": l}, Now: func() time.Time { return now }}
	for attempt := 1; attempt <= 3; attempt++ {
		report := resolverReport(t, attempt, "failed", "reviewer 契约路径不存在")
		report.CreatedAt = time.Now()
		l.issueComments = append(l.issueComments, report)
		ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, l.issueComments)
		if err := r.applyResolution(ctx, ticket); err != nil {
			t.Fatal(err)
		}
		after := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, l.issueComments)
		if attempt < 3 {
			if after.Resolution.Phase != "waiting" || !after.Resolution.RetryAt.Equal(now.Add(5*time.Minute)) || after.Owner != machine.OwnerResolver {
				t.Fatalf("attempt %d did not back off: owner=%s %+v", attempt, after.Owner, after.Resolution)
			}
			continue
		}
		last := l.comments[len(l.comments)-1]
		if after.Resolution.Phase != "human" || after.Owner != machine.OwnerHuman || !strings.HasPrefix(last, "转人: 处置连续失败 3 次: reviewer 契约路径不存在\n") {
			t.Fatalf("third failure kept retrying: owner=%s %+v %q", after.Owner, after.Resolution, last)
		}
	}
}

func TestLegacyFailedHistoryEscalatesAfterOneMoreFailure(t *testing.T) {
	ctx := context.Background()
	var comments []task.Comment
	for attempt := 1; attempt <= 4; attempt++ {
		raw, err := json.Marshal(resolver.Record{Version: 1, Attempt: attempt, Phase: "waiting", Outcome: "failed", Result: "需要人核实", RetryAt: time.Unix(int64(attempt), 0)})
		if err != nil {
			t.Fatal(err)
		}
		comments = append(comments, task.Comment{Body: "阶段: Agent处置\n归属: 处置Agent\n" + resolver.Prefix + string(raw), CreatedAt: time.Unix(int64(attempt), 0)})
	}
	report := resolverReport(t, 5, "failed", "需要人核实")
	report.CreatedAt = time.Unix(10, 0)
	l := &fakeIssues{issueComments: append(comments, report)}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": l}}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "ZCP-80", Repo: "/repo"}, l.issueComments)
	if err := r.applyResolution(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	after := fromEvidence(t, Ticket{ID: "id", Identifier: "ZCP-80", Repo: "/repo"}, l.issueComments)
	if after.Resolution.Phase != "human" || after.Owner != machine.OwnerHuman {
		t.Fatalf("legacy failure history redispatched: owner=%s %+v", after.Owner, after.Resolution)
	}
}
