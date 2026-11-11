package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

func TestTopResolverBeforeHuman(t *testing.T) {
	for _, flags := range []string{"", "stop", "scope, top"} {
		t.Run(flags, func(t *testing.T) {
			store := &fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+`{"version":1,"attempt":3,"phase":"result","outcome":"failed","result":"failed"}`)}
			repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_AUTO_REVIEW": flags, "AGENT_ROLE_RESOLVER_TOP": "claude:claude-fable-5-2:xhigh"}}
			r := &Runtime{Clients: map[string]issues.Store{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}
			ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: repo.Path}, store.issueComments)
			if err := r.escalateResolution(context.Background(), ticket, ticket.Resolution, "票面口径待定", "已检查"); err != nil {
				t.Fatal(err)
			}
			after := fromEvidence(t, ticket, store.issueComments)
			if flags == "scope, top" {
				if after.Owner != machine.OwnerResolver || after.Resolution.Phase != "waiting" {
					t.Fatalf("expected final review before human: %+v", after)
				}
				if !strings.Contains(store.comments[0], `"top":true`) {
					t.Fatalf("missing durable top marker: %s", store.comments[0])
				}
			} else if after.Owner != machine.OwnerHuman || after.Resolution.Phase != "human" {
				t.Fatalf("default changed: %+v", after)
			}
		})
	}
}

func TestTopResolverPreservesBlockClass(t *testing.T) {
	decision, err := os.ReadFile("../../testdata/recorded/resolver-escalate-credentials.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(resolver.Record{Version: 1, Attempt: 3, Phase: "result", Outcome: "failed", Result: string(decision)})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+string(raw))}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": store}, Repositories: map[string]config.Repository{"/repo": {Env: map[string]string{"AGENT_AUTO_REVIEW": "top"}}}}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, store.issueComments)
	if err := r.applyResolution(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(store.comments) != 1 || !strings.HasPrefix(store.comments[0], "Agent处置: 终审处置\n") || !strings.Contains(store.comments[0], `"top_class":"credentials"`) {
		t.Fatalf("missing structured block: %q", store.comments)
	}
}

func TestTopResolverTierAndPrompt(t *testing.T) {
	repo := t.TempDir()
	roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
	if err := os.MkdirAll(roles, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+`{"version":1,"attempt":3,"phase":"waiting","top":true,"top_used":true}`)}
	r := &Runtime{Clients: map[string]issues.Store{repo: store}, Repositories: map[string]config.Repository{repo: {Env: map[string]string{"AGENT_ROLE_RESOLVER": "codex:ordinary:low", "AGENT_ROLE_RESOLVER_TOP": "claude:claude-fable-5-2:xhigh"}}}, Orca: &resolverStartupProbe{}}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: repo}, store.issueComments)
	if err := r.ResolveTicket(context.Background(), ticket, nil, "controller", nil); err == nil {
		t.Fatal("expected startup probe stop")
	}
	after := fromEvidence(t, ticket, store.issueComments).Resolution
	if after.Agent != "claude" || after.Model != "claude-fable-5-2" || after.Effort != "xhigh" || !strings.Contains(after.Spec, "你是该票的最终决策者") {
		t.Fatalf("wrong final review: %+v", after)
	}
}

func TestTopResolverAcceptedMergeEndsRound(t *testing.T) {
	h := validHandoffForResolver()
	decision, err := json.Marshal(resolver.Resolution{Action: "merge", Reason: "reviewed", Handoff: &h})
	if err != nil {
		t.Fatal(err)
	}
	testTopResolverNextRound(t, "applied", "succeeded", string(decision), false)
}

func TestTopResolverAcceptedOpenPREndsRound(t *testing.T) {
	testTopResolverNextRound(t, "waiting", "succeeded", `{"action":"open-pr","branch":"agent/OF-1/executor","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","title":"Ready","body":"Checked","reason":"verified"}`, false)
}

func TestTopResolverFailedRoundKeepsTier(t *testing.T) {
	testTopResolverNextRound(t, "waiting", "failed", "temporary failure", true)
}

