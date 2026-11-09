package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
)

var gatewayKeys = []string{"AGENT_FEISHU_APP_ID", "AGENT_FEISHU_APP_SECRET", "AGENT_FEISHU_INTAKE_CHAT", "AGENT_FEISHU_INTAKE_USERS", "AGENT_FEISHU_BOT_OPEN_ID", "AGENT_FEISHU_ENDPOINT", "AGENT_INTAKE_CLAUDE", "AGENT_INTAKE_PI", "AGENT_FEISHU_INTAKE_PROJECT", "AGENT_FEISHU_APPROVERS"}

var optionalGatewayKeys = []string{"AGENT_FEISHU_ENDPOINT", "AGENT_INTAKE_CLAUDE", "AGENT_INTAKE_PI", "AGENT_FEISHU_INTAKE_PROJECT", "AGENT_FEISHU_APPROVERS"}

func loadGatewaySettings() (map[string]string, error) {
	settings := map[string]string{}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(home, ".config/orca-factory/driver.env"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("driver.env could not be read")
	}
	if err == nil {
		settings, err = config.ParseEnv(f)
		err = errors.Join(err, f.Close())
		if err != nil {
			return nil, errors.New("driver.env is invalid")
		}
	}
	var missing []string
	for _, key := range gatewayKeys {
		if v := os.Getenv(key); v != "" {
			settings[key] = v
		}
		settings[key] = strings.TrimSpace(settings[key])
		if !slices.Contains(optionalGatewayKeys, key) && settings[key] == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing configuration: %s", strings.Join(missing, ", "))
	}
	if len(gatewayUsers(settings["AGENT_FEISHU_INTAKE_USERS"])) == 0 {
		return nil, errors.New("AGENT_FEISHU_INTAKE_USERS must contain open_ids")
	}
	return settings, nil
}

// tierParts splits a repository's AGENT_ROLE_INTAKE (or AGENT_TIER_STRONG
// fallback) "claude:model:effort" tier into its model and effort.
func tierParts(env map[string]string) (model, effort string, err error) {
	tier := env["AGENT_ROLE_INTAKE"]
	if tier == "" {
		tier = env["AGENT_TIER_STRONG"]
	}
	parts := strings.Split(tier, ":")
	if parts[0] != "claude" || len(parts) > 3 {
		return "", "", errors.New("需求助手档位不是 claude（AGENT_ROLE_INTAKE）")
	}
	parts = append(parts, "", "")
	return parts[1], parts[2], nil
}

func intakeConcurrency(env map[string]string) (int, error) {
	raw := env["AGENT_INTAKE_CONCURRENCY"]
	if raw == "" {
		return 2, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, errors.New("AGENT_INTAKE_CONCURRENCY must be a positive integer")
	}
	return n, nil
}

// intakeLedger opens the ledger Store scoped to this project's own
// repository env (its AGENT_FEISHU_PROJECT remote): the gateway submits
// confirmed drafts through it (OF-15) and reads it for the ticket context.
func intakeLedger(ctx context.Context, env map[string]string, tuning config.Tuning) (issues.Store, error) {
	return issues.New(ctx, env, tuning)
}

// projectRuntime lazily builds and caches each eligible project's heavy
// wiring (Feishu ledger Store validation, model/effort tier, Runner) by
// prefix, across repeated discovery runs (OF-42): a new Feishu topic
// rediscovers the live eligible set, but only the first topic that actually
// dispatches to a given prefix pays for building its Store and Runner.
type projectRuntime struct {
	mu     sync.Mutex
	claude string
	// piTier selects the pi --mode rpc execution path when non-empty
	// (AGENT_INTAKE_PI); piBinaryErr defers the "pi not installed" failure
	// to projects that actually opt into the path.
	piTier       string
	piBinary     string
	piBinaryErr  error
	piSessionDir string
	piConfigDir  string
	cacheDir     string
	tuning       config.Tuning
	built        map[string]*intake.Runner
	projects     map[string]intake.Project
	// ledgers holds each resolved project's full issues.Store (its intake
	// Project only exposes the narrower intake.Ledger): the draft_ticket
	// submission reads dependencies and creates tickets through it (OF-417).
	ledgers map[string]issues.Store
	// sources and readers back the Agent's read-only evidence queries
	// (OF-427), keyed by the same prefix as ledgers. logOwners is the latest
	// discovery round's logDirOwners.
	sources   map[string]querySource
	logOwners map[string]map[string]bool
	readers   evidenceReaders
}

