package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func TestScopeBlockedReportGoesToResolverWithoutCorrection(t *testing.T) {
	e, o, _, s, l, ticket := recoveryFixture(t)
	o.failFirst = true
	o.failBody = "第 4 项做不了\nSCOPE-BLOCKED: functions/api/v1/_mcp-account-routes.js functions/mcp/oauth.test.js\n"
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver || result.State.Phase != machine.PhaseResolver {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if s.sessions != 0 || s.children != 0 || len(o.starts) != 1 || result.State.Corrections != 0 {
		t.Fatalf("correction ran: sessions=%d children=%d starts=%d state=%+v", s.sessions, s.children, len(o.starts), result.State)
	}
	all := strings.Join(l.comments, "\n")
	if !strings.Contains(all, "Agent处置: "+result.State.HumanReason) || !strings.Contains(all, "functions/api/v1/_mcp-account-routes.js functions/mcp/oauth.test.js") {
		t.Fatalf("handoff evidence: %q", all)
	}
}

func TestUnverifiedScopeBlockedKeepsCorrection(t *testing.T) {
	e, o, _, s, _, ticket := recoveryFixture(t)
	o.failFirst = true
	o.failBody = "SCOPE-BLOCKED: unit/change.go\n"
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || s.sessions != 1 {
		t.Fatalf("result=%+v err=%v sessions=%d", result, err, s.sessions)
	}
}

func TestScopeBlockedFiles(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "gate.spec")
	if err := os.WriteFile(spec, []byte("UNIT: executor\nSCOPE: driver/internal/* a.go\nCHECK: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"one file", "SCOPE-BLOCKED: b.go", []string{"b.go"}},
		{"line among prose", "原因\n  SCOPE-BLOCKED: b.go web/c.ts  \n详情", []string{"b.go", "web/c.ts"}},
		{"several lines", "SCOPE-BLOCKED: b.go\nSCOPE-BLOCKED: c.go", []string{"b.go", "c.go"}},
		{"no signal", "SCOPE 缺 b.go，请把它加入 SCOPE", nil},
		{"mid-line mention", "可以写 SCOPE-BLOCKED: b.go", nil},
		{"no paths", "SCOPE-BLOCKED:", nil},
		{"already in SCOPE", "SCOPE-BLOCKED: a.go", nil},
		{"already in SCOPE by glob", "SCOPE-BLOCKED: driver/internal/x/y.go", nil},
		{"one of several in SCOPE", "SCOPE-BLOCKED: b.go a.go", nil},
		{"line number", "SCOPE-BLOCKED: b.go:53", nil},
		{"glob", "SCOPE-BLOCKED: web/*", nil},
		{"absolute", "SCOPE-BLOCKED: /etc/passwd", nil},
		{"parent", "SCOPE-BLOCKED: ../b.go", nil},
		{"unclean", "SCOPE-BLOCKED: web/../b.go", nil},
		{"ledger field injection", "SCOPE-BLOCKED: b.go\x00归属:", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopeBlockedFiles(tc.body, spec); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	if got := scopeBlockedFiles("SCOPE-BLOCKED: b.go", filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("unreadable spec verified %q", got)
	}
}

func TestWorkerScopeBlockedAutoRule(t *testing.T) {
	const docs = "docs/conversation-follow-up-center.md docs/system-manual.md"
	for _, tc := range []struct {
		name, mode, files       string
		prior, frozen, approved bool
	}{
		{name: "auto docs", mode: "auto", files: docs, approved: true},
		{name: "control files", mode: "auto", files: "core/gate.sh scripts/agent-gates/gate.sh"},
		{name: "eleven paths", mode: "auto", files: "a.go b.go c.go d.go e.go f.go g.go h.go i.go j.go k.go"},
		{name: "two approvals", mode: "auto", files: docs, prior: true},
		{name: "frozen merge spec", mode: "auto", files: docs, frozen: true},
		{name: "manual docs", mode: "manual", files: docs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_WORKER_AUTO_SCOPE", "")
			e, o, tools, recovery, ledger, ticket := recoveryFixture(t)
			e.Launch, e.LaunchReceipts = "launch_one", true
			ledger.comments = []string{launchReceipt(tc.mode)}
			ticket.Scope = "unit/change.go"
			face := "## LANE 预判\n2\n\n## SCOPE\nunit/change.go\n\n## CHECK\ngo test ./unit\n"
			store := &scopeIssues{budgetIssues: ledger, description: face}
			e.Issues = store
			if tc.prior {
				ledger.comments = append(ledger.comments, "扩SCOPE：auto 2026-11-21T00:00:00Z", "扩SCOPE：worker-auto 2026-11-21T01:00:00Z\n路径：docs/previous.md")
			}
			initialComments := len(ledger.comments)
			o.failFirst = true
			o.failBody = "SCOPE-BLOCKED: " + tc.files
			var restored *checkpoint
			if tc.frozen {
				o.starts = []orca.StartOptions{{Task: "task_one"}}
				restored = &checkpoint{Run: "run_one", Task: "task_one", Dispatch: "ctx_one", Worktree: "/original/worktree", MergeSpec: "UNIT: executor\nSCOPE: unit/change.go\nCHECK: true\n"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := e.execute(ctx, ticket, restored, 0)
			if err != nil {
				t.Fatal(err)
			}
			comments := strings.Join(ledger.comments[initialComments:], "\n")
			if !tc.approved {
				if store.description != face || result.State.Owner != machine.OwnerResolver || result.State.Phase != machine.PhaseResolver || result.State.Corrections != 0 || recovery.sessions != 0 || !strings.Contains(comments, "Agent处置: ") || strings.Contains(comments, "扩SCOPE：worker-auto ") {
					t.Fatalf("refusal changed ticket or skipped resolver: face=%q state=%+v comments=%q", store.description, result.State, comments)
				}
				return
			}
			wantScope := "unit/change.go " + docs
			if !strings.Contains(store.description, "## SCOPE\n"+wantScope+"\n") || result.State.Corrections != 1 || recovery.sessions != 1 || recovery.children != 1 || len(o.starts) != 2 || result.State.Infra != 0 {
				t.Fatalf("auto request did not extend scope and enter correction: face=%q state=%+v sessions=%d", store.description, result.State, recovery.sessions)
			}
			if strings.Count(comments, "扩SCOPE：worker-auto ") != 1 || strings.Contains(comments, "Agent处置: ") || !strings.Contains(comments, "路径："+docs) {
				t.Fatalf("audit or ownership: %q", comments)
			}
			spec, err := os.ReadFile(filepath.Join(e.Config.Root, e.Config.Repository, ticket.Identifier, "gate.spec"))
			if err != nil || !strings.Contains(string(spec), "SCOPE: "+wantScope+"\n") || !strings.Contains(recovery.requests[0].Brief, "SCOPE: "+wantScope+"\n") || tools.gateCalls != 1 || tools.reviewCalls != 1 {
				t.Fatalf("scope propagation or gates: spec=%q err=%v", spec, err)
			}
		})
	}
}

func launchReceipt(mode string) string {
	return "派工档位: launch=launch_one attempt=1 mode=" + mode
}

func TestWorkerScopeBlockedFollowsVerifiedMode(t *testing.T) {
	const docs, sibling = "docs/system-manual.md", "unit/change_test.go"
	const claim = "阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1 since=2026-10-05T05:59:00Z"
	approvals := []string{"扩SCOPE：auto 2026-11-21T00:00:00Z", "扩SCOPE：worker-auto 2026-11-21T01:00:00Z"}
	launched := func(mode string, more ...string) []string {
		return append([]string{claim, launchReceipt(mode)}, more...)
	}
	const registered = "阶段: worker 运行中\n归属: 执行器 term=term_exec run=run_one attempt=1 since=2026-10-05T05:59:30Z launch=launch_one"
	otherLaunch := "派工档位: launch=launch_two attempt=1 mode=auto"
	verified := func(mode string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return mode, nil }
	}
	unverified := func(context.Context) (string, error) { return "", errors.New("repository is not taken over") }
	const autoRule, whitelist, resolver = "扩SCOPE：worker-auto ", "自动扩 SCOPE：", ""
	for _, tc := range []struct {
		name     string
		comments []string
		verify   func(context.Context) (string, error)
		operator bool
		files    string
		want     string
	}{
		{"dispatched auto outranks checkout", launched("auto"), verified("manual"), false, docs, autoRule},
		{"dispatched auto after two approvals", launched("auto", approvals...), verified("manual"), false, sibling, resolver},
		{"dispatched manual outranks checkout", launched("manual"), verified("auto"), false, sibling, whitelist},
		{"dispatched manual rejects auto paths", launched("manual"), verified("auto"), false, docs, resolver},
		{"relaunch replaces the failed launch", []string{claim, otherLaunch, launchReceipt("manual")}, verified("auto"), false, docs, resolver},
		{"dispatched without receipt", []string{claim}, verified("auto"), false, docs, resolver},
		{"dispatched with another launch's receipt", []string{claim, otherLaunch}, verified("auto"), false, sibling, resolver},
		{"launch name used twice", launched("auto", registered), verified("auto"), false, docs, resolver},
		{"operator start in auto repository", nil, verified("auto"), true, docs, autoRule},
		{"operator start in auto repository after two approvals", approvals, verified("auto"), true, sibling, resolver},
		{"operator start in manual repository keeps whitelist", nil, verified("manual"), true, sibling, whitelist},
		{"operator start in manual repository rejects auto paths", nil, verified("manual"), true, docs, resolver},
		{"operator start on an undelivered claim", []string{claim}, verified("manual"), true, docs, resolver},
		{"operator start beside another launch", []string{claim, otherLaunch}, verified("manual"), true, docs, resolver},
		{"operator start unverified", []string{claim}, unverified, true, sibling, resolver},
		{"operator start without verifier", nil, nil, true, sibling, resolver},
		{"operator restart after a launch", launched("auto", registered), verified("manual"), true, docs, resolver},
		{"operator restart after a launch unverified", launched("manual", registered), unverified, true, sibling, resolver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_WORKER_AUTO_SCOPE", "1")
			e, o, _, _, ledger, ticket := recoveryFixture(t)
			e.VerifyMode = tc.verify
			if !tc.operator {
				e.Launch, e.LaunchReceipts = "launch_one", true
			}
			ticket.Scope = "unit/change.go"
			const face = "## LANE 预判\n2\n\n## SCOPE\nunit/change.go\n\n## CHECK\ngo test ./unit\n"
			store := &scopeIssues{budgetIssues: ledger, description: face}
			e.Issues = store
			ledger.comments = slices.Clone(tc.comments)
			o.failFirst = true
			o.failBody = "SCOPE-BLOCKED: " + tc.files
			result, err := e.Execute(t.Context(), ticket)
			if err != nil {
				t.Fatal(err)
			}
			written := strings.Join(ledger.comments[len(tc.comments):], "\n")
			if tc.want == resolver {
				if store.description != face || result.State.Phase != machine.PhaseResolver || result.State.Corrections != 0 || strings.Contains(written, autoRule) || strings.Contains(written, whitelist) {
					t.Fatalf("face=%q state=%+v comments=%q", store.description, result.State, written)
				}
				return
			}
			other := autoRule
			if tc.want == autoRule {
				other = whitelist
			}
			if !strings.Contains(store.description, "unit/change.go "+tc.files) || result.State.Corrections != 1 || strings.Count(written, tc.want) != 1 || strings.Contains(written, other) {
				t.Fatalf("face=%q state=%+v comments=%q", store.description, result.State, written)
			}
		})
	}
}

func TestScopeModeNeedsAnAuthenticatedLedger(t *testing.T) {
	e, _, _, _, ledger, ticket := recoveryFixture(t)
	e.Issues = &scopeIssues{budgetIssues: ledger}
	ledger.comments = []string{launchReceipt("auto"), "归属: 执行器 term=term_exec run=run_one attempt=1 launch=launch_one"}
	e.VerifyMode = func(context.Context) (string, error) { return "auto", nil }
	for _, authenticated := range []bool{true, false} {
		e.Launch, e.LaunchReceipts = "launch_one", authenticated
		mode, err := e.scopeMode(t.Context(), ticket, "term_exec")
		if err != nil || (mode == "auto") != authenticated {
			t.Fatalf("authenticated=%v mode=%q err=%v", authenticated, mode, err)
		}
	}
}

func TestLaunchedModeTrustsOnlyDriverReceipts(t *testing.T) {
	var envelope struct {
		Data struct {
			Issue struct {
				Comments struct{ Nodes []task.Comment }
			}
		}
	}
	if err := json.Unmarshal(recorded(t, "linear-comments-executor-handoff.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	const terminal = "term_fdf45555-3378-4ca8-99b1-6200fc3106fc"
	const recordedRegistration = "归属: 执行器 term=" + terminal + " run=run_c2d32709f17c attempt=1 since=2026-10-05T14:08:40Z"
	const registration = recordedRegistration + " launch=launch_one"
	event := func(source, body string) task.Comment { return task.Comment{Source: source, Body: body} }
	history := slices.Clone(envelope.Data.Issue.Comments.Nodes)
	for i := range history {
		history[i].Body = strings.Replace(history[i].Body, recordedRegistration, registration, 1)
	}
	receipt := launchReceipt("auto")
	for _, tc := range []struct {
		name     string
		history  []task.Comment
		events   []task.Comment
		terminal string
		want     string
	}{
		{"driver receipt", history, []task.Comment{event(task.SourceDriver, receipt)}, terminal, "auto"},
		{"recorded registration without launch", envelope.Data.Issue.Comments.Nodes, []task.Comment{event(task.SourceDriver, receipt)}, terminal, ""},
		{"no receipt", history, nil, terminal, ""},
		{"operator event", history, []task.Comment{event(task.SourceOperator, receipt)}, terminal, ""},
		{"event without source", history, []task.Comment{event("", receipt)}, terminal, ""},
		{"quoted in another event", history, []task.Comment{event(task.SourceDriver, "阶段: gate\n"+receipt)}, terminal, ""},
		{"another launch", history, []task.Comment{event(task.SourceDriver, "派工档位: launch=launch_two attempt=1 mode=auto")}, terminal, ""},
		{"unknown mode", history, []task.Comment{event(task.SourceDriver, launchReceipt("turbo"))}, terminal, ""},
		{"another terminal", history, []task.Comment{event(task.SourceDriver, receipt)}, "term_other", ""},
		{"registered twice", history, []task.Comment{event(task.SourceDriver, receipt), event(task.SourceDriver, registration)}, terminal, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := launchedMode(slices.Concat(tc.history, tc.events), "launch_one", tc.terminal); got != tc.want {
				t.Fatalf("mode=%q, want %q", got, tc.want)
			}
		})
	}
}