func testTopResolverNextRound(t *testing.T, phase, outcome, result string, wantTop bool) {
	t.Helper()
	repo := t.TempDir()
	roles := filepath.Join(repo, "scripts", "agent-gates", "roles")
	if err := os.MkdirAll(roles, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roles, "resolver.md"), []byte("Resolve"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(resolver.Record{Version: 1, Attempt: 4, Phase: phase, Outcome: outcome, Result: result, Top: true, TopUsed: true, Agent: "claude", Model: "claude-fable-5-2", Effort: "xhigh"})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeIssues{issueComments: evidenceComments(resolver.Prefix+string(raw), "Agent处置: 合并通道要求继续复核\n阶段: Agent处置\n归属: 处置Agent")}
	r := &Runtime{Clients: map[string]issues.Store{repo: store}, Repositories: map[string]config.Repository{repo: {Env: map[string]string{"AGENT_AUTO_REVIEW": "top", "AGENT_ROLE_RESOLVER": "codex:ordinary:low", "AGENT_ROLE_RESOLVER_TOP": "claude:claude-fable-5-2:xhigh"}}}, Orca: &resolverStartupProbe{}}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: repo}, store.issueComments)
	if p := ticket.Resolution; p.Phase != "waiting" || p.Top != wantTop || !p.TopUsed {
		t.Errorf("wrong recovered round: %+v", p)
	}
	if wantTop && ticket.Resolution.Failures != 1 {
		t.Errorf("failures=%d, want 1", ticket.Resolution.Failures)
	}
	if err := r.ResolveTicket(context.Background(), ticket, nil, "controller", nil); err == nil {
		t.Fatal("expected startup probe stop")
	}
	after := fromEvidence(t, ticket, store.issueComments).Resolution
	agent, model, effort := "codex", "ordinary", "low"
	if wantTop {
		agent, model, effort = "claude", "claude-fable-5-2", "xhigh"
	}
	if after.Agent != agent || after.Model != model || after.Effort != effort || after.Top != wantTop || !after.TopUsed || strings.Contains(after.Spec, "你是该票的最终决策者") != wantTop {
		t.Errorf("wrong next dispatch: %+v", after)
	}
	if !wantTop {
		if err := r.escalateResolution(context.Background(), ticket, after, "需人复核", "已检查"); err != nil {
			t.Fatal(err)
		}
		if last := fromEvidence(t, ticket, store.issueComments).Resolution; last.Phase != "human" {
			t.Errorf("repeated top round: %+v", last)
		}
		for _, comment := range store.comments {
			if strings.Contains(comment, "Agent处置: 终审处置") {
				t.Fatalf("repeated final review: %s", comment)
			}
		}
	}
}