func newProjectRuntime(claude, cacheDir string, tuning config.Tuning) *projectRuntime {
	return &projectRuntime{claude: claude, cacheDir: cacheDir, tuning: tuning, built: map[string]*intake.Runner{}, projects: map[string]intake.Project{}, ledgers: map[string]issues.Store{},
		sources: map[string]querySource{}, readers: defaultEvidenceReaders()}
}

// setPi installs the gateway-wide pi RPC configuration: the tier comes from
// AGENT_INTAKE_PI and the binary resolves like claude's, but only projects
// whose own env opts into the tier need pi installed at all.
func (r *projectRuntime) setPi(tier, sessionDir, configDir string) {
	r.piTier = tier
	r.piSessionDir = sessionDir
	r.piConfigDir = configDir
	// The binary resolves regardless of the gateway-level tier: a project
	// whose own env opts into AGENT_INTAKE_PI must get the pi path even
	// when the gateway setting is empty.
	binary, err := exec.LookPath("pi")
	if err == nil && !filepath.IsAbs(binary) {
		binary, err = filepath.Abs(binary)
	}
	if err != nil {
		r.piBinaryErr = errors.New("AGENT_INTAKE_PI 需要 pi 在 PATH 上")
		return
	}
	r.piBinary = binary
}

// stop ends every built Runner's pi janitor and resident processes; the
// gateway defers it for its whole lifetime.
func (r *projectRuntime) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, runner := range r.built {
		runner.Stop()
	}
}

// resolve returns the fully wired intake.Project for an eligible discovery
// result, building and caching its Ledger Store and Runner the first time
// this prefix is seen; later discovery rounds reuse the cached wiring but
// refresh the query source from the current profile (OF-427).
func (r *projectRuntime) resolve(ctx context.Context, p projects.Project) (intake.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	chat := p.Repository.Env["AGENT_FEISHU_NOTIFY_CHAT"]
	if cached, ok := r.projects[p.Prefix]; ok {
		cached.Chat = chat
		r.sources[p.Prefix] = newQuerySource(r.ledgers[p.Prefix], p.Path, p.Remote, p.Repository.Env)
		return cached, nil
	}
	model, effort, err := tierParts(p.Repository.Env)
	if err != nil {
		return intake.Project{}, err
	}
	// A repository's own AGENT_INTAKE_PI opts it into the pi RPC path
	// (OF-416), falling back to the gateway-wide tier; empty keeps claude.
	piTier := strings.TrimSpace(p.Repository.Env["AGENT_INTAKE_PI"])
	if piTier == "" {
		piTier = r.piTier
	}
	var piProvider, piModel, piThinking string
	if piTier != "" {
		if r.piBinaryErr != nil {
			return intake.Project{}, r.piBinaryErr
		}
		if piProvider, piModel, piThinking, err = intake.ParsePiTier(piTier); err != nil {
			return intake.Project{}, err
		}
	}
	store, err := intakeLedger(ctx, p.Repository.Env, r.tuning)
	if err != nil {
		return intake.Project{}, fmt.Errorf("账本打开失败：%w", err)
	}
	checkout := filepath.Join(r.cacheDir, "orca-factory", "intake", p.Prefix, "checkout")
	project := intake.Project{Prefix: p.Prefix, Name: p.Name, Remote: p.Remote, Enabled: true, Repo: p.Path, Checkout: checkout, Ledger: store, Chat: chat}
	// core/gate.sh always treats scripts/agent-gates/* as lane 2.
	project.Lane2Globs, project.Lane3Globs = "scripts/agent-gates/* "+p.Repository.Env["AGENT_LANE2_GLOBS"], p.Repository.Env["AGENT_LANE3_GLOBS"]
	runner := &intake.Runner{Claude: r.claude, Repo: p.Path, Checkout: checkout, Model: model, Effort: effort, Timeout: 5 * time.Minute, Tickets: intakeTickets(store, p.Path, r.readers)}
	if piTier != "" {
		runner.PiBinary = r.piBinary
		runner.PiProvider, runner.PiModel, runner.PiThinking = piProvider, piModel, piThinking
		runner.PiSessionDir = r.piSessionDir
		runner.PiConfigDir = r.piConfigDir
		runner.Start()
	}
	r.built[p.Prefix] = runner
	r.projects[p.Prefix] = project
	r.ledgers[p.Prefix] = store
	r.sources[p.Prefix] = newQuerySource(store, p.Path, p.Remote, p.Repository.Env)
	return project, nil
}

