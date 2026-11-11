package scheduler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type labelStore struct {
	fakeIssues
	state       string
	add, remove []string
}

func (s *labelStore) UpdateIssue(_ context.Context, _ string, state string, add, remove []string) (task.Issue, error) {
	s.updates++
	s.state, s.add, s.remove = state, add, remove
	return task.Issue{ID: "id"}, nil
}

func modeRuntime(store issues.Store, env map[string]string) *Runtime {
	repo := config.Repository{Path: "/repo", Env: env}
	return &Runtime{Clients: map[string]issues.Store{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}
}

func assertParked(t *testing.T, store *labelStore, ticket Ticket) {
	t.Helper()
	after := fromEvidence(t, ticket, store.issueComments)
	if after.Owner != machine.OwnerNone || after.Resolution.Phase != "applied" {
		t.Fatalf("parked ticket keeps ownership: owner=%s resolution=%+v", after.Owner, after.Resolution)
	}
	last := store.comments[len(store.comments)-1]
	if !strings.HasPrefix(last, "搁置：") || strings.Contains(last, "转人: ") {
		t.Fatalf("park comment=%q", last)
	}
	if !slices.Contains(store.add, "parked") || !slices.Contains(store.remove, "ready-for-agent") || store.state != "Todo" {
		t.Fatalf("park labels state=%q add=%q remove=%q", store.state, store.add, store.remove)
	}
}

func TestAgentModeEscalationAfterTop(t *testing.T) {
	for _, mode := range []string{"", "manual", "auto"} {
		t.Run(mode, func(t *testing.T) {
			store := &labelStore{fakeIssues: fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+`{"version":1,"attempt":4,"phase":"result","top":true,"top_used":true,"outcome":"failed","result":"{\"action\":\"escalate\",\"reason\":\"票面口径待定\",\"body\":\"已核对\",\"class\":\"other\"}"}`)}}
			env := map[string]string{"AGENT_AUTO_REVIEW": "top"}
			if mode != "" {
				env["AGENT_MODE"] = mode
			}
			r := modeRuntime(store, env)
			ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, store.issueComments)
			if err := r.applyResolution(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			if mode == "auto" {
				assertParked(t, store, ticket)
				return
			}
			if after := fromEvidence(t, ticket, store.issueComments); after.Owner != machine.OwnerHuman || after.Resolution.Phase != "human" {
				t.Fatalf("manual changed: %+v", after)
			}
		})
	}
}

func TestAutoModeImpliesTopReview(t *testing.T) {
	store := &labelStore{fakeIssues: fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+`{"version":1,"attempt":3,"phase":"result","outcome":"failed","result":"failed"}`)}}
	r := modeRuntime(store, map[string]string{"AGENT_MODE": "auto"})
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, store.issueComments)
	if err := r.escalateResolution(context.Background(), ticket, ticket.Resolution, "票面口径待定", "已检查"); err != nil {
		t.Fatal(err)
	}
	if after := fromEvidence(t, ticket, store.issueComments); after.Owner != machine.OwnerResolver || !after.Resolution.Top {
		t.Fatalf("auto mode skipped final review: %+v", after.Resolution)
	}
	if !autoReviewEnabled(r.Repositories["/repo"].Env, "scope") || !autoReviewEnabled(r.Repositories["/repo"].Env, "release") {
		t.Fatal("auto mode must imply scope and release")
	}
}

func TestAutoModeParksMustHumanClassBeforeTop(t *testing.T) {
	store := &labelStore{fakeIssues: fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+`{"version":1,"attempt":3,"phase":"result","outcome":"failed","result":"{\"action\":\"escalate\",\"reason\":\"缺少部署凭证\",\"body\":\"已核对\",\"class\":\"credentials\"}"}`)}}
	r := modeRuntime(store, map[string]string{"AGENT_MODE": "auto"})
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, store.issueComments)
	if err := r.applyResolution(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	assertParked(t, store, ticket)
	if strings.Contains(store.comments[0], "终审处置") {
		t.Fatalf("must-human class entered final review: %q", store.comments)
	}
}

