package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/status"
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
				if after := fromEvidence(t, ticket, store.issueComments); after.Owner != machine.OwnerResolver || after.Resolution.Phase != "waiting" || store.updates != 0 {
					t.Fatalf("auto top escalation skipped its retries: %+v", after.Resolution)
				}
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
	decision, err := os.ReadFile("../../testdata/recorded/resolver-escalate-credentials.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(resolver.Record{Version: 1, Attempt: 3, Phase: "result", Outcome: "failed", Result: string(decision)})
	if err != nil {
		t.Fatal(err)
	}
	store := &labelStore{fakeIssues: fakeIssues{issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+string(raw))}}
	r := modeRuntime(store, map[string]string{"AGENT_MODE": "auto"})
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, store.issueComments)
	if err := r.applyResolution(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	assertParked(t, store, ticket)
	if strings.Contains(store.comments[0], "终审处置") {
		t.Fatalf("must-human class entered final review: %q", store.comments)
	}
	labels := func(names ...string) task.Issue {
		issue := task.Issue{}
		for _, name := range names {
			issue.Labels.Nodes = append(issue.Labels.Nodes, task.NamedID{ID: name, Name: name})
		}
		return issue
	}
	if reason, resumed := parkedState(labels("parked"), store.issueComments); !strings.HasPrefix(reason, "部署服务账号凭证未配置") || resumed {
		t.Fatalf("parked read reason=%q resumed=%v", reason, resumed)
	}
	if reason, resumed := parkedState(labels("ready-for-agent"), store.issueComments); reason != "" || !resumed {
		t.Fatalf("restored read reason=%q resumed=%v", reason, resumed)
	}
}

func TestAutoModeParksMustHumanEvidenceBeforeTop(t *testing.T) {
	for _, tc := range []struct {
		name        string
		description string
		edit        func(*Ticket)
	}{
		{"evidence conflict", "", func(t *Ticket) { t.MergeProblem = "conflicting handoff" }},
		{"deployment paused", "", func(t *Ticket) {
			t.Progress = &merge.Progress{Version: 1, Phase: "merged", Deployment: &merge.DeploymentProgress{State: "paused"}}
		}},
		{"lane 3", "", func(t *Ticket) { t.Handoff = &merge.Handoff{Version: 1, Lane: 3} }},
		{"ticket face lane 3", "## LANE 预判\n3（网关按 AGENT_LANE2_GLOBS / AGENT_LANE3_GLOBS 计算）\n", func(*Ticket) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &labelStore{fakeIssues: fakeIssues{description: tc.description, issueComments: evidenceComments("阶段: Agent处置\n归属: 处置Agent", resolver.Prefix+`{"version":1,"attempt":3,"phase":"result","outcome":"failed","result":"{\"action\":\"escalate\",\"reason\":\"无法继续\",\"body\":\"已核对\",\"class\":\"other\"}"}`)}}
			r := modeRuntime(store, map[string]string{"AGENT_MODE": "auto"})
			ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, store.issueComments)
			tc.edit(&ticket)
			if err := r.escalateResolution(context.Background(), ticket, ticket.Resolution, "无法继续", "已核对"); err != nil {
				t.Fatal(err)
			}
			last := store.comments[len(store.comments)-1]
			if !strings.HasPrefix(last, "搁置：") || strings.Contains(last, "终审处置") || !slices.Contains(store.add, "parked") {
				t.Fatalf("must-human evidence entered final review: %q", store.comments)
			}
		})
	}
}