// agent is the Turn dispatcher passed to intake.New: every prefix a Turn can
// name was already resolved (discoverIntakeProjects only ever hands the
// gateway prefixes it resolved), so a miss here means no local repository
// ever matched that project. Each turn serves the Agent's read-only
// intake-query-v1 loop against the bound project (OF-427); only the final
// answer is shown, and only it may carry a draft_ticket block, whose
// receipt is appended after submission (OF-417).
func (r *projectRuntime) agent(ctx context.Context, t intake.Turn) (string, error) {
	answer, err := intake.RunQueries(ctx, r.turn, r.query, t)
	if err != nil {
		return "", err
	}
	if receipt, ok := r.submitDraftTicket(ctx, t, answer); ok {
		answer += "\n\n" + receipt
	}
	return answer, nil
}

// turn dispatches one Turn to the project's built Runner.
func (r *projectRuntime) turn(ctx context.Context, t intake.Turn) (string, error) {
	r.mu.Lock()
	runner, ok := r.built[t.Project.Prefix]
	r.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("intake gateway: no local repository for project %s", t.Project.Prefix)
	}
	return runner.Turn(ctx, t)
}

// draftTicketFence isolates the Agent's draft_ticket JSON block from the
// surrounding reply text, which the group sees as-is.
var draftTicketFence = regexp.MustCompile("(?s)```draft_ticket\\s*\\n(.*?)\\n```")

// draftTicketAllowedLabels is the only label set a draft may carry: the
// agent's reply is untrusted data, so unknown labels are dropped, never
// passed through to the ledger. Extend this fixed set deliberately.
var draftTicketAllowedLabels = map[string]bool{}

// draftTicketWire is the Agent's draft_ticket JSON (OF-417): one fully
// specified ticket the gateway validates and creates on the Agent's behalf.
type draftTicketWire struct {
	Title    string   `json:"title"`
	Todo     []string `json:"todo"`
	Check    string   `json:"check"`
	Scope    string   `json:"scope"`
	Blockers []string `json:"blockers"`
	Related  []string `json:"related"`
	Labels   []string `json:"labels"`
}

// parseDraftTicket extracts the draft_ticket block from an agent reply.
// found is false when the answer carries no block; a present but malformed
// block comes back as a problem instead of being silently ignored.
func parseDraftTicket(answer string) (draft draftTicketWire, problems []string, found bool) {
	m := draftTicketFence.FindStringSubmatch(answer)
	if m == nil {
		return draftTicketWire{}, nil, false
	}
	if err := json.Unmarshal([]byte(m[1]), &draft); err != nil {
		return draftTicketWire{}, []string{"draft_ticket 不是合法 JSON：" + err.Error()}, true
	}
	return draft, nil, true
}

