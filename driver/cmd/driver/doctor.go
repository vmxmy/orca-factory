package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func init() { Register("doctor", runDoctor) }

const (
	healthOK      = "ok"
	healthWarn    = "warn"
	healthUnknown = "unknown"
	healthFail    = "fail"
	// healthHuman marks a bootstrap prerequisite only a person can satisfy:
	// a login, a credential, a desktop install.
	healthHuman = "human"

	doctorLaunchWindow   = 30 * time.Minute
	doctorLaunchRepeats  = 3
	doctorFetchStale     = 30 * time.Minute
	doctorDeployGrace    = 60 * time.Minute
	doctorSyncPRStale    = 24 * time.Hour
	doctorServiceUnit    = "orca-factory-driver.service"
	doctorCommandTimeout = 30 * time.Second
)

const doctorUsage = "Usage: orca-factory-driver doctor [--json] | doctor --bootstrap [--json]"

// doctorChecks fixes the text output order; JSON keys are the same names.
var doctorChecks = []string{"service_path", "version", "maintenance", "checkouts", "sync_prs", "ledger", "launches", "orca_runtime", "host_memory", "credentials", "models"}

var doctorServiceTools = []string{"go", "gh", "git", "orca"}
var doctorAgentTools = []string{"claude", "codex"}
var doctorCredentialNames = []string{"AGENT_FEISHU_APP_ID", "AGENT_FEISHU_APP_SECRET"}

// doctorDeferredExits mark launch exits that postpone work without failing it.
var doctorDeferredExits = []string{"maintenance refused", "deferred", "recovery evidence changed"}

// healthCheck is one doctor finding. Reason and Details never carry raw
// command output or environment values: either can hold a credential.
type healthCheck struct {
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Details any    `json:"details,omitempty"`
	Fix     string `json:"fix,omitempty"`
	// unprobed marks a check with data it could not read; it is reported in
	// errors as well, like `orca-factory doctor` reports failed probes.
	unprobed bool
}

// healthReport mirrors `orca-factory doctor --json`: a top-level object keyed
// by name plus an errors string array. OF-128 consumes this shape.
type healthReport struct {
	Checks map[string]healthCheck `json:"checks"`
	Errors []string               `json:"errors"`
}

type doctorLedger interface {
	List(context.Context) ([]issues.Task, error)
	ListComments(context.Context, string) ([]task.Comment, error)
}

// doctorProbe is every input doctor reads. git and gh run from PATH.
type doctorProbe struct {
	now         time.Time
	stateRoot   string
	factoryRoot string
	serviceEnv  func(context.Context) (map[string]string, error)
	// projects returns the eligible repositories, and as Rejected only the
	// unloadedRepositories.
	projects        func(context.Context) (projects.Result, error)
	ledger          func(context.Context, projects.Project) (doctorLedger, error)
	githubReachable func(context.Context) error
	modelSettings   func() doctorModelSettings
	modelProbe      func(ctx context.Context, ref modelhealth.Ref, dir string, endpoint modelhealth.Endpoint) modelhealth.Status
	recordModels    bool
}

// unprobed is unknown, not fail: doctor runs every 30 minutes under OF-128,
// and an unreachable dependency is no finding anyone can act on.
func unprobed(reason string) healthCheck {
	return healthCheck{Status: healthUnknown, Reason: reason, unprobed: true}
}

func worst(statuses ...string) string {
	switch {
	case slices.Contains(statuses, healthFail):
		return healthFail
	case slices.Contains(statuses, healthUnknown):
		return healthUnknown
	case slices.Contains(statuses, healthWarn):
		return healthWarn
	}
	return healthOK
}

func commandFailure(what string, err error) string {
	return fmt.Sprintf("%s 失败（exit %d）", what, exitCode(err))
}

func runDoctor(args []string) int {
	if slices.Contains(args, "--bootstrap") {
		return runBootstrap(args)
	}
	probe, err := newDoctorProbe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return probe.main(ctx, args, os.Stdout)
}

func newDoctorProbe() (doctorProbe, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return doctorProbe{}, err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return doctorProbe{}, err
	}
	ledgers := issues.NewFactory(cfg.Tuning)
	return doctorProbe{
		now:          time.Now(),
		stateRoot:    logStateRoot(),
		modelProbe:   modelhealth.Probe,
		recordModels: true,
		modelSettings: func() doctorModelSettings {
			proxy, proxyErr := config.ModelProxyURL(home)
			settings := doctorModelSettings{proxyURL: proxy, proxyErr: proxyErr}
			// As loadGatewaySettings: driver.env always parses, then a non-empty
			// process value replaces the file value before trimming.
			file, err := (bootstrapProbe{home: home}).driverEnv()
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				settings.driverEnvErr = err
			}
			gateway := func(key string) string { return strings.TrimSpace(cmp.Or(os.Getenv(key), file[key])) }
			settings.intakePi = gateway("AGENT_INTAKE_PI")
			// The gateway resolves an empty value to claude on PATH, as an unset Binary does.
			settings.intakeClaude = gateway("AGENT_INTAKE_CLAUDE")
			_, settings.intakeClaudeErr = resolveClaude(settings.intakeClaude)
			return settings
		},
		factoryRoot: cfg.FactoryRoot,
		serviceEnv:  serviceEnviron,
		projects: func(ctx context.Context) (projects.Result, error) {
			result, ledger, err := discoverProjects(ctx, cfg, &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout})
			result.Rejected = unloadedRepositories(result.Rejected, ledger)
			return result, err
		},
		ledger: func(ctx context.Context, p projects.Project) (doctorLedger, error) {
			return ledgers.New(ctx, p.Repository.Env)
		},
		githubReachable: func(ctx context.Context) error { return httpReachable(ctx, "https://api.github.com/") },
	}, nil
}

