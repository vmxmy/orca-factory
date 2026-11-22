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
			e.Launch.Dispatched, e.LaunchReceipts = true, true
			ledger.comments = []string{modeReceipt(tc.mode)}
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

func modeReceipt(mode string) string {
	return "派工档位: term=term_exec attempt=1 mode=" + mode
}

func TestWorkerScopeBlockedFollowsVerifiedMode(t *testing.T) {
	const docs, sibling = "docs/system-manual.md", "unit/change_test.go"
	const claim = "阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1 since=2026-10-05T05:59:00Z"
	approvals := []string{"扩SCOPE：auto 2026-11-21T00:00:00Z", "扩SCOPE：worker-auto 2026-11-21T01:00:00Z"}
	launched := func(mode string, more ...string) []string {
		return append([]string{claim, modeReceipt(mode)}, more...)
	}
	const registered = "阶段: worker 运行中\n归属: 执行器 term=term_exec run=run_one attempt=1 since=2026-10-05T05:59:30Z"
	otherTerminal := "派工档位: term=term_other attempt=1 mode=auto"
	verified := func(mode string, lane3 ...string) func(context.Context) (ScopeMode, error) {
		return func(context.Context) (ScopeMode, error) { return ScopeMode{Mode: mode, Lane3Globs: lane3}, nil }
	}
	unverified := func(context.Context) (ScopeMode, error) {
		return ScopeMode{}, errors.New("repository is not taken over")
	}
	const autoRule, whitelist, resolver = "扩SCOPE：worker-auto ", "自动扩 SCOPE：", ""
	for _, tc := range []struct {
		name     string
		comments []string
		verify   func(context.Context) (ScopeMode, error)
		operator bool
		files    string
		want     string
	}{
		{"dispatched auto outranks checkout", launched("auto"), verified("manual"), false, docs, autoRule},
		{"dispatched auto after two approvals", launched("auto", approvals...), verified("manual"), false, sibling, resolver},
		{"dispatched manual outranks checkout", launched("manual"), verified("auto"), false, sibling, whitelist},
		{"dispatched manual rejects auto paths", launched("manual"), verified("auto"), false, docs, resolver},
		{"relaunch replaces the failed launch", []string{claim, otherTerminal, modeReceipt("manual")}, verified("auto"), false, docs, resolver},
		{"dispatched meets tracked lane three", []string{claim, modeReceipt("auto") + "\nlane3: .env* docs/*"}, nil, false, docs, resolver},
		{"dispatched without receipt", []string{claim}, verified("auto"), false, docs, resolver},
		{"dispatched with another terminal's receipt", []string{claim, otherTerminal}, verified("auto"), false, sibling, resolver},
		{"dispatched twice in one terminal", launched("auto", registered), verified("auto"), false, docs, resolver},
		{"operator start in auto repository", nil, verified("auto"), true, docs, autoRule},
		{"operator start meets current lane three", nil, verified("auto", "docs/*"), true, docs, resolver},
		{"operator start in auto repository after two approvals", approvals, verified("auto"), true, sibling, resolver},
		{"operator start in manual repository keeps whitelist", nil, verified("manual"), true, sibling, whitelist},
		{"operator start in manual repository rejects auto paths", nil, verified("manual"), true, docs, resolver},
		{"operator start on an undelivered claim", []string{claim}, verified("manual"), true, docs, resolver},
		{"operator start beside another terminal", []string{claim, otherTerminal}, verified("manual"), true, docs, resolver},
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
				e.Launch.Dispatched, e.LaunchReceipts = true, true
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

func TestWorkerScopeBlockedAwaitsLaunchReceipt(t *testing.T) {
	t.Setenv("AGENT_WORKER_AUTO_SCOPE", "")
	e, o, _, _, ledger, ticket := recoveryFixture(t)
	e.Launch.Dispatched, e.LaunchReceipts = true, true
	e.Launch.CallTimeout = 15 * time.Minute
	waited := time.Duration(0)
	e.Sleep = func(_ context.Context, wait time.Duration) error {
		if wait == launchReceiptInterval {
			if waited += wait; waited == e.Launch.CallTimeout+launchReceiptMargin {
				ledger.comments = append(ledger.comments, modeReceipt("auto"))
			}
		}
		return nil
	}
	ticket.Scope = "unit/change.go"
	store := &scopeIssues{budgetIssues: ledger, description: "## LANE 预判\n2\n\n## SCOPE\nunit/change.go\n\n## CHECK\ngo test ./unit\n"}
	e.Issues = store
	o.failFirst = true
	o.failBody = "SCOPE-BLOCKED: docs/system-manual.md"
	result, err := e.Execute(t.Context(), ticket)
	if err != nil || result.State.Corrections != 1 || !strings.Contains(store.description, "unit/change.go docs/system-manual.md") {
		t.Fatalf("err=%v state=%+v face=%q", err, result.State, store.description)
	}
}

func TestAutoApprovalRechecksItsSnapshot(t *testing.T) {
	const registered, claimed, human = "归属: 执行器 term=term_exec run=run_one attempt=1", "归属: 执行器 term=- run=- attempt=2", "转人: 人工接手"
	for _, tc := range []struct {
		name       string
		dispatched bool
		bodies     []string
		ok, lost   bool
	}{
		{"operator registered", false, []string{claimed, registered}, true, false},
		{"operator registration not visible", false, []string{claimed}, false, true},
		{"operator registration replaced", false, []string{"归属: 执行器 term=term_other run=run_two attempt=2"}, false, true},
		{"operator handed over before its registration is visible", false, []string{claimed, human}, false, true},
		{"operator handed to a human", false, []string{registered, human}, false, true},
		{"operator superseded", false, []string{registered, claimed}, false, true},
		{"operator restarted after release", false, []string{registered, "归属: 无", registered}, true, false},
		{"dispatched", true, []string{claimed, modeReceipt("auto"), registered}, true, false},
		{"dispatched registration not visible", true, []string{modeReceipt("auto")}, false, true},
		{"dispatched registered twice", true, []string{modeReceipt("auto"), registered, registered}, false, false},
		{"dispatched handed to a human", true, []string{modeReceipt("auto"), registered, human}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var comments []task.Comment
			for _, body := range tc.bodies {
				comments = append(comments, task.Comment{Source: task.SourceDriver, Body: body})
			}
			e := &Executor{Launch: Launch{Dispatched: tc.dispatched}}
			_, ok, err := e.autoApproval(t.Context(), Ticket{}, "term_exec", ScopeMode{Mode: "auto"})(comments)
			if ok != tc.ok || errors.Is(err, errLaunchSuperseded) != tc.lost || err != nil && !tc.lost {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestAutoApprovalReadsLane3AfterItsSnapshot(t *testing.T) {
	const profile = "AGENT_GH_REPO=owner/repo\nAGENT_ROLE_REVIEWER_CMD='review command'\nAGENT_TIER_DEFAULT=codex::low\nAGENT_LANE3_GLOBS="
	for _, tc := range []struct {
		name       string
		dispatched bool
		tracked    bool
		current    string
		rewritten  bool
		restored   bool
		ok         bool
	}{
		{name: "unchanged", dispatched: true, tracked: true, current: ".env*", ok: true},
		{name: "dispatched with a profile written since launch", dispatched: true, tracked: true, current: ".env*", rewritten: true},
		{name: "dispatched with the original profile restored", dispatched: true, tracked: true, restored: true},
		{name: "dispatched without a profile version", dispatched: true, current: ".env*"},
		{name: "operator outside the scheduler", current: "docs/*", rewritten: true, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			repo := t.TempDir()
			path := filepath.Join(repo, "scripts/agent-gates/agent-graph.env")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			write := func(globs string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(profile+"'"+globs+"'\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(".env*")
			version, err := ProfileVersion(repo)
			if err != nil {
				t.Fatal(err)
			}
			e, _, _, _, _, ticket := recoveryFixture(t)
			e.Config.Lane3Globs = []string{"start/*"}
			e.bindMode(repo, map[string]string{"AGENT_ISSUE_BACKEND": "feishu"}, nil)
			e.Launch.Dispatched = tc.dispatched
			mode := ScopeMode{Mode: "auto", Lane3Globs: []string{"tracked/*"}, Profile: version}
			if !tc.tracked {
				mode.Profile = ""
			}
			approve := e.autoApproval(t.Context(), ticket, "term_exec", mode)
			if tc.rewritten || tc.restored {
				time.Sleep(20 * time.Millisecond)
			}
			if tc.restored {
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				write("docs/*")
				if err := os.Rename(path+".original", path); err != nil {
					t.Fatal(err)
				}
			} else if tc.rewritten {
				write(tc.current)
			}
			lane3, ok, err := approve([]task.Comment{{Source: task.SourceDriver, Body: modeReceipt("auto")}, {Source: task.SourceDriver, Body: "归属: 执行器 term=term_exec run=run_one attempt=1"}})
			if err != nil || ok != tc.ok || ok && lane3 != "start/* tracked/* "+tc.current {
				t.Fatalf("lane3=%q ok=%v err=%v", lane3, ok, err)
			}
		})
	}
}

func TestBindModeFollowsTheBoundLedger(t *testing.T) {
	const github = "AGENT_ISSUE_BACKEND=github\nAGENT_GH_REPO=owner/repo\n"
	for _, tc := range []struct {
		name, start, approval string
		dispatched            bool
		want                  string
	}{
		{"feishu dispatched", "AGENT_ISSUE_BACKEND=feishu\nAGENT_GH_REPO=owner/repo\n", "AGENT_ISSUE_BACKEND=feishu\nAGENT_GH_REPO=owner/repo\nAGENT_MODE=manual\n", true, "auto"},
		{"feishu operator", "AGENT_ISSUE_BACKEND=feishu\nAGENT_GH_REPO=owner/repo\n", "", false, "discovered"},
		{"github dispatched", github + "AGENT_MODE=auto\n", github + "AGENT_MODE=auto\n", true, ""},
		{"github operator switched to manual", github + "AGENT_MODE=auto\n", github + "AGENT_MODE=manual\n", false, "manual"},
		{"github operator auto", github + "AGENT_MODE=auto\n", github + "AGENT_MODE=auto\n", false, "auto"},
		{"github operator empty mode", github, github + "AGENT_MODE=\n", false, ""},
		{"github operator moved instance", github, github + "AGENT_GH_API_URL=https://other.example/api\nAGENT_MODE=auto\n", false, ""},
		{"github operator names the default instance", github, github + "AGENT_GH_API_URL=https://api.github.com/\n", false, "manual"},
		{"github operator ignores feishu fields", github, github + "AGENT_FEISHU_PROJECT=host/two\n", false, "manual"},
		{"github operator moved backend", github, "AGENT_ISSUE_BACKEND=feishu\nAGENT_GH_REPO=owner/repo\nAGENT_MODE=auto\n", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			repo := t.TempDir()
			path := filepath.Join(repo, "scripts/agent-gates/agent-graph.env")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			write := func(profile string) {
				if err := os.WriteFile(path, []byte("AGENT_ROLE_REVIEWER_CMD='review command'\nAGENT_TIER_DEFAULT=codex::low\n"+profile), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(tc.start)
			_, bound, err := repositoryExecutor(t.Context(), repo)
			if err != nil {
				t.Fatal(err)
			}
			write(tc.approval)
			e, _, _, _, ledger, ticket := recoveryFixture(t)
			e.Issues = &scopeIssues{budgetIssues: ledger}
			ledger.comments = []string{modeReceipt("auto"), "归属: 执行器 term=term_exec run=run_one attempt=1"}
			e.Launch.Dispatched = tc.dispatched
			e.bindMode(repo, bound, func(_ context.Context, path string, env map[string]string) (ScopeMode, error) {
				if path != repo || !reflect.DeepEqual(env, bound) {
					t.Fatalf("verified %q %v", path, env)
				}
				return ScopeMode{Mode: "discovered"}, nil
			})
			if mode, err := e.scopeMode(t.Context(), ticket, "term_exec"); err != nil || mode.Mode != tc.want {
				t.Fatalf("mode=%+v err=%v, want %q", mode, err, tc.want)
			}
		})
	}
}

func TestWorkerScopeBlockedAfterSupersedingLaunchWritesNothing(t *testing.T) {
	e, o, _, _, ledger, ticket := recoveryFixture(t)
	e.Launch.Dispatched, e.LaunchReceipts = true, true
	ticket.Scope = "unit/change.go"
	store := &scopeIssues{budgetIssues: ledger, description: "## LANE 预判\n2\n\n## SCOPE\nunit/change.go\n\n## CHECK\ngo test ./unit\n"}
	e.Issues = store
	e.Sleep = func(_ context.Context, wait time.Duration) error {
		if wait == launchReceiptInterval {
			ledger.comments = append(ledger.comments, "阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=2 scheduler-attempt=2")
		}
		return nil
	}
	o.failFirst = true
	o.failBody = "SCOPE-BLOCKED: docs/system-manual.md"
	if _, err := e.Execute(t.Context(), ticket); !errors.Is(err, errLaunchSuperseded) {
		t.Fatalf("err=%v", err)
	}
	if comments := strings.Join(ledger.comments, "\n"); strings.Contains(comments, "Agent处置") || strings.Contains(comments, "扩SCOPE：") {
		t.Fatalf("superseded executor wrote the ticket: %q", comments)
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
	const registration = "归属: 执行器 term=" + terminal + " run=run_c2d32709f17c attempt=1"
	handedOver := envelope.Data.Issue.Comments.Nodes
	history := slices.DeleteFunc(slices.Clone(handedOver), func(c task.Comment) bool { return strings.HasPrefix(c.Body, "转人: ") })
	launched := time.Date(2026, 10, 5, 14, 8, 36, 0, time.UTC)
	event := func(source, body string) task.Comment {
		return task.Comment{Source: source, Body: body, CreatedAt: launched}
	}
	later := func(body string) task.Comment {
		return task.Comment{Source: task.SourceDriver, Body: body, CreatedAt: launched.Add(time.Minute)}
	}
	receipt := "派工档位: term=" + terminal + " attempt=1 mode=auto"
	for _, tc := range []struct {
		name       string
		events     []task.Comment
		want       string
		superseded bool
	}{
		{"driver receipt", []task.Comment{event(task.SourceDriver, receipt)}, "auto", false},
		{"no receipt", nil, "", false},
		{"operator event", []task.Comment{event(task.SourceOperator, receipt)}, "", false},
		{"event without source", []task.Comment{event("", receipt)}, "", false},
		{"quoted in another event", []task.Comment{event(task.SourceDriver, "阶段: gate\n"+receipt)}, "", false},
		{"another terminal", []task.Comment{event(task.SourceDriver, "派工档位: term=term_other attempt=1 mode=auto")}, "", false},
		{"unknown mode", []task.Comment{event(task.SourceDriver, "派工档位: term="+terminal+" attempt=1 mode=turbo")}, "", false},
		{"claimed again", []task.Comment{event(task.SourceDriver, receipt), later("阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=2 scheduler-attempt=2")}, "", true},
		{"launched again", []task.Comment{event(task.SourceDriver, receipt), later("派工档位: term=term_other attempt=2 mode=manual")}, "", true},
		{"released", []task.Comment{event(task.SourceDriver, receipt), later("阶段: 待领\n归属: 无")}, "", true},
		{"released in the same second", []task.Comment{event(task.SourceDriver, receipt), event(task.SourceDriver, "阶段: 待领\n归属: 无")}, "", true},
		{"earlier launch", []task.Comment{{Source: task.SourceDriver, Body: "派工档位: term=term_other attempt=1 mode=manual", CreatedAt: launched.Add(-time.Minute)}, event(task.SourceDriver, receipt)}, "auto", false},
		{"registered twice", []task.Comment{event(task.SourceDriver, receipt), event(task.SourceDriver, registration)}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode, _, err := launchedMode(slices.Concat(history, tc.events), terminal)
			if mode.Mode != tc.want || errors.Is(err, errLaunchSuperseded) != tc.superseded || err != nil && !tc.superseded {
				t.Fatalf("mode=%+v err=%v, want %q superseded=%v", mode, err, tc.want, tc.superseded)
			}
		})
	}
	if _, _, err := launchedMode(append(slices.Clone(handedOver), event(task.SourceDriver, receipt)), terminal); !errors.Is(err, errLaunchSuperseded) {
		t.Fatalf("recorded handoff to a human: err=%v", err)
	}
	if _, pending, err := launchedMode([]task.Comment{event(task.SourceDriver, receipt)}, terminal); !pending || err != nil {
		t.Fatalf("unseen registration: pending=%v err=%v", pending, err)
	}
}