// draftTicketFieldProblems applies the adopt-style face requirement: 要做,
// CHECK and SCOPE must all be present and non-empty.
func draftTicketFieldProblems(d draftTicketWire) []string {
	var missing []string
	if strings.TrimSpace(d.Title) == "" {
		missing = append(missing, "title")
	}
	if len(d.Todo) == 0 || slices.ContainsFunc(d.Todo, func(s string) bool { return strings.TrimSpace(s) == "" }) {
		missing = append(missing, "todo（非空数组，且每项非空）")
	}
	if strings.TrimSpace(d.Check) == "" {
		missing = append(missing, "check")
	}
	if strings.TrimSpace(d.Scope) == "" {
		missing = append(missing, "scope")
	}
	return missing
}

// draftTicketDependencies validates blockers/related like issues.Adopt:
// both arrays must be explicit (nil is not a complete list), free of
// duplicates, and every reference must resolve to a Complete task. It
// returns the canonical IDs the ticket is created with.
func draftTicketDependencies(ctx context.Context, store issues.Store, d draftTicketWire) (blockers, related []string, problems []string) {
	if d.Blockers == nil || d.Related == nil {
		return nil, nil, []string{"blockers、related 必须是完整数组（没有依赖写 []，不能省略或写 null）"}
	}
	refs := slices.Concat(d.Blockers, d.Related)
	if len(slices.Compact(slices.Sorted(slices.Values(refs)))) != len(refs) {
		return nil, nil, []string{"blockers、related 中有重复的票"}
	}
	blockers, problems = resolveDraftTicketRefs(ctx, store, d.Blockers)
	related, relatedProblems := resolveDraftTicketRefs(ctx, store, d.Related)
	return blockers, related, append(problems, relatedProblems...)
}

func resolveDraftTicketRefs(ctx context.Context, store issues.Store, refs []string) (ids []string, problems []string) {
	for _, ref := range refs {
		dep, err := store.Read(ctx, ref)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("依赖 %s 在账本中不存在", ref))
		case !dep.Complete:
			problems = append(problems, fmt.Sprintf("依赖 %s 尚未纳管", ref))
		default:
			ids = append(ids, dep.ID)
		}
	}
	return ids, problems
}

// similarTicket finds an open ledger ticket whose title looks like the
// draft, so a repeated proposal never creates a duplicate.
func similarTicket(ctx context.Context, store issues.Store, title string) (issues.Task, bool, error) {
	tasks, err := store.List(ctx)
	if err != nil {
		return issues.Task{}, false, err
	}
	for _, t := range tasks {
		if closedTicketState(t.Metadata.State) {
			continue
		}
		if similarTitle(t.Title, title) {
			return t, true, nil
		}
	}
	return issues.Task{}, false, nil
}

func closedTicketState(state string) bool {
	switch state {
	case "Done", "Canceled", "Cancelled", "Duplicate":
		return true
	}
	return false
}

// similarTitle compares normalized titles: equality, containment, or a
// character-bigram overlap (shared bigrams over the smaller title's
// bigram set) of at least 0.6 counts as similar.
func similarTitle(a, b string) bool {
	na, nb := normalizeTitle(a), normalizeTitle(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb || strings.Contains(na, nb) || strings.Contains(nb, na) {
		return true
	}
	ga, gb := titleBigrams(na), titleBigrams(nb)
	shared := 0
	for g := range ga {
		if gb[g] {
			shared++
		}
	}
	smaller := min(len(ga), len(gb))
	return smaller > 0 && float64(shared) >= 0.6*float64(smaller)
}

func normalizeTitle(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, s)
}

func titleBigrams(s string) map[string]bool {
	runes := []rune(s)
	out := make(map[string]bool, len(runes))
	for i := 0; i+1 < len(runes); i++ {
		out[string(runes[i:i+2])] = true
	}
	return out
}