func (p doctorProbe) main(ctx context.Context, args []string, out io.Writer) int {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) != 0 && !asJSON {
		fmt.Fprintln(os.Stderr, doctorUsage)
		return 2
	}
	report := p.run(ctx)
	if err := writeHealth(out, report, asJSON, doctorChecks); err != nil {
		fmt.Fprintln(os.Stderr, "cannot output doctor report")
		return 1
	}
	for _, check := range report.Checks {
		if check.Status == healthFail {
			return 1
		}
	}
	return 0
}

func (p doctorProbe) run(ctx context.Context) healthReport {
	env, envErr := p.serviceEnv(ctx)
	discovered, projectsErr := p.projects(ctx)
	eligible := discovered.Eligible
	state, stateErr := maintenance.Read()
	checks := map[string]healthCheck{
		"service_path": checkServicePath(env, envErr),
		"version":      p.checkVersion(ctx, state, stateErr),
		"maintenance":  p.checkMaintenance(state, stateErr),
		"credentials":  p.checkCredentials(ctx, env, envErr),
		"launches":     p.checkLaunches(),
		"orca_runtime": p.checkOrcaRuntime(),
		"host_memory":  p.checkHostMemory(),
	}
	if projectsErr != nil {
		for _, name := range []string{"checkouts", "sync_prs", "ledger", "models"} {
			checks[name] = unprobed("项目发现失败，无法列出已接管项目")
		}
	} else {
		checks["checkouts"] = p.checkCheckouts(ctx, eligible)
		checks["sync_prs"] = p.checkSyncPRs(ctx, eligible)
		checks["ledger"] = p.checkLedger(ctx, eligible)
		checks["models"] = p.checkModels(ctx, eligible, discovered.Rejected)
	}
	report := healthReport{Checks: checks, Errors: []string{}}
	for _, name := range doctorChecks {
		if checks[name].unprobed {
			report.Errors = append(report.Errors, name+": "+checks[name].Reason)
		}
	}
	return report
}

func writeHealth(out io.Writer, report healthReport, asJSON bool, names []string) error {
	if asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-13s %-8s %s\n", "CHECK", "STATUS", "REASON")
	for _, name := range names {
		check := report.Checks[name]
		fmt.Fprintf(&b, "%-13s %-8s %s\n", name, check.Status, check.Reason)
		if name == "models" {
			if rows, ok := check.Details.([]doctorModelStatus); ok {
				for _, row := range rows {
					fmt.Fprintf(&b, "  %s %s %s %s %s\n", row.Repository, row.Role, row.Status.Ref.String(), row.Status.State, row.Status.Reason)
				}
			}
		}
		if check.Fix != "" {
			fmt.Fprintf(&b, "%-13s %-8s 修法: %s\n", "", "", check.Fix)
		}
	}
	fmt.Fprintf(&b, "Errors [%s]\n", strings.Join(report.Errors, "; "))
	_, err := io.WriteString(out, b.String())
	return err
}

// serviceEnviron reads the running service's own environment, which already
// merges Environment=, EnvironmentFile= and every drop-in.
func serviceEnviron(ctx context.Context) (map[string]string, error) {
	out, err := runx.Run(ctx, runx.Spec{Path: "systemctl", Args: []string{"--user", "show", "--property", "MainPID", "--value", doctorServiceUnit}, Timeout: doctorCommandTimeout})
	if err != nil {
		return nil, errors.New(commandFailure("systemctl show", err))
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" || pid == "0" {
		return nil, errors.New(doctorServiceUnit + " 未运行")
	}
	raw, err := os.ReadFile(filepath.Join("/proc", pid, "environ"))
	if err != nil {
		return nil, errors.New("无法读取服务进程环境")
	}
	env := map[string]string{}
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if key, value, ok := strings.Cut(string(entry), "="); ok {
			env[key] = value
		}
	}
	return env, nil
}