func TestTopResolverRuling(t *testing.T) {
	for _, tc := range []struct {
		name, reason, class, evidence string
		allowed                       bool
	}{
		{name: "普通裁定", reason: "按已有 CHECK 实施", allowed: true},
		{name: "无害文字", reason: "无需授权，不触碰生产数据", allowed: true},
		{name: "凭证缺失", class: "credentials"},
		{name: "环境阻塞", class: "environment"},
		{name: "需要授权", class: "authorization"},
		{name: "不可逆操作", class: "irreversible"},
		{name: "生产 DDL", class: "production-ddl"},
		{name: "生产数据", class: "production-data"},
		{name: "证据冲突", evidence: merge.HandoffPrefix + "invalid"},
		{name: "lane 3", evidence: merge.HandoffPrefix + `{"version":1,"lane":3}`},
		{name: "部署 paused", evidence: merge.ProgressPrefix + `{"version":1,"deployment":{"state":"paused"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := tc.reason
			if reason == "" {
				reason = "可以继续"
			}
			decision, _ := json.Marshal(resolver.Resolution{Action: "ruling", Reason: reason})
			raw, _ := json.Marshal(map[string]any{"version": 1, "attempt": 4, "phase": "result", "top": true, "top_used": true, "top_class": tc.class, "outcome": "succeeded", "result": string(decision)})
			store := &topStore{fakeIssues: fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+string(raw), tc.evidence)}}
			r := &Runtime{Clients: map[string]issues.Store{"/repo": store}}
			ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, store.issueComments)
			if err := r.applyResolution(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			after := fromEvidence(t, ticket, store.issueComments)
			last := store.comments[len(store.comments)-1]
			if tc.allowed {
				if !strings.HasPrefix(last, "裁定：top-resolver ") || after.Resolution.Phase != "waiting" || !after.Resolution.TopUsed || after.Resolution.Top {
					t.Fatalf("ruling not shared or resumed normally: %s %+v", last, after.Resolution)
				}
			} else if after.Resolution.Phase != "human" || strings.HasPrefix(last, "裁定：") {
				t.Fatalf("forbidden ruling accepted: %s", last)
			}
		})
	}
}

func TestTopResolverNeverRepeatsAfterRuling(t *testing.T) {
	store := &fakeIssues{issueComments: evidenceComments(resolver.Prefix+`{"version":1,"attempt":3,"phase":"result","top":true}`, "裁定：top-resolver now\n"+resolver.Prefix+`{"version":1,"attempt":3,"phase":"waiting"}`)}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": store}, Repositories: map[string]config.Repository{"/repo": {Env: map[string]string{"AGENT_AUTO_REVIEW": "top"}}}}
	ticket := fromEvidence(t, Ticket{ID: "id", Repo: "/repo"}, store.issueComments)
	if err := r.escalateResolution(context.Background(), ticket, ticket.Resolution, "再失败", "proof"); err != nil {
		t.Fatal(err)
	}
	if after := fromEvidence(t, ticket, store.issueComments); after.Resolution.Phase != "human" {
		t.Fatalf("second top: %+v", after.Resolution)
	}
}

type topStore struct{ fakeIssues }

func (s *topStore) Read(ctx context.Context, id string) (issues.Task, error) {
	ticket, err := s.fakeIssues.Read(ctx, id)
	ticket.Description = "## LANE 预判\n2\n\n## SCOPE\na.go\n"
	ticket.Metadata.ID = id
	return ticket, err
}

func TestTopResolverFinalEscalationSendsCard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := &notifyStore{fakeIssues: fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+`{"version":1,"attempt":4,"phase":"result","top":true,"top_used":true,"outcome":"failed","result":"{\"action\":\"escalate\",\"reason\":\"票面口径待定\",\"body\":\"已核对\"}"}`)}}
	for i := range store.issueComments {
		store.issueComments[i].CreatedAt = time.Time{}
	}
	sender := &notifySender{store: store}
	sender.start(t)
	repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat", "AGENT_AUTO_REVIEW": "top,release,scope"}}
	r := &Runtime{Clients: map[string]issues.Store{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: repo.Path}, store.issueComments)
	if err := r.NotifyFeishu(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 0 {
		t.Fatal("card before final escalation")
	}
	if err := r.applyResolution(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	ticket = fromEvidence(t, ticket, store.issueComments)
	store.readErr = errors.New("final decision must not enter automatic review again")
	if err := r.NotifyFeishu(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 || ticket.Resolution.Phase != "human" {
		t.Fatalf("final escalation swallowed: messages=%q state=%+v", sender.messages, ticket.Resolution)
	}
}

func TestTopResolverSecondFailure(t *testing.T) {
	store := &fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+`{"version":1,"attempt":4,"phase":"waiting","top":true,"outcome":"failed"}`, resolver.Prefix+`{"version":1,"attempt":5,"phase":"result","top":true,"outcome":"failed","result":"暂时失败"}`)}
	r := &Runtime{Clients: map[string]issues.Store{"/repo": store}}
	ticket := fromEvidence(t, Ticket{ID: "id", Repo: "/repo"}, store.issueComments)
	if err := r.applyResolution(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if after := fromEvidence(t, ticket, store.issueComments); after.Resolution.Phase != "human" {
		t.Fatalf("top retries beyond second failure: %+v", after.Resolution)
	}
}