// draftTicketDescription renders the create face both backends adopt:
// 要做/验收/SCOPE sections plus a 依赖 section whose DEPENDENCIES line
// issues.Adopt re-checks against the creation input.
func draftTicketDescription(session, prompt string, d draftTicketWire, blockers, related []string) string {
	deps, _ := json.Marshal(issues.AdoptInput{Blockers: blockers, Related: related})
	var todo strings.Builder
	for i, item := range d.Todo {
		fmt.Fprintf(&todo, "%d. %s\n", i+1, strings.TrimSpace(item))
	}
	return fmt.Sprintf("## 现状\nintake draft_ticket 代建，需求原话摘要：%s\n\n## 要做\n%s## 验收\nCHECK: %s\n\n## SCOPE\n%s\n\n## 依赖\nDEPENDENCIES: %s\n\n## 来源\nintake 会话 %s\n",
		draftTicketSummary(prompt), todo.String(), strings.TrimSpace(d.Check), strings.Join(strings.Fields(d.Scope), " "), deps, session)
}

// draftTicketSummary truncates the requester's original words for the
// provenance comment and the ticket face.
func draftTicketSummary(prompt string) string {
	runes := []rune(strings.TrimSpace(prompt))
	if len(runes) > 200 {
		return string(runes[:200]) + "…"
	}
	return string(runes)
}

// draftTicketLabels keeps only the fixed allowlist; the agent's reply is
// untrusted, so unknown labels never reach the ledger.
func draftTicketLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if draftTicketAllowedLabels[l] {
			out = append(out, l)
		}
	}
	return out
}

// createDraftTicket creates the validated draft at Todo (never
// ready-for-agent: a human confirms the need before that label) and writes
// the provenance comment OF-417 requires.
func createDraftTicket(ctx context.Context, store issues.Store, session, prompt string, d draftTicketWire, blockers, related []string) (issues.Task, error) {
	task, err := store.Create(ctx, issues.CreateInput{
		Title:       strings.TrimSpace(d.Title),
		Description: draftTicketDescription(session, prompt, d, blockers, related),
		State:       "Todo",
		Labels:      draftTicketLabels(d.Labels),
		Blockers:    blockers,
		Related:     related,
	})
	if err != nil {
		return issues.Task{}, fmt.Errorf("代建票据: %w", err)
	}
	if err := auditDraftTicket(ctx, store, task, session, prompt); err != nil {
		fmt.Fprintln(os.Stderr, "intake draft ticket audit failed:", err)
	}
	return task, nil
}

// auditDraftTicket writes the source audit comment on the created ticket; a
// failure is logged and left in the receipt but does not undo the creation.
func auditDraftTicket(ctx context.Context, store issues.Store, t issues.Task, session, prompt string) error {
	body := fmt.Sprintf("来源审计：intake 会话 %s\n需求原话摘要：%s", session, draftTicketSummary(prompt))
	if _, err := store.CreateComment(ctx, t.ID, body); err != nil {
		return fmt.Errorf("审计评论写入失败: %w", err)
	}
	return nil
}

