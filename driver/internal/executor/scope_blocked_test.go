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
			ledger.comments = []string{schedulerClaim(tc.mode)}
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

func schedulerClaim(mode string) string {
	return "阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1 since=2026-10-05T05:59:00Z mode=" + mode
}

func TestWorkerScopeBlockedFollowsVerifiedMode(t *testing.T) {
	const docs, sibling = "docs/system-manual.md", "unit/change_test.go"
	approvals := []string{"扩SCOPE：auto 2026-11-21T00:00:00Z", "扩SCOPE：worker-auto 2026-11-21T01:00:00Z"}
	autoClaim, manualClaim := schedulerClaim("auto"), schedulerClaim("manual")
	registered := "阶段: worker 运行中\n归属: 执行器 term=term_exec run=run_one attempt=1 since=2026-10-05T05:59:30Z"
	verified := func(mode string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return mode, nil }
	}
	unverified := func(context.Context) (string, error) { return "", errors.New("repository is not taken over") }
	const autoRule, whitelist, resolver = "扩SCOPE：worker-auto ", "自动扩 SCOPE：", ""
	for _, tc := range []struct {
		name     string
		comments []string
		verify   func(context.Context) (string, error)
		files    string
		want     string
	}{
		{"dispatched auto outranks checkout", []string{autoClaim}, verified("manual"), docs, autoRule},
		{"dispatched auto after two approvals", append([]string{autoClaim}, approvals...), verified("manual"), sibling, resolver},
		{"dispatched manual outranks checkout", []string{manualClaim}, verified("auto"), sibling, whitelist},
		{"dispatched manual rejects auto paths", []string{manualClaim}, verified("auto"), docs, resolver},
		{"operator start in auto repository", nil, verified("auto"), docs, autoRule},
		{"operator start in auto repository after two approvals", approvals, verified("auto"), sibling, resolver},
		{"operator start in manual repository keeps whitelist", nil, verified("manual"), sibling, whitelist},
		{"operator start in manual repository rejects auto paths", nil, verified("manual"), docs, resolver},
		{"operator start unverified", nil, unverified, sibling, resolver},
		{"operator start without verifier", nil, nil, sibling, resolver},
		{"operator resume agrees with receipt", []string{autoClaim, registered}, verified("auto"), docs, autoRule},
		{"operator resume after switch to manual", []string{autoClaim, registered}, verified("manual"), sibling, resolver},
		{"operator resume after switch to auto", []string{manualClaim, registered}, verified("auto"), docs, resolver},
		{"operator resume unverified", []string{manualClaim, registered}, unverified, sibling, resolver},
		{"claim without mode", []string{strings.TrimSuffix(autoClaim, " mode=auto")}, unverified, sibling, resolver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_WORKER_AUTO_SCOPE", "1")
			e, o, _, _, ledger, ticket := recoveryFixture(t)
			e.VerifyMode = tc.verify
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

func TestClaimedModeTrustsOnlyTheCurrentClaim(t *testing.T) {
	registration := task.Comment{Body: "归属: 执行器 term=term_exec run=run_one attempt=1"}
	claim := task.Comment{Body: schedulerClaim("auto")}
	operator := task.Comment{Source: task.SourceOperator, Body: schedulerClaim("auto")}
	released := task.Comment{Body: "阶段: 未认领\n归属: 无"}
	for _, tc := range []struct {
		name       string
		comments   []task.Comment
		mode       string
		dispatched bool
	}{
		{"dispatched", []task.Comment{claim, registration}, "auto", true},
		{"operator event", []task.Comment{operator, registration}, "", false},
		{"released claim", []task.Comment{claim, released, registration}, "", false},
		{"another terminal", []task.Comment{claim, {Body: "归属: 执行器 term=term_other run=run_one attempt=1"}}, "auto", false},
		{"another run", []task.Comment{claim, {Body: "归属: 执行器 term=term_exec run=run_two attempt=1"}}, "auto", false},
		{"newer claim", []task.Comment{claim, registration, {Body: schedulerClaim("manual")}}, "manual", false},
		{"unknown mode", []task.Comment{{Body: schedulerClaim("turbo")}, registration}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if mode, dispatched := claimedMode(tc.comments, "term_exec", "run_one"); mode != tc.mode || dispatched != tc.dispatched {
				t.Fatalf("mode=%q dispatched=%v", mode, dispatched)
			}
		})
	}
}

func TestRecordedClaimCarriesMode(t *testing.T) {
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
	const terminal, run = "term_fdf45555-3378-4ca8-99b1-6200fc3106fc", "run_c2d32709f17c"
	const claim = "归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1 since=2026-10-05T14:08:34Z"
	comments := envelope.Data.Issue.Comments.Nodes
	if mode, dispatched := claimedMode(comments, terminal, run); mode != "" || dispatched {
		t.Fatalf("recorded claim without mode: mode=%q dispatched=%v", mode, dispatched)
	}
	derived := slices.Clone(comments)
	for i := range derived {
		derived[i].Body = strings.Replace(derived[i].Body, claim, claim+" mode=auto", 1)
	}
	if mode, dispatched := claimedMode(derived, terminal, run); mode != "auto" || !dispatched {
		t.Fatalf("recorded claim with mode: mode=%q dispatched=%v", mode, dispatched)
	}
}