func TestAutoModeTopRetriesUpToResolverLimit(t *testing.T) {
	failed := func(attempt int) string {
		return resolver.Prefix + fmt.Sprintf(`{"version":1,"attempt":%d,"phase":"waiting","top":true,"top_used":true,"outcome":"failed"}`, attempt)
	}
	for _, outcome := range []string{`"暂时失败"`, `"{\"action\":\"escalate\",\"reason\":\"票面口径待定\",\"body\":\"已核对\",\"class\":\"other\"}"`} {
		result := resolver.Prefix + `{"version":1,"attempt":6,"phase":"result","top":true,"top_used":true,"outcome":"failed","result":` + outcome + `}`
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
}

func TestParkedTicketIsNotClaimed(t *testing.T) {
	issue := task.Issue{State: task.NamedID{Name: "Todo"}, Labels: task.Connection[task.NamedID]{Nodes: []task.NamedID{{Name: "ready-for-agent"}, {Name: "parked"}}}}
	if candidate, _, _ := issueEligibility(issue); candidate {
		t.Fatal("parked ticket is a candidate")
	}
}

func TestParkedTicketStatusAndNotification(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	ticket := Ticket{ID: "id", Repo: "a", Identifier: "OF-9", Owner: machine.OwnerNone, Parked: "缺少部署凭证", MergeProblem: "conflicting handoff"}
	if rows := SnapshotRows([]Ticket{ticket}, nil, time.Now()); len(rows) != 1 {
		t.Fatalf("parked ticket missing from status: %+v", rows)
	}
	s := setup(&fakeBackend{tickets: []Ticket{ticket}})
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if view := status.Read(s.Now(), 0); len(view.Tickets) != 1 || view.Tickets[0].BlockedReason != "parked: 缺少部署凭证" {
		t.Fatalf("live status lost parked ticket: %+v", view.Tickets)
	}
	store := &notifyStore{}
	sender := &notifySender{store: store}
	sender.start(t)
	r := modeRuntime(store, map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat", "AGENT_MODE": "auto"})
	ticket.Repo = "/repo"
	for _, edit := range []func(){
		func() { ticket.Parked, ticket.MergeProblem = "缺少部署凭证", "" },
		func() { ticket.Parked, ticket.MergeProblem = "部署暂停", "conflicting handoff" },
	} {
		edit()
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

func TestResumedParkedTicketWithMergeEvidenceReturnsToResolver(t *testing.T) {
	for name, problem := range map[string]string{"lane 3 handoff": "", "evidence conflict": "conflicting handoff"} {
		t.Run(name, func(t *testing.T) {
			f := &fakeBackend{tickets: []Ticket{{ID: "p", Identifier: "OF-9", Repo: "a", Candidate: true, Resumed: true, MergeProblem: problem, Handoff: &merge.Handoff{Version: 1, Lane: 3, QueueID: "q1"}}}}
			if err := setup(f).Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.actions) != 1 || f.actions[0].Kind != Resolve {
				t.Fatalf("resumed parked ticket actions=%+v", f.actions)
			}
		})
	}
}

func TestParkedMergeHeadPausesLane(t *testing.T) {
	store := &labelStore{}
	r := modeRuntime(store, map[string]string{"AGENT_MODE": "auto"})
	paused := &merge.Progress{Version: 1, QueueID: "q1", Phase: "merged", MergeSHA: strings.Repeat("b", 40), Deployment: &merge.DeploymentProgress{State: "paused", Reason: "smoke failed"}}
	head := Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo", Owner: machine.OwnerNone, Parked: "smoke failed", Handoff: &merge.Handoff{Version: 1, QueueID: "q1", EnqueuedAt: time.Unix(1, 0)}, Progress: paused}
	if err := r.AdvanceMerges(context.Background(), []Ticket{head}); err != nil {
		t.Fatal(err)
	}
	if len(store.comments) != 0 || store.updates != 0 {
		t.Fatalf("parked merge head resumed automatic work: comments=%q updates=%d", store.comments, store.updates)
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

func TestRecordedParkedStateReadsLedger(t *testing.T) {
	var ledger struct{ Comments []task.Comment }
	if err := json.Unmarshal(recorded(t, "ledger-comments-human-release.json"), &ledger); err != nil {
		t.Fatal(err)
	}
	var read struct{ Result struct{ Issue issues.Task } }
	if err := json.Unmarshal(recorded(t, "issues-read-OF-427.json"), &read); err != nil {
		t.Fatal(err)
	}
	store := &labelStore{}
	if err := modeRuntime(store, map[string]string{"AGENT_MODE": "auto"}).parkTicket(context.Background(), Ticket{ID: "id", Identifier: "ZY-366", Repo: "/repo"}, "缺少部署凭证", "已核对", ""); err != nil {
		t.Fatal(err)
	}
	park := task.Comment{ID: "park", Body: store.comments[0], CreatedAt: ledger.Comments[3].CreatedAt.Add(time.Minute)}
	comments := append([]task.Comment{park}, ledger.Comments[:4]...)
	restored := read.Result.Issue.Issue
	parked := restored
	parked.Labels.Nodes = append(slices.Clone(restored.Labels.Nodes), task.NamedID{ID: parkedLabel, Name: parkedLabel})
	if reason, resumed := parkedState(parked, comments); reason != "缺少部署凭证" || resumed {
		t.Fatalf("parked read reason=%q resumed=%v", reason, resumed)
	}
	if reason, resumed := parkedState(restored, comments); reason != "" || !resumed {
		t.Fatalf("restored read reason=%q resumed=%v", reason, resumed)
	}
	if _, resumed := parkedState(restored, append(comments, ledger.Comments[4])); resumed {
		t.Fatal("a later ownership event still reads as a resumed park")
	}
}