// submitDraftTicket turns a draft_ticket block in the agent's answer into a
// ledger ticket: validate, dedup by title, create at Todo with a provenance
// comment, and report back in the thread. It returns ok=false when the
// answer carries no draft_ticket block, leaving the reply untouched.
func (r *projectRuntime) submitDraftTicket(ctx context.Context, t intake.Turn, answer string) (receipt string, ok bool) {
	d, problems, found := parseDraftTicket(answer)
	if !found {
		return "", false
	}
	header := "[网关] draft_ticket："
	r.mu.Lock()
	store := r.ledgers[t.Project.Prefix]
	r.mu.Unlock()
	if store == nil {
		return header + "该项目账本不可用，未建票", true
	}
	if problems == nil {
		problems = draftTicketFieldProblems(d)
	}
	var blockers, related []string
	if len(problems) == 0 {
		blockers, related, problems = draftTicketDependencies(ctx, store, d)
	}
	if len(problems) == 0 {
		if existing, similar, err := similarTicket(ctx, store, d.Title); err != nil {
			problems = []string{"账本读取失败，无法核对重复：" + err.Error()}
		} else if similar {
			return header + fmt.Sprintf("与在途票 %s《%s》标题高度相似，未重复建票；如确为新需求请改标题后重新输出", existing.Metadata.Identifier, existing.Title), true
		}
	}
	if len(problems) > 0 {
		return header + "校验未通过，未建票：\n- " + strings.Join(problems, "\n- "), true
	}
	task, err := createDraftTicket(ctx, store, t.Session, t.Prompt, d, blockers, related)
	if err != nil {
		fmt.Fprintln(os.Stderr, "intake draft ticket create failed:", err)
		return header + "建票失败：" + err.Error(), true
	}
	return header + fmt.Sprintf("已建票 %s《%s》\n%s\n状态 %s，未打 ready-for-agent；确认需求后由人打标放行", task.Metadata.Identifier, task.Title, task.URL, task.Metadata.State), true
}

// discoverIntakeProjects re-runs project discovery (OF-42: live Orca
// repository inventory, the Feishu ledger's project table, each repository's
// own pipeline check) and resolves every eligible repository's wiring via
// runtime. A failure here is the discovery round itself failing outright
// (the live Orca repository list or the ledger project table could not be
// read): there is then no row to report anything against, so the caller
// must not install an empty project set over an already-working one.
func discoverIntakeProjects(ctx context.Context, cfg config.Config, client *orca.Client, runtime *projectRuntime) ([]intake.Project, []intake.Onboardable, []intake.PendingEnable, error) {
	result, rows, err := discoverProjects(ctx, cfg, client)
	if err != nil {
		return nil, nil, nil, err
	}
	return resolveIntakeProjects(ctx, result, rows, runtime), resolveOnboardable(result), resolvePendingEnable(result, rows), nil
}

// resolveOnboardable extracts OF-49's discovery result: repositories Orca
// knows about but this driver has never onboarded (scripts/agent-gates/ does
// not exist at all), named by their local checkout's own directory.
func resolveOnboardable(result projects.Result) []intake.Onboardable {
	var out []intake.Onboardable
	for _, r := range result.Rejected {
		if r.Reason != projects.ReasonOnboardable {
			continue
		}
		out = append(out, intake.Onboardable{Name: filepath.Base(r.Path), Remote: r.Remote, Path: r.Path})
	}
	return out
}

// resolvePendingEnable extracts OF-54's discovery result: repositories whose
// pipeline is installed and own check passes (or only lags on core, OF-53)
// but whose ledger project-table row still has 启用=false. Unlike
// resolveOnboardable, these already have a ledger row (ReasonProjectDisabled
// requires one), so Prefix and Name come from rows rather than the local
// checkout's directory name.
func resolvePendingEnable(result projects.Result, rows []issues.LedgerProject) []intake.PendingEnable {
	byRemote := make(map[string]issues.LedgerProject, len(rows))
	for _, row := range rows {
		byRemote[row.Remote] = row
	}
	var out []intake.PendingEnable
	for _, r := range result.Rejected {
		if r.Reason != projects.ReasonProjectDisabled {
			continue
		}
		row, ok := byRemote[r.Remote]
		if !ok {
			continue
		}
		out = append(out, intake.PendingEnable{Prefix: row.Prefix, Name: row.Name, Remote: r.Remote, Path: r.Path})
	}
	return out
}