func TestAutoModeTopRetriesUpToResolverLimit(t *testing.T) {
	failed := func(attempt int) string {
		return resolver.Prefix + fmt.Sprintf(`{"version":1,"attempt":%d,"phase":"waiting","top":true,"top_used":true,"outcome":"failed"}`, attempt)
	}
	result := resolver.Prefix + `{"version":1,"attempt":6,"phase":"result","top":true,"top_used":true,"outcome":"failed","result":"暂时失败"}`
	for _, tc := range []struct {
		name     string
		comments []string
		parked   bool
	}{
		{"second failure retries", []string{failed(5), result}, false},
		{"third failure parks", []string{failed(4), failed(5), result}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &labelStore{fakeIssues: fakeIssues{issueComments: evidenceComments(append([]string{"阶段: Agent处置\n归属: 处置Agent"}, tc.comments...)...)}}
			r := modeRuntime(store, map[string]string{"AGENT_MODE": "auto"})
			ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, store.issueComments)
			if err := r.applyResolution(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			if tc.parked {
				assertParked(t, store, ticket)
				return
			}
			if after := fromEvidence(t, ticket, store.issueComments); after.Owner != machine.OwnerResolver || after.Resolution.Phase != "waiting" {
				t.Fatalf("auto top stopped before resolver limit: %+v", after.Resolution)
			}
		})
	}
}

func TestParkedTicketIsNotClaimed(t *testing.T) {
	issue := task.Issue{State: task.NamedID{Name: "Todo"}, Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "ready-for-agent"}, {Name: "parked"}}}}
	if candidate, _, _ := issueEligibility(issue); candidate {
		t.Fatal("parked ticket is a candidate")
	}
}

func TestParkedTicketStatusAndNotification(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ticket := Ticket{ID: "id", Repo: "/repo", Identifier: "OF-9", Owner: machine.OwnerNone, Parked: "缺少部署凭证"}
	if row := ObservationRow(ticket, nil, time.Now()); row.BlockedReason != "parked: 缺少部署凭证" {
		t.Fatalf("blockedReason=%q", row.BlockedReason)
	}
	if rows := SnapshotRows([]Ticket{ticket}, nil, time.Now()); len(rows) != 1 {
		t.Fatalf("parked ticket missing from status: %+v", rows)
	}
	store := &notifyStore{}
	sender := &notifySender{store: store}
	sender.start(t)
	r := modeRuntime(store, map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat", "AGENT_MODE": "auto"})
	for range 2 {
		ticket.NotifyEvents = map[string]bool{}
		for _, c := range store.issueComments {
			ticket.NotifyEvents[c.Body] = true
		}
		if err := r.NotifyFeishu(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
	}
	if len(sender.messages) != 1 || !strings.HasPrefix(sender.messages[0], "oc_chat\nOF-9 ") || strings.Contains(sender.messages[0], "放行") {
		t.Fatalf("park notification=%q", sender.messages)
	}
}

func TestAutoModeParksLane3(t *testing.T) {
	store := &labelStore{}
	r := modeRuntime(store, map[string]string{"AGENT_MODE": "auto"})
	ticket := Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo", Handoff: &merge.Handoff{QueueID: "q1", PR: 7, Head: strings.Repeat("a", 40)}}
	if err := r.mergeDecision(context.Background(), ticket, strings.Repeat("b", 40), merge.Result{Status: merge.HumanDirect, Reason: "lane 3 path .env"}); err != nil {
		t.Fatal(err)
	}
	after := fromEvidence(t, ticket, store.issueComments)
	last := store.comments[len(store.comments)-1]
	if after.Owner != machine.OwnerNone || !strings.HasPrefix(last, "搁置：lane 3 path .env\n") || !strings.Contains(last, merge.ProgressPrefix) || !slices.Contains(store.add, "parked") {
		t.Fatalf("lane 3 not parked: owner=%s comment=%q add=%q", after.Owner, last, store.add)
	}
}
