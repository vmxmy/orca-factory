package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func init() { Register("doctor", runDoctor) }

const (
	healthOK      = "ok"
	healthWarn    = "warn"
	healthUnknown = "unknown"
	healthFail    = "fail"

	doctorLaunchWindow   = 30 * time.Minute
	doctorLaunchRepeats  = 3
	doctorFetchStale     = 30 * time.Minute
	doctorDeployGrace    = 60 * time.Minute
	doctorSyncPRStale    = 24 * time.Hour
	doctorServiceUnit    = "orca-factory-driver.service"
	doctorCommandTimeout = 30 * time.Second
)

// doctorChecks fixes the text output order; JSON keys are the same names.
var doctorChecks = []string{"service_path", "version", "maintenance", "checkouts", "sync_prs", "ledger", "launches", "credentials"}

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
	now             time.Time
	stateRoot       string
	factoryRoot     string
	serviceEnv      func(context.Context) (map[string]string, error)
	projects        func(context.Context) ([]projects.Project, error)
	ledger          func(context.Context, projects.Project) (doctorLedger, error)
	githubReachable func(context.Context) error
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
	return doctorProbe{
		now:         time.Now(),
		stateRoot:   logStateRoot(),
		factoryRoot: cfg.FactoryRoot,
		serviceEnv:  serviceEnviron,
		projects: func(ctx context.Context) ([]projects.Project, error) {
			result, _, err := discoverProjects(ctx, cfg, &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout})
			return result.Eligible, err
		},
		ledger: func(ctx context.Context, p projects.Project) (doctorLedger, error) {
			return issues.New(ctx, p.Repository.Env, cfg.Tuning)
		},
		githubReachable: githubReachable,
	}, nil
}

func (p doctorProbe) main(ctx context.Context, args []string, out io.Writer) int {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) != 0 && !asJSON {
		fmt.Fprintln(os.Stderr, "Usage: orca-factory-driver doctor [--json]")
		return 2
	}
	report := p.run(ctx)
	if err := writeHealth(out, report, asJSON); err != nil {
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
	eligible, projectsErr := p.projects(ctx)
	state, stateErr := maintenance.Read()
	checks := map[string]healthCheck{
		"service_path": checkServicePath(env, envErr),
		"version":      p.checkVersion(ctx, state, stateErr),
		"maintenance":  p.checkMaintenance(state, stateErr),
		"credentials":  p.checkCredentials(ctx, env, envErr),
		"launches":     p.checkLaunches(),
	}
	if projectsErr != nil {
		for _, name := range []string{"checkouts", "sync_prs", "ledger"} {
			checks[name] = unprobed("项目发现失败，无法列出已接管项目")
		}
	} else {
		checks["checkouts"] = p.checkCheckouts(ctx, eligible)
		checks["sync_prs"] = p.checkSyncPRs(ctx, eligible)
		checks["ledger"] = p.checkLedger(ctx, eligible)
	}
	report := healthReport{Checks: checks, Errors: []string{}}
	for _, name := range doctorChecks {
		if checks[name].unprobed {
			report.Errors = append(report.Errors, name+": "+checks[name].Reason)
		}
	}
	return report
}

func writeHealth(out io.Writer, report healthReport, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-13s %-8s %s\n", "CHECK", "STATUS", "REASON")
	for _, name := range doctorChecks {
		fmt.Fprintf(&b, "%-13s %-8s %s\n", name, report.Checks[name].Status, report.Checks[name].Reason)
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
	text := string(stdout)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		text += string(exit.Stderr)
	}
	return strings.Contains(text, "The token in ") && strings.Contains(text, " is invalid")
}

// githubReachable treats any HTTP response, a 403 rate limit included, as
// reachable: the question is the network path, not authentication.
func githubReachable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