func lookPathIn(path, name string) string {
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

func checkServicePath(env map[string]string, envErr error) healthCheck {
	if envErr != nil {
		return unprobed(envErr.Error())
	}
	resolved, missing := map[string]string{}, []string{}
	for _, tool := range append(slices.Clone(doctorServiceTools), doctorAgentTools...) {
		if found := lookPathIn(env["PATH"], tool); found != "" {
			resolved[tool] = found
		} else if slices.Contains(doctorServiceTools, tool) {
			missing = append(missing, tool)
		}
	}
	if resolved["claude"] == "" && resolved["codex"] == "" {
		missing = append(missing, "claude|codex")
	}
	details := map[string]any{"resolved": resolved, "missing": missing}
	if len(missing) > 0 {
		return healthCheck{Status: healthFail, Reason: "服务 PATH 解析不到 " + strings.Join(missing, ", "), Details: details}
	}
	return healthCheck{Status: healthOK, Reason: "服务 PATH 解析到全部工具", Details: details}
}

func eachLogRecord(path string, visit func(map[string]json.RawMessage)) (err error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		var fields map[string]json.RawMessage
		if json.Unmarshal(line, &fields) == nil {
			visit(fields)
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// runningVersion is the version of the newest serve start, searching the live
// scheduler log before its rotations.
func (p doctorProbe) runningVersion() (string, error) {
	live := filepath.Join(p.stateRoot, "scheduler.log")
	rotations, err := logsRotations(live)
	if err != nil {
		return "", err
	}
	slices.Reverse(rotations)
	for _, path := range append([]string{live}, rotations...) {
		version := ""
		err := eachLogRecord(path, func(fields map[string]json.RawMessage) {
			if logsString(fields["event"]) == "start" && logsString(fields["component"]) == "scheduler" {
				version = logsString(fields["version"])
			}
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if version != "" {
			return version, nil
		}
	}
	return "", errors.New("scheduler.log 中没有 serve 启动记录")
}

// doctorFreshness tells the caller how old origin/main may be: doctor never
// fetches, so origin/main is only as new as the last fetch.
type doctorFreshness struct {
	mainCommitted, fetched time.Time
}

func readFreshness(ctx context.Context, dir string) (doctorFreshness, error) {
	committed, err := runGit(ctx, dir, doctorCommandTimeout, "log", "-1", "--format=%cI", "origin/main")
	if err != nil {
		return doctorFreshness{}, errors.New(commandFailure("git log origin/main", err))
	}
	fresh := doctorFreshness{}
	if fresh.mainCommitted, err = time.Parse(time.RFC3339, committed); err != nil {
		return doctorFreshness{}, errors.New("origin/main 提交时间不可解析")
	}
	fetchHead, err := runGit(ctx, dir, doctorCommandTimeout, "rev-parse", "--git-path", "FETCH_HEAD")
	if err != nil {
		return doctorFreshness{}, errors.New(commandFailure("git rev-parse FETCH_HEAD", err))
	}
	if !filepath.IsAbs(fetchHead) {
		fetchHead = filepath.Join(dir, fetchHead)
	}
	if info, err := os.Stat(fetchHead); err == nil {
		fresh.fetched = info.ModTime()
	}
	return fresh, nil
}

func formatDoctorTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339)
}

func (p doctorProbe) checkVersion(ctx context.Context, state maintenance.State, stateErr error) healthCheck {
	running, err := p.runningVersion()
	if err != nil {
		return unprobed("读取运行版本失败: " + err.Error())
	}
	if p.factoryRoot == "" {
		return unprobed("driver.toml 未配置 factory_root")
	}
	factoryMain, err := runGit(ctx, p.factoryRoot, doctorCommandTimeout, "rev-parse", "origin/main")
	if err != nil {
		return unprobed(commandFailure("git rev-parse origin/main", err))
	}
	fresh, err := readFreshness(ctx, p.factoryRoot)
	if err != nil {
		return unprobed(err.Error())
	}
	details := map[string]string{"running": running, "factory_main": factoryMain, "factory_main_committed_at": formatDoctorTime(fresh.mainCommitted), "factory_fetched_at": formatDoctorTime(fresh.fetched)}
	if running != "" && strings.HasPrefix(factoryMain, running) {
		return healthCheck{Status: healthOK, Reason: "运行版本与 factory origin/main 一致", Details: details}
	}
	reason := fmt.Sprintf("运行版本 %s 与 factory origin/main %.7s 不一致", running, factoryMain)
	switch {
	case stateErr == nil && state.Mode != "running":
		return healthCheck{Status: healthWarn, Reason: reason + "，维护状态为 " + state.Mode + "，部署可能在进行", Details: details}
	case p.now.Sub(fresh.mainCommitted) < doctorDeployGrace:
		return healthCheck{Status: healthWarn, Reason: reason + "，origin/main 最新提交不到 60 分钟，可能尚未部署", Details: details}
	}
	return healthCheck{Status: healthFail, Reason: reason, Details: details}
}

func (p doctorProbe) checkMaintenance(state maintenance.State, err error) healthCheck {
	if err != nil {
		return unprobed("读取维护状态失败")
	}
	details := map[string]string{"mode": state.Mode, "updated_at": formatDoctorTime(state.UpdatedAt)}
	if state.Mode == "running" {
		return healthCheck{Status: healthOK, Reason: "维护状态为 running", Details: details}
	}
	reason := fmt.Sprintf("维护状态为 %s，不是 running，更新于 %s", state.Mode, formatDoctorTime(state.UpdatedAt))
	if p.now.Sub(state.UpdatedAt) <= doctorDeployGrace {
		return healthCheck{Status: healthWarn, Reason: reason + "，部署可能在进行", Details: details}
	}
	return healthCheck{Status: healthFail, Reason: reason + "，已超过 60 分钟", Details: details}
}

type doctorCheckout struct {
	Name            string `json:"name"`
	Path            string `json:"path"`
	Branch          string `json:"branch"`
	Clean           bool   `json:"clean"`
	Behind          int    `json:"behind"`
	MainCommittedAt string `json:"origin_main_committed_at"`
	FetchedAt       string `json:"fetched_at"`
	fetched         time.Time
}

func readCheckout(ctx context.Context, p projects.Project) (doctorCheckout, error) {
	row := doctorCheckout{Name: p.Name, Path: p.Path}
	branch, err := runGit(ctx, p.Path, doctorCommandTimeout, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return row, errors.New(commandFailure("git rev-parse HEAD", err))
	}
	status, err := runGit(ctx, p.Path, doctorCommandTimeout, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return row, errors.New(commandFailure("git status", err))
	}
	fresh, err := readFreshness(ctx, p.Path)
	if err != nil {
		return row, err
	}
	row.Branch, row.Clean = branch, status == ""
	row.MainCommittedAt, row.FetchedAt, row.fetched = formatDoctorTime(fresh.mainCommitted), formatDoctorTime(fresh.fetched), fresh.fetched
	if branch != "main" {
		return row, nil
	}
	behind, err := runGit(ctx, p.Path, doctorCommandTimeout, "rev-list", "--count", "HEAD..origin/main")
	if err != nil {
		return row, errors.New(commandFailure("git rev-list", err))
	}
	row.Behind, err = strconv.Atoi(behind)
	return row, err
}

// checkCheckouts only fails a checkout that stayed behind past a fetch older
// than doctorFetchStale; right after a merge, behind is the normal state.
func (p doctorProbe) checkCheckouts(ctx context.Context, eligible []projects.Project) healthCheck {
	rows, statuses, problems := []doctorCheckout{}, []string{healthOK}, []string{}
	for _, project := range eligible {
		row, err := readCheckout(ctx, project)
		if err != nil {
			return unprobed(project.Name + ": " + err.Error())
		}
		rows = append(rows, row)
		if row.Branch != "main" {
			statuses = append(statuses, healthWarn)
			problems = append(problems, fmt.Sprintf("%s 检出在 %s，不在 main", project.Name, row.Branch))
		}
		if row.Behind > 0 {
			status, note := healthWarn, "最后 fetch 不到 30 分钟"
			if row.fetched.IsZero() || p.now.Sub(row.fetched) > doctorFetchStale {
				status, note = healthFail, "最后 fetch 早于 30 分钟"
			}
			statuses = append(statuses, status)
			problems = append(problems, fmt.Sprintf("%s 落后 origin/main %d 个提交，%s", project.Name, row.Behind, note))
		}
		if !row.Clean {
			statuses = append(statuses, healthWarn)
			problems = append(problems, project.Name+" 有未提交改动")
		}
	}
	reason := "全部检出在 main、干净且不落后 origin/main"
	if len(problems) > 0 {
		reason = strings.Join(problems, "；")
	}
	return healthCheck{Status: worst(statuses...), Reason: reason, Details: map[string]any{"projects": rows}}
}

type doctorSyncPR struct {
	Project   string `json:"project"`
	Number    int    `json:"number"`
	Branch    string `json:"branch"`
	URL       string `json:"url"`
	Behind    int    `json:"behind"`
	CreatedAt string `json:"created_at"`
	created   time.Time
}

func (p doctorProbe) openSyncPRs(ctx context.Context, project projects.Project) ([]doctorSyncPR, error) {
	out, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"pr", "list", "--state", "open", "--json", "number,headRefName,url,createdAt", "--limit", "100"}, Dir: project.Path, Timeout: doctorCommandTimeout})
	if err != nil {
		return nil, errors.New(commandFailure("gh pr list", err))
	}
	var open []struct {
		Number      int       `json:"number"`
		HeadRefName string    `json:"headRefName"`
		URL         string    `json:"url"`
		CreatedAt   time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal(out, &open); err != nil {
		return nil, errors.New("gh pr list 输出不可解析")
	}
	var rows []doctorSyncPR
	for _, pr := range open {
		version, ok := strings.CutPrefix(pr.HeadRefName, coreSyncBranchPrefix)
		if !ok {
			continue
		}
		behind, err := runGit(ctx, p.factoryRoot, doctorCommandTimeout, "rev-list", "--count", version+"..origin/main")
		if err != nil {
			return nil, errors.New(commandFailure("git rev-list "+version, err))
		}
		row := doctorSyncPR{Project: project.Name, Number: pr.Number, Branch: pr.HeadRefName, URL: pr.URL, CreatedAt: formatDoctorTime(pr.CreatedAt), created: pr.CreatedAt}
		if row.Behind, err = strconv.Atoi(behind); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (p doctorProbe) checkSyncPRs(ctx context.Context, eligible []projects.Project) healthCheck {
	if p.factoryRoot == "" {
		return unprobed("driver.toml 未配置 factory_root")
	}
	rows, statuses, problems := []doctorSyncPR{}, []string{healthOK}, []string{}
	for _, project := range eligible {
		open, err := p.openSyncPRs(ctx, project)
		if err != nil {
			return unprobed(project.Name + ": " + err.Error())
		}
		for _, pr := range open {
			rows = append(rows, pr)
			status, note := healthWarn, "待合并"
			if p.now.Sub(pr.created) > doctorSyncPRStale {
				status, note = healthFail, "开启超过 24 小时未合并"
			}
			statuses = append(statuses, status)
			problems = append(problems, fmt.Sprintf("%s #%d %s，落后 factory %d 个提交", pr.Project, pr.Number, note, pr.Behind))
		}
	}
	reason := "没有未合并的 core 同步 PR"
	if len(problems) > 0 {
		reason = strings.Join(problems, "；")
	}
	return healthCheck{Status: worst(statuses...), Reason: reason, Details: map[string]any{"prs": rows}}
}

type doctorLedgerTicket struct {
	Project    string `json:"project"`
	Identifier string `json:"identifier"`
	Comments   int    `json:"comments"`
	Empty      int    `json:"empty"`
	MaxRunes   int    `json:"max_runes"`
	Headroom   int    `json:"headroom"`
}

func (p doctorProbe) ledgerTickets(ctx context.Context, project projects.Project) ([]doctorLedgerTicket, error) {
	ledger, err := p.ledger(ctx, project)
	if err != nil {
		return nil, errors.New("无法连接票据主账本")
	}
	tasks, err := ledger.List(ctx)
	if err != nil {
		return nil, errors.New("读取票据失败")
	}
	var rows []doctorLedgerTicket
	for _, t := range tasks {
		if t.State.ID != "started" {
			continue
		}
		comments, err := ledger.ListComments(ctx, t.ID)
		if err != nil {
			return nil, errors.New("读取 " + t.Identifier + " 评论失败")
		}
		row := doctorLedgerTicket{Project: project.Name, Identifier: t.Identifier, Comments: len(comments)}
		for _, c := range comments {
			if strings.TrimSpace(c.Body) == "" {
				row.Empty++
			}
			row.MaxRunes = max(row.MaxRunes, utf8.RuneCountInString(c.Body))
		}
		row.Headroom = issues.FeishuBodyRunes - row.MaxRunes
		rows = append(rows, row)
	}
	return rows, nil
}

func (p doctorProbe) checkLedger(ctx context.Context, eligible []projects.Project) healthCheck {
	rows, statuses, problems := []doctorLedgerTicket{}, []string{healthOK}, []string{}
	for _, project := range eligible {
		tickets, err := p.ledgerTickets(ctx, project)
		if err != nil {
			return unprobed(project.Name + ": " + err.Error())
		}
		for _, t := range tickets {
			rows = append(rows, t)
			if t.Headroom < issues.FeishuBodyRunes/10 {
				statuses = append(statuses, healthFail)
				problems = append(problems, fmt.Sprintf("%s 最长正文离上限只剩 %d 字", t.Identifier, t.Headroom))
			}
			if t.Empty > 0 {
				statuses = append(statuses, healthWarn)
				problems = append(problems, fmt.Sprintf("%s 有 %d 条空正文评论（历史遗留）", t.Identifier, t.Empty))
			}
		}
	}
	reason := "在途票没有空正文，最长正文离上限充足"
	if len(problems) > 0 {
		reason = strings.Join(problems, "；")
	}
	return healthCheck{Status: worst(statuses...), Reason: reason, Details: map[string]any{"body_limit": issues.FeishuBodyRunes, "tickets": rows}}
}

type doctorLaunch struct {
	Identifier string `json:"identifier"`
	Starts     int    `json:"starts"`
	Failures   int    `json:"failures"`
}

func readLaunch(path string, cutoff time.Time) (doctorLaunch, error) {
	identifier := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "launch-"), ".log")
	row := doctorLaunch{Identifier: identifier}
	err := eachLogRecord(path, func(fields map[string]json.RawMessage) {
		when, err := time.Parse(time.RFC3339Nano, logsString(fields["time"]))
		if err != nil || when.Before(cutoff) {
			return
		}
		switch logsString(fields["event"]) {
		case "start":
			row.Starts++
		case "exit":
			code, reason := string(fields["exit_code"]), logsString(fields["err"])
			deferred := slices.ContainsFunc(doctorDeferredExits, func(marker string) bool { return strings.Contains(reason, marker) })
			if code != "" && code != "0" && !deferred {
				row.Failures++
			}
		}
	})
	return row, err
}

func (p doctorProbe) checkLaunches() healthCheck {
	paths, err := filepath.Glob(filepath.Join(p.stateRoot, "launch-*.log"))
	if err != nil {
		return unprobed("列出 launch 日志失败")
	}
	cutoff := p.now.Add(-doctorLaunchWindow)
	rows, statuses, problems := []doctorLaunch{}, []string{healthOK}, []string{}
	for _, path := range paths {
		if info, err := os.Stat(path); err != nil || info.ModTime().Before(cutoff) {
			continue
		}
		row, err := readLaunch(path, cutoff)
		if err != nil {
			return unprobed("读取 " + filepath.Base(path) + " 失败")
		}
		if row.Starts == 0 && row.Failures == 0 {
			continue
		}
		rows = append(rows, row)
		switch {
		case row.Failures >= doctorLaunchRepeats:
			statuses = append(statuses, healthFail)
			problems = append(problems, fmt.Sprintf("%s 近 30 分钟失败 %d 次", row.Identifier, row.Failures))
		case row.Starts >= doctorLaunchRepeats || row.Failures > 0:
			statuses = append(statuses, healthWarn)
			problems = append(problems, fmt.Sprintf("%s 近 30 分钟启动 %d 次、失败 %d 次", row.Identifier, row.Starts, row.Failures))
		}
	}
	reason := "近 30 分钟没有同票重复启动或失败"
	if len(problems) > 0 {
		reason = strings.Join(problems, "；")
	}
	return healthCheck{Status: worst(statuses...), Reason: reason, Details: map[string]any{"window_minutes": int(doctorLaunchWindow.Minutes()), "tickets": rows}}
}

// checkCredentials reports variable names only. Feishu authentication is
// proven by the ledger check reading the ledger with those credentials.
func (p doctorProbe) checkCredentials(ctx context.Context, env map[string]string, envErr error) healthCheck {
	if envErr != nil {
		return unprobed(envErr.Error())
	}
	present, missing := map[string]bool{}, []string{}
	for _, name := range doctorCredentialNames {
		present[name] = env[name] != ""
		if !present[name] {
			missing = append(missing, name)
		}
	}
	authOut, authErr := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"auth", "status"}, Timeout: doctorCommandTimeout})
	unreadable := authErr != nil && ghTokenInvalid(authOut, authErr) && p.githubReachable(ctx) == nil
	details := map[string]any{"variables": present, "gh_auth": authErr == nil}
	statuses, problems := []string{healthOK}, []string{}
	if len(missing) > 0 {
		statuses = append(statuses, healthFail)
		problems = append(problems, "服务环境缺少 "+strings.Join(missing, ", "))
	}
	switch {
	case unreadable:
		statuses = append(statuses, healthFail)
		problems = append(problems, "GitHub 凭证不可读（keyring 可能未解锁），修法：登录桌面会话解锁 login keyring")
	case authErr != nil:
		statuses = append(statuses, healthUnknown)
		problems = append(problems, commandFailure("gh auth status", authErr)+"，无法区分未登录与网络故障")
	}
	if len(problems) == 0 {
		return healthCheck{Status: healthOK, Reason: "凭证变量齐全，gh 已认证", Details: details}
	}
	return healthCheck{Status: worst(statuses...), Reason: strings.Join(problems, "；"), Details: details, unprobed: authErr != nil && !unreadable}
}