// resolveIntakeProjects maps one discovery round's eligible/rejected
// repositories and ledger project rows into the gateway's routable set.
// Every ledger row is returned: an eligible repository whose own wiring
// fails to resolve (OF-48: AGENT_ROLE_INTAKE/AGENT_TIER_STRONG is not a
// claude tier, or its ledger Store fails to open) is listed unavailable with
// that failure as its Reason instead of aborting the whole round, exactly
// like a repository discovery itself rejected; every other row still
// resolves. A row discovery did not consider eligible carries the rejection
// reason discovery already gave it.
func resolveIntakeProjects(ctx context.Context, result projects.Result, rows []issues.LedgerProject, runtime *projectRuntime) []intake.Project {
	owners := logDirOwners(result, rows)
	runtime.mu.Lock()
	runtime.logOwners = owners
	runtime.mu.Unlock()
	rejections := make(map[string]projects.Rejection, len(result.Rejected))
	for _, rejected := range result.Rejected {
		rejections[rejected.Remote] = rejected
	}
	eligible := make(map[string]projects.Project, len(result.Eligible))
	for _, p := range result.Eligible {
		eligible[p.Remote] = p
	}
	out := make([]intake.Project, 0, len(rows))
	for _, row := range rows {
		if p, ok := eligible[row.Remote]; ok {
			project, err := runtime.resolve(ctx, p)
			if err != nil {
				out = append(out, intake.Project{Prefix: row.Prefix, Name: row.Name, Remote: row.Remote, Enabled: row.Enabled, Reason: err.Error(), Chat: p.Repository.Env["AGENT_FEISHU_NOTIFY_CHAT"]})
				continue
			}
			out = append(out, project)
			continue
		}
		rejected := rejections[row.Remote]
		out = append(out, intake.Project{Prefix: row.Prefix, Name: row.Name, Remote: row.Remote, Enabled: row.Enabled, Reason: string(rejected.Reason), Chat: rejected.Chat})
	}
	return out
}

// refreshIntakeProjects rediscovers and installs the gateway's current
// routable project set (OF-42). A discovery failure is logged and leaves the
// gateway's existing projects untouched: a transient Orca or ledger error
// must not drop an already-working project out of the picker.
func refreshIntakeProjects(ctx context.Context, gateway *intake.Gateway, cfg config.Config, client *orca.Client, runtime *projectRuntime) {
	fresh, onboardable, pendingEnable, err := discoverIntakeProjects(ctx, cfg, client, runtime)
	if err != nil {
		fmt.Fprintln(os.Stderr, "intake gateway: project discovery failed:", err)
		return
	}
	if err := gateway.SetProjects(fresh); err != nil {
		fmt.Fprintln(os.Stderr, "intake gateway: invalid discovered projects:", err)
	}
	gateway.SetOnboardable(onboardable)
	gateway.SetPendingEnable(pendingEnable)
}