// ghTokenInvalid matches gh's "The token in <source> is invalid", which gh
// also prints when the keyring holding the token is locked (OF-138).
func ghTokenInvalid(stdout []byte, err error) bool {
	text := commandText(stdout, err)
	return strings.Contains(text, "The token in ") && strings.Contains(text, " is invalid")
}

func commandText(stdout []byte, err error) string {
	text := string(stdout)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		text += string(exit.Stderr)
	}
	return text
}

// httpReachable treats any HTTP response, a 403 rate limit included, as
// reachable: the question is the network path, not authentication.
func httpReachable(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (p doctorProbe) checkOrcaRuntime() healthCheck {
	view := status.Read(p.now, 0)
	detail := map[string]any{"failures": view.OrcaRuntime.Failures, "snapshot_state": view.State}
	if view.OrcaRuntime.Paused {
		fix := "重启 orca-serve 服务，并查看其日志里的 profile 写入超时；调度器探测成功后自动恢复。"
		detail["recovery"] = fix
		return healthCheck{Status: healthFail, Reason: "Orca terminal create 连续运行时失败，已暂停认领与派工", Details: detail, Fix: fix}
	}
	return healthCheck{Status: healthOK, Reason: "状态快照未记录 Orca terminal create 连续故障", Details: detail}
}

func (p doctorProbe) checkHostMemory() healthCheck {
	view := status.Read(p.now, 0)
	memory := view.HostMemory
	detail := map[string]any{"available": memory.Reading.Available, "some_avg60": memory.Reading.SomeAvg60, "snapshot_state": view.State}
	if memory.Error != "" {
		detail["error"] = memory.Error
		return healthCheck{Status: healthWarn, Reason: "读不到主机内存读数，调度器不按内存暂停：" + memory.Error, Details: detail}
	}
	if memory.Paused {
		fix := "用 memory.peak 与 systemd-cgls 找占用大户；MemAvailable 不低于 10 GiB 且 some avg60 低于 5 后自动恢复。"
		detail["since"] = memory.Since
		detail["recovery"] = fix
		return healthCheck{Status: healthWarn, Reason: "主机内存吃紧，已暂停认领与新建终端", Details: detail, Fix: fix}
	}
	return healthCheck{Status: healthOK, Reason: "状态快照未记录主机内存暂停", Details: detail}
}

// doctorModelStatus reports configured identities without changing dispatch selection.
type doctorModelStatus struct {
	Repository string             `json:"repository"`
	Role       string             `json:"role"`
	Status     modelhealth.Status `json:"status"`
}

type doctorModelSettings struct {
	proxyURL     string
	proxyErr     error
	intakePi     string
	intakeClaude string
	// intakeClaudeErr is why the intake gateway would refuse AGENT_INTAKE_CLAUDE.
	intakeClaudeErr error
	driverEnvErr    error
}

type doctorModelRole struct {
	name     string
	ref      modelhealth.Ref
	endpoint modelhealth.Endpoint
	reason   string
}

// probeKey resolves an unset Binary on PATH, so the PATH agent and its absolute
// path share one probe.
func (r doctorModelRole) probeKey() string {
	binary := r.endpoint.Binary
	if binary == "" {
		binary, _ = exec.LookPath(r.ref.Agent)
	}
	return strings.Join([]string{r.ref.Key(), binary, r.endpoint.BaseURL, strconv.FormatBool(r.endpoint.Isolated)}, "\x00")
}

func invalidModelRole(name, raw string) doctorModelRole {
	agent, model, effort := splitTier(raw)
	return doctorModelRole{name: name, ref: modelhealth.Ref{Agent: agent, Model: model, Effort: effort}, reason: "config_invalid"}
}

func workerModelRoles(env map[string]string) []doctorModelRole {
	chain, err := config.TierChain(env["AGENT_TIER_CHAIN"])
	if err != nil {
		return []doctorModelRole{invalidModelRole("worker_default", "")}
	}
	if env["AGENT_TIER_ESCALATE"] != "1" {
		chain = []string{"default"}
	}
	start := slices.Index(chain, "default")
	if start < 0 {
		return []doctorModelRole{invalidModelRole("worker_default", "")}
	}

	end := min(len(chain), start+1+machine.DefaultLimits.Corrections)
	roles := make([]doctorModelRole, 0, end-start)
	for _, tier := range chain[start:end] {
		raw := env["AGENT_TIER_"+strings.ToUpper(tier)]
		name := "worker_" + tier
		ref, err := modelhealth.Parse(raw)
		switch {
		case tier == "default" && raw == "":
			roles = append(roles, doctorModelRole{name: name, reason: "not_configured"})
		case err != nil:
			roles = append(roles, invalidModelRole(name, raw))
		default:
			if ref.Model == "" {
				ref.Effort = ""
			}
			roles = append(roles, doctorModelRole{name: name, ref: ref})
		}
	}
	// The executor validates the entire chain at startup when escalation is enabled.
	for _, tier := range slices.Concat(chain[:start], chain[end:]) {
		raw := env["AGENT_TIER_"+strings.ToUpper(tier)]
		if _, err := modelhealth.Parse(raw); err != nil {
			roles = append(roles, invalidModelRole("worker_"+tier, raw))
		}
	}
	return roles
}

func supportedModelAgent(agent string) bool {
	return agent == "codex" || agent == "claude"
}

func (s doctorModelSettings) intakeRole(env map[string]string, claudeTier, pi string) doctorModelRole {
	if claudeTier == "" {
		return doctorModelRole{name: "intake", reason: "not_configured"}
	}
	model, effort, err := tierParts(env)
	if err != nil || s.driverEnvErr != nil || s.intakeClaudeErr != nil {
		return invalidModelRole("intake", claudeTier)
	}
	if pi == "" {
		return doctorModelRole{name: "intake", ref: modelhealth.Ref{Agent: "claude", Model: model, Effort: effort},
			endpoint: modelhealth.Endpoint{Binary: s.intakeClaude, Isolated: true}}
	}
	provider, model, thinking, err := intake.ParsePiTier(pi)
	if err != nil {
		return invalidModelRole("intake", pi)
	}
	return doctorModelRole{name: "intake", ref: modelhealth.Ref{Agent: "pi", Model: provider + "/" + model, Effort: thinking}}
}

func (s doctorModelSettings) modelRoles(project projects.Project) []doctorModelRole {
	env := project.Repository.Env
	roles := workerModelRoles(env)
	hasReviewer, hasSecondReviewer := false, false
	var reviewer doctorModelRole
	var intakeTier string
	for _, role := range repositoryRoles(env, project.Repository.Sources) {
		ref := modelhealth.Ref{Agent: role.Agent, Model: role.Model, Effort: role.Effort}
		modelRole := doctorModelRole{name: role.Role, ref: ref}
		if strings.HasSuffix(role.Key, "_CMD") || strings.HasSuffix(role.Key, "_CMD_SMALL") {
			command := strings.TrimSpace(role.Raw)
			if command == "" {
				// The executor trims the command and refuses an empty one.
				continue
			}
			// A legacy command is arbitrary shell: /bin/sh decides its program, model
			// and endpoint. Only a command the structured value generates verbatim,
			// with words the shell leaves unchanged, is probed; any other is neither
			// probed nor reported word by word.
			generated, err := config.GenerateReviewerCommand(ref.String())
			literal := shellLiteral.MatchString(ref.Agent) && shellLiteral.MatchString(ref.Model) && shellLiteral.MatchString(ref.Effort)
			if err != nil || generated != command || !literal {
				ref = modelhealth.Ref{}
				modelRole = doctorModelRole{name: role.Role, reason: "legacy_command"}
			}
		}
		switch role.Role {
		case "reviewer", "reviewer_small":
			if role.Role == "reviewer" {
				hasReviewer, reviewer = true, modelRole
			}
			if ref.Agent == "claude" {
				modelRole.endpoint.BaseURL = s.proxyURL
				if s.proxyErr != nil {
					modelRole.reason = "model_proxy_invalid"
				}
			}
		case "second_reviewer":
			// Cross review runs both reviewers with only HOME and PATH (scheduler/crossreview.go).
			modelRole.endpoint.Isolated = true
			hasSecondReviewer = true
		case "resolver":
			if !supportedModelAgent(ref.Agent) || strings.Count(role.Raw, ":") > 2 {
				modelRole = invalidModelRole(role.Role, role.Raw)
			}
		case "intake":
			intakeTier = role.Raw
			continue
		case "worker_default", "worker_strong":
			continue
		}
		roles = append(roles, modelRole)
	}
	if !hasReviewer {
		roles = append(roles, doctorModelRole{name: "reviewer", reason: "not_configured"})
	}
	if hasReviewer && hasSecondReviewer {
		cross := reviewer
		cross.name, cross.endpoint = "cross_reviewer", modelhealth.Endpoint{Binary: reviewer.endpoint.Binary, Isolated: true}
		roles = append(roles, cross)
	}
	if raw := env["AGENT_ROLE_RESOLVER_TOP"]; raw != "" {
		ref, err := modelhealth.Parse(raw)
		role := doctorModelRole{name: "resolver_top", ref: ref}
		if err != nil || !supportedModelAgent(ref.Agent) || ref.Model == "" || ref.Effort == "" {
			role = invalidModelRole("resolver_top", raw)
		}
		roles = append(roles, role)
	}
	pi := strings.TrimSpace(env["AGENT_INTAKE_PI"])
	if pi == "" {
		pi = s.intakePi
	}
	roles = append(roles, s.intakeRole(env, intakeTier, pi))
	for i, role := range roles {
		if role.reason == "" && !supportedModelAgent(role.ref.Agent) {
			roles[i].reason = "unsupported_agent"
		}
	}
	return roles
}

// unloadedRepositories keeps the repositories discovery rejected because their
// pipeline check or profile failed. With a readable ledger only those whose
// last row is enabled are kept, as projects.Discover reads it; a nil ledger
// means no profile could read it.
func unloadedRepositories(rejected []projects.Rejection, ledger []issues.LedgerProject) []projects.Rejection {
	enabled := map[string]bool{}
	for _, row := range ledger {
		enabled[row.Remote] = row.Enabled
	}
	return slices.DeleteFunc(slices.Clone(rejected), func(r projects.Rejection) bool {
		return r.Reason != projects.ReasonCheckFailed || ledger != nil && !enabled[r.Remote]
	})
}

// shellLiteral matches a word /bin/sh passes through unchanged.
var shellLiteral = regexp.MustCompile(`^[A-Za-z0-9._/@+-]*$`)

// checkModels also lists each enabled repository whose profile could not be
// loaded, since the driver runs none of its roles.
func (p doctorProbe) checkModels(ctx context.Context, eligible []projects.Project, unloaded []projects.Rejection) healthCheck {
	settings := p.modelSettings()
	registry, err := modelhealth.Open(p.stateRoot)
	if err != nil {
		return unprobed("模型可用性登记无法读取")
	}
	probeCtx, cancel := context.WithTimeout(ctx, modelhealth.ProbeTimeout)
	defer cancel()
	var probes sync.WaitGroup
	pending := map[string]*modelhealth.Status{}
	projectRoles := make([][]doctorModelRole, len(eligible))
	// Keys are computed once: probeKey reads PATH, which may change mid-run.
	projectKeys := make([][]string, len(eligible))
	for i, project := range eligible {
		roles := settings.modelRoles(project)
		projectRoles[i] = roles
		projectKeys[i] = make([]string, len(roles))
		for j, role := range roles {
			if role.reason != "" {
				continue
			}
			key := role.probeKey()
			projectKeys[i][j] = key
			if pending[key] != nil {
				continue
			}
			result := new(modelhealth.Status)
			pending[key] = result
			probes.Go(func() { *result = p.modelProbe(probeCtx, role.ref, project.Path, role.endpoint) })
		}
	}
	probes.Wait()

	rows := []doctorModelStatus{}
	var unavailable, unknown, exhausted, invalid bool
	for i, roles := range projectRoles {
		retryable := map[string]bool{}
		for j, role := range roles {
			state := modelhealth.Status{Ref: role.ref, State: modelhealth.Unknown, Reason: role.reason, CheckedAt: p.now}
			if role.reason == "" {
				state = *pending[projectKeys[i][j]]
				state.Ref = role.ref
				// An inconclusive probe keeps the model's unexpired cooldown.
				if recorded := registry.Status(role.ref, p.now); state.State == modelhealth.Unknown && recorded.State == modelhealth.Unavailable {
					state = recorded
				}
			}
			rows = append(rows, doctorModelStatus{Repository: eligible[i].Name, Role: role.name, Status: state})
			unavailable = unavailable || state.State == modelhealth.Unavailable
			unknown = unknown || state.State == modelhealth.Unknown
			switch role.reason {
			case "config_invalid":
				invalid = true
			case "not_configured":
				exhausted = true
			default:
				group := role.name
				if strings.HasPrefix(group, "worker_") {
					group = "worker"
				}
				retryable[group] = retryable[group] || state.State != modelhealth.Unavailable
			}
		}
		for _, canRetry := range retryable {
			exhausted = exhausted || !canRetry
		}
	}
	for _, r := range unloaded {
		rows = append(rows, doctorModelStatus{Repository: r.Remote, Role: "profile", Status: modelhealth.Status{State: modelhealth.Unknown, Reason: "config_invalid", CheckedAt: p.now}})
		unknown, invalid = true, true
	}
	// Recording after the rows keeps watch and watch --dry-run on the same verdict.
	var recordErr error
	if p.recordModels {
		recordErr = recordModelProbes(registry, pending)
	}
	check := healthCheck{Status: healthOK, Reason: "各角色均有可重试候选", Details: rows, unprobed: unknown}
	switch {
	case settings.proxyErr != nil:
		check.Status, check.Reason = healthFail, "模型代理配置无效，执行器无法启动"
	case invalid:
		check.Status, check.Reason = healthFail, "仓库角色的模型档位配置无效"
	case exhausted:
		check.Status, check.Reason = healthFail, "缺少必需角色配置，或有角色的全部模型候选仍处于不可用期"
	case unavailable:
		check.Status, check.Reason = healthWarn, "有模型不可用，角色仍有可重试候选"
	case unknown:
		check.Status, check.Reason = healthUnknown, "有模型未能确认可用性"
	}
	if recordErr != nil {
		check.unprobed = true
		check.Reason += "；模型可用性登记无法写入"
		if check.Status == healthOK {
			check.Status = healthUnknown
		}
	}
	return check
}

func recordModelProbes(registry *modelhealth.Registry, probes map[string]*modelhealth.Status) error {
	selected := map[string]*modelhealth.Status{}
	// An inconclusive endpoint outranks ok, so one endpoint's success cannot end
	// a cooldown another endpoint of the same model has not confirmed over.
	priority := map[string]int{modelhealth.OK: 0, modelhealth.Unknown: 1, modelhealth.Unavailable: 2}
	// A stable endpoint order breaks ties between statuses of equal priority.
	for _, key := range slices.Sorted(maps.Keys(probes)) {
		result := probes[key]
		model := result.Ref.Key()
		// Among unavailable endpoints the latest Until keeps the model out of selection longest.
		if prev := selected[model]; prev == nil || priority[result.State] > priority[prev.State] ||
			result.State == modelhealth.Unavailable && prev.State == modelhealth.Unavailable && result.Until.After(prev.Until) {
			selected[model] = result
		}
	}
	var err error
	for _, result := range selected {
		err = errors.Join(err, registry.Record(*result))
	}
	return err
}