// isNewIntakeTopic reports whether event opens a new thread rather than
// replying inside one Feishu already assigned a thread_id: discovery only
// needs to run again when a topic is starting (OF-42).
func isNewIntakeTopic(event *im.P2MessageReceiveV1) bool {
	return event != nil && event.Event != nil && event.Event.Message != nil && value(event.Event.Message.ThreadId) == ""
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func gatewayUsers(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

func init() { Register("feishu-gateway", feishuGateway) }

func feishuGateway(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: orca-factory-driver feishu-gateway")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := runFeishuGateway(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runFeishuGateway(ctx context.Context) error {
	settings, err := loadGatewaySettings()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	claude, err := resolveClaude(settings["AGENT_INTAKE_CLAUDE"])
	if err != nil {
		return err
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	concurrency, err := intakeConcurrency(map[string]string{"AGENT_INTAKE_CONCURRENCY": os.Getenv("AGENT_INTAKE_CONCURRENCY")})
	if err != nil {
		return err
	}
	runtime := newProjectRuntime(claude, cacheDir, cfg.Tuning)
	runtime.setPi(settings["AGENT_INTAKE_PI"], filepath.Join(logStateRoot(), "intake-pi-sessions"), filepath.Join(home, ".pi", "agent"))
	defer runtime.stop()
	orcaClient := &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout}
	dir := filepath.Join(logStateRoot(), "intake")
	lock, err := flock.TryLock(filepath.Join(dir, "gateway.lock"))
	if err != nil {
		return errors.New("cannot acquire intake gateway lock")
	}
	defer func() { _ = lock.Close() }()
	client, err := feishu.New(feishu.Options{AppID: settings["AGENT_FEISHU_APP_ID"], AppSecret: settings["AGENT_FEISHU_APP_SECRET"], Endpoint: settings["AGENT_FEISHU_ENDPOINT"]})
	if err != nil {
		return err
	}
	// Projects are discovered on demand (OF-42): the gateway starts with none
	// and rediscovers the live Orca/ledger state when each new topic begins.
	gateway, err := intake.New(dir, settings["AGENT_FEISHU_INTAKE_CHAT"], settings["AGENT_FEISHU_BOT_OPEN_ID"], gatewayUsers(settings["AGENT_FEISHU_INTAKE_USERS"]), gatewayUsers(settings["AGENT_FEISHU_APPROVERS"]), concurrency, nil, settings["AGENT_FEISHU_INTAKE_PROJECT"], runtime.agent, client.ReplyInThread, client.AddReaction, client.ReplyCardInThread, client.UpdateCard)
	if err != nil {
		return err
	}
	if cfg.FactoryRoot != "" {
		gateway.SetOnboarder(onboarder{cfg: cfg, orca: orcaClient, claude: claude, stateRoot: logStateRoot(), cacheDir: cacheDir, writeRow: factoryLedgerRow(cfg), preflight: defaultPreflight}.run)
		gateway.SetEnabler(enabler{checker: factoryChecker(cfg.FactoryRoot), writeRow: factoryEnabledWriter(cfg), cleanup: onboardSceneCleaner(cacheDir)}.run)
	}
	// Replies in a project chat's open threads arrive before any new topic would
	// trigger discovery, and that chat is only known from discovery.
	refreshIntakeProjects(ctx, gateway, cfg, orcaClient, runtime)
	handler := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(func(ctx context.Context, event *im.P2MessageReceiveV1) error {
		if isNewIntakeTopic(event) {
			refreshIntakeProjects(ctx, gateway, cfg, orcaClient, runtime)
		}
		return gateway.Handle(ctx, event)
	}).OnP2CardActionTrigger(func(_ context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		// The callback must be answered within Feishu's deadline, while the
		// card update and the first agent turn can take much longer.
		response, settle := gateway.HandleCardAction(event)
		if settle != nil {
			go func() {
				if err := settle(ctx); err != nil {
					fmt.Fprintln(os.Stderr, "intake gateway: card action failed:", err)
				}
			}()
		}
		return response, nil
	})
	handler.Config.Logger = gatewaySDKLogger{}
	options := []ws.ClientOption{ws.WithEventHandler(handler), ws.WithAutoReconnect(true), ws.WithLogger(gatewaySDKLogger{})}
	if endpoint := settings["AGENT_FEISHU_ENDPOINT"]; endpoint != "" {
		options = append(options, ws.WithDomain(endpoint))
	}
	connection := ws.NewClient(settings["AGENT_FEISHU_APP_ID"], settings["AGENT_FEISHU_APP_SECRET"], options...)
	err = connection.Start(ctx)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return errors.New("feishu gateway connection failed")
	}
	return nil
}

// SDK diagnostics can contain payloads and credential-bearing connection URLs.
// Keep only the severity at this boundary; never format SDK arguments.
type gatewaySDKLogger struct{}

func (gatewaySDKLogger) Debug(context.Context, ...interface{}) {}
func (gatewaySDKLogger) Info(context.Context, ...interface{})  {}
func (gatewaySDKLogger) Warn(context.Context, ...interface{}) {
	fmt.Fprintln(os.Stderr, "feishu gateway SDK warning")
}
func (gatewaySDKLogger) Error(context.Context, ...interface{}) {
	fmt.Fprintln(os.Stderr, "feishu gateway SDK error")
}
