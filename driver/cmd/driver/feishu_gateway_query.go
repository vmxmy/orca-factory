package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

// querySource is one resolved project's read-only evidence wiring for the
// intake query loop (OF-427). repo is the local checkout path the scheduler
// status snapshot and shared scheduler records name; remote is its canonical
// host/owner/repo identity; ghRepo is AGENT_GH_REPO, the repository field of
// shared merge-lane records; logKey is the executor/resolver log directory
// component, which the driver derives from the repository name alone. Every
// query reads only through these, so two projects sharing a ticket number
// never mix.
type querySource struct {
	store  issues.Store
	repo   string
	remote string
	ghRepo string
	logKey string
}

// evidenceReaders are the host reads behind the queries; tests replace them.
type evidenceReaders struct {
	status      func(now time.Time, staleAfter time.Duration) status.View
	maintenance func() (maintenance.Observation, error)
	profile     func(repo string) (config.Repository, time.Time, error)
	logRoot     string
	now         func() time.Time
}

func defaultEvidenceReaders() evidenceReaders {
	return evidenceReaders{status: status.Read, maintenance: maintenance.Status, profile: readProfile, logRoot: logStateRoot(), now: time.Now}
}

// readProfile rereads the repository's agent-graph.env at query time, so a
// config query never serves the copy cached when the project was resolved.
func readProfile(repo string) (config.Repository, time.Time, error) {
	info, err := os.Stat(filepath.Join(repo, "scripts", "agent-gates", "agent-graph.env"))
	if err != nil {
		return config.Repository{}, time.Time{}, err
	}
	repository, err := config.LoadRepository(repo)
	if err != nil {
		return config.Repository{}, time.Time{}, err
	}
	return repository, info.ModTime(), nil
}

func newQuerySource(store issues.Store, repo, remote string, env map[string]string) querySource {
	return querySource{store: store, repo: filepath.Clean(repo), remote: remote, ghRepo: strings.TrimSpace(env["AGENT_GH_REPO"]), logKey: logDirKey(repo, env)}
}

// logDirKey is the directory driver exec and driver resolve log under:
// AGENT_GH_REPO's repository name, else the checkout's base name.
func logDirKey(repo string, env map[string]string) string {
	if _, name, ok := strings.Cut(strings.TrimSpace(env["AGENT_GH_REPO"]), "/"); ok && logComponent.MatchString(name) {
		return name
	}
	return filepath.Base(repo)
}

// logDirOwners maps each log directory to the repository identities that can
// write it: every ledger project row, enabled or not, and every local
// repository discovery saw. Executor and resolver records carry only the
// repository name, so a directory with a second identity cannot be attributed.
func logDirOwners(result projects.Result, rows []issues.LedgerProject) map[string]map[string]bool {
	owners := map[string]map[string]bool{}
	claim := func(key, identity string) {
		if owners[key] == nil {
			owners[key] = map[string]bool{}
		}
		owners[key][identity] = true
	}
	for _, row := range rows {
		claim(row.Remote[strings.LastIndex(row.Remote, "/")+1:], row.Remote)
	}
	for _, p := range result.Eligible {
		claim(logDirKey(p.Path, p.Repository.Env), p.Remote)
	}
	for _, rejected := range result.Rejected {
		var env map[string]string
		if rejected.Repository != nil {
			env = rejected.Repository.Env
		}
		identity := rejected.Remote
		if identity == "" {
			identity = rejected.Path
		}
		claim(logDirKey(rejected.Path, env), identity)
	}
	return owners
}

// query is the intake.Querier: it resolves the session's bound project to
// its own source and never accepts a repository from the request.
func (r *projectRuntime) query(ctx context.Context, p intake.Project, q intake.QueryRequest) intake.Evidence {
	r.mu.Lock()
	source, ok := r.sources[p.Prefix]
	readers := r.readers
	sharedLogDir := false
	for identity := range r.logOwners[source.logKey] {
		sharedLogDir = sharedLogDir || identity != source.remote
	}
	r.mu.Unlock()
	now := readers.now()
	if !ok {
		return intake.Evidence{Source: "gateway", CollectedAt: now, Outcome: intake.OutcomeFailed, Error: "会话绑定的项目没有本机查询来源"}
	}
	switch q.Op {
	case intake.QueryStatus:
		return statusEvidence(readers, source, now)
	case intake.QueryConfig:
		return configEvidence(readers, source, now)
	case intake.QueryMaintenance:
		return maintenanceEvidence(readers, now)
	case intake.QueryTicket:
		return ticketEvidence(ctx, source, q.Ticket, now)
	case intake.QueryLogs:
		return logsEvidence(readers, source, sharedLogDir, q.Ticket, q.Window, now)
	}
	return intake.Evidence{Source: "gateway", CollectedAt: now, Outcome: intake.OutcomeRejected, Error: "未知查询操作"}
}

func encodeEvidence(e intake.Evidence, data any) intake.Evidence {
	b, err := json.Marshal(data)
	if err != nil {
		e.Outcome, e.Complete, e.Error = intake.OutcomeFailed, false, "证据编码失败"
		return e
	}
	e.Data = string(b)
	return e
}

type statusTicketEvidence struct {
	Identifier    string             `json:"identifier"`
	Title         string             `json:"title,omitempty"`
	Owner         string             `json:"owner"`
	Phase         string             `json:"phase"`
	Stage         string             `json:"stage,omitempty"`
	BlockedReason string             `json:"blockedReason,omitempty"`
	Held          bool               `json:"held,omitempty"`
	QueuePosition int                `json:"queuePosition,omitempty"`
	Deployment    *status.Deployment `json:"deployment,omitempty"`
	Since         time.Time          `json:"since,omitzero"`
}

// projectStatus keeps only this project's rows of a status view.
func projectStatus(view status.View, repo string) []statusTicketEvidence {
	out := []statusTicketEvidence{}
	for _, t := range view.Tickets {
		if filepath.Clean(t.Repo) != repo {
			continue
		}
		out = append(out, statusTicketEvidence{Identifier: t.Identifier, Title: t.Title, Owner: t.Owner, Phase: t.Phase, Stage: t.Stage,
			BlockedReason: t.BlockedReason, Held: t.Held, QueuePosition: t.QueuePosition, Deployment: t.Deployment, Since: t.Since})
	}
	return out
}

// statusEvidence reports the scheduler's pushed snapshot. Its age comes from
// the snapshot's own generatedAt, so rereading the file never freshens it.
func statusEvidence(readers evidenceReaders, source querySource, now time.Time) intake.Evidence {
	e := intake.Evidence{Source: "driver status", CollectedAt: now}
	view := readers.status(now, intake.StatusStaleAfter)
	e.SourceGeneratedAt = view.GeneratedAt
	if view.State == "unknown" || view.GeneratedAt.IsZero() {
		e.Outcome, e.Error = intake.OutcomeFailed, "没有可用的调度快照（driver serve 可能未运行）；归属与存活均未知"
		return e
	}
	e.Stale = view.State != "ok" || now.Sub(view.GeneratedAt) > intake.StatusStaleAfter
	tickets := projectStatus(view, source.repo)
	e.Outcome, e.Complete = intake.OutcomeOK, true
	if len(tickets) == 0 {
		e.Outcome = intake.OutcomeEmpty
	}
	return encodeEvidence(e, map[string]any{"state": view.State, "nextScanAt": view.NextScanAt, "tickets": tickets,
		"note": "快照只反映调度器观察到的归属与阶段，不证明 worker 进程存活"})
}

// configKeys is the only repository configuration the Agent sees: candidate
// limits and lane rules, never commands, credentials or the whole env.
var configKeys = []string{"AGENT_MAX_IN_FLIGHT", "AGENT_STALE_HOURS", "AGENT_REVIEWER_SMALL_MAX_ESTIMATE", "AGENT_TIER_ESCALATE", "AGENT_LANE2_AUTOMERGE", "AGENT_LANE3_GLOBS", "AGENT_ISSUE_BACKEND"}

func configEvidence(readers evidenceReaders, source querySource, now time.Time) intake.Evidence {
	e := intake.Evidence{Source: "repository agent-graph.env + maintenance", CollectedAt: now, Outcome: intake.OutcomeOK, Complete: true}
	repository, modified, err := readers.profile(source.repo)
	if err != nil {
		e.Outcome, e.Complete, e.Error = intake.OutcomeFailed, false, "仓库配置读取失败"
		return e
	}
	e.SourceGeneratedAt = modified
	limits := map[string]string{}
	for _, item := range repositoryRouting(repository.Env) {
		if !slices.Contains(configKeys, item.Key) {
			continue
		}
		value := "（未设置）"
		if item.Value != nil && *item.Value != "" {
			value = *item.Value
		}
		limits[item.Key] = value + "（" + item.Source + "）"
	}
	type tier struct{ Role, Agent, Model, Effort string }
	tiers := []tier{}
	for _, role := range repositoryRoles(repository.Env, repository.Sources) {
		tiers = append(tiers, tier{role.Role, role.Agent, role.Model, role.Effort})
	}
	mode := "unknown"
	if state, err := readers.maintenance(); err != nil {
		e.Outcome, e.Complete, e.Error = intake.OutcomePartial, false, "维护状态读取失败"
	} else {
		mode = state.Mode
	}
	return encodeEvidence(e, map[string]any{"tiers": tiers, "limits": limits, "maintenanceMode": mode,
		"note":           "配置为查询时 agent-graph.env 的内容，sourceGeneratedAt 为文件修改时间；不证明调度器已采用该配置",
		"candidateRules": "候选标签与状态：有 ready-for-agent、无 auto-impl 与 auto-failed，状态为 Todo 或 Backlog；归属、依赖、held 与证据完整性在领取时另行检查；auto-failed 票只由处置 Agent 处理"})
}

func maintenanceEvidence(readers evidenceReaders, now time.Time) intake.Evidence {
	e := intake.Evidence{Source: "driver maintenance status", CollectedAt: now}
	state, err := readers.maintenance()
	if err != nil {
		e.Outcome, e.Error = intake.OutcomeFailed, "维护状态读取失败"
		return e
	}
	e.SourceGeneratedAt = state.UpdatedAt
	e.Outcome, e.Complete = intake.OutcomeOK, true
	return encodeEvidence(e, state)
}

type ticketDetail struct {
	Identifier       string   `json:"identifier"`
	Title            string   `json:"title"`
	State            string   `json:"state"`
	Labels           []string `json:"labels"`
	Blockers         []string `json:"blockers"`
	Related          []string `json:"related"`
	Managed          bool     `json:"managed"`
	Held             bool     `json:"held"`
	URL              string   `json:"url"`
	Owner            string   `json:"owner"`
	Phase            string   `json:"phase"`
	OwnershipSource  string   `json:"ownershipSource"`
	CommentsComplete bool     `json:"commentsComplete"`
}

// ticketEvidence reads the ticket live from this project's own ledger and
// derives its owner and phase from the ledger's ownership events.
func ticketEvidence(ctx context.Context, source querySource, identifier string, now time.Time) intake.Evidence {
	e := intake.Evidence{Source: "ticket ledger " + source.store.Repository(), CollectedAt: now}
	t, err := source.store.Read(ctx, identifier)
	if err != nil {
		e.Outcome, e.Error = intake.OutcomeFailed, "账本读取失败："+runx.RedactDiagnostic(err.Error())
		return e
	}
	if t.Metadata.Identifier != identifier {
		e.Outcome, e.Error = intake.OutcomeFailed, "账本返回的票号与请求不一致，未采用"
		return e
	}
	e.SourceGeneratedAt = t.UpdatedAt
	labels := []string{}
	for _, l := range t.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	blockers := []string{}
	for _, b := range t.BlockedBy {
		blockers = append(blockers, b.Issue.Identifier)
	}
	if len(blockers) == 0 {
		blockers = append(blockers, t.Metadata.Blockers...)
	}
	detail := ticketDetail{Identifier: identifier, Title: t.Title, State: t.Metadata.State, Labels: labels, Blockers: blockers, Related: append([]string{}, t.Metadata.Related...),
		Managed: t.Metadata.Complete, Held: t.Held, URL: t.URL, Owner: "未知", Phase: "未知", OwnershipSource: "ledger comments"}
	e.Outcome, e.Complete = intake.OutcomeOK, true
	comments, err := source.store.ListComments(ctx, t.ID)
	if err == nil {
		var ownership scheduler.Ticket
		if ownership, err = scheduler.InspectOwnership(comments); err == nil {
			detail.CommentsComplete = true
			if ownership.Owner != "" {
				detail.Owner = string(ownership.Owner)
			}
			if ownership.Phase != "" {
				detail.Phase = string(ownership.Phase)
			}
		}
	}
	if err != nil {
		e.Outcome, e.Complete, e.Error = intake.OutcomePartial, false, "归属事件读取失败，归属未知："+runx.RedactDiagnostic(err.Error())
	}
	return encodeEvidence(e, detail)
}

// logFields are the only record fields a logs query returns.
var logFields = []string{"time", "level", "component", "msg", "event", "phase", "stage", "result", "outcome", "reason", "error", "err", "exit_code", "status", "duration_ms", "queue_id", "attempt", "executable", "stderr", "method", "path"}

// logsEvidence reads the ticket's executor and resolver logs from this
// project's own directory, plus shared scheduler records attributed to this
// project; a shared record that names neither its checkout path nor its
// repository cannot be attributed and is left out. The driver names the
// per-ticket directory by repository name alone, so when another bound
// project has the same name that directory is not read at all.
func logsEvidence(readers evidenceReaders, source querySource, sharedLogDir bool, identifier string, window time.Duration, now time.Time) intake.Evidence {
	e := intake.Evidence{Source: "driver logs " + identifier + "（" + source.logKey + "）", CollectedAt: now}
	cutoff := now.Add(-window)
	dir := filepath.Join(readers.logRoot, source.logKey, identifier)
	var records []logsRecord
	var failures []string
	names := []string{"executor.log", "resolver.log", "scheduler.log"}
	if sharedLogDir {
		names = names[2:]
		failures = append(failures, "executor.log 与 resolver.log 未读取：日志目录 "+source.logKey+" 被多个项目共用，无法归属")
	}
	for _, name := range names {
		path, shared := filepath.Join(dir, name), name == "scheduler.log"
		if shared {
			path = filepath.Join(readers.logRoot, name)
		}
		rotations, err := logsRotations(path)
		if err != nil {
			failures = append(failures, name)
			continue
		}
		for _, file := range append(rotations, path) {
			batch, err := readLogsFile(&logsFile{path: file, shared: shared}, identifier, cutoff)
			if err != nil {
				failures = append(failures, filepath.Base(file))
				continue
			}
			for _, record := range batch {
				if shared && !source.owns(record) {
					continue
				}
				records = append(records, record)
			}
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].when.Before(records[j].when) })
	omitted := 0
	if len(records) > intake.MaxLogRecords {
		omitted = len(records) - intake.MaxLogRecords
		records = records[omitted:]
	}
	events := make([]map[string]any, 0, len(records))
	for _, record := range records {
		event := map[string]any{}
		for _, key := range logFields {
			raw, ok := record.fields[key]
			if !ok {
				continue
			}
			var value any
			if json.Unmarshal(raw, &value) != nil {
				continue
			}
			if text, ok := value.(string); ok {
				value = runx.RedactDiagnostic(text)
			}
			event[key] = value
		}
		events = append(events, event)
	}
	switch {
	case len(failures) > 0 && len(events) == 0:
		e.Outcome, e.Error = intake.OutcomeFailed, "日志读取失败："+strings.Join(failures, ", ")
		return e
	case len(failures) > 0:
		e.Outcome, e.Error = intake.OutcomePartial, "部分日志读取失败："+strings.Join(failures, ", ")
	case len(events) == 0:
		e.Outcome, e.Complete = intake.OutcomeEmpty, true
	default:
		e.Outcome, e.Complete = intake.OutcomeOK, omitted == 0
	}
	return encodeEvidence(e, map[string]any{"window": window.String(), "records": events, "omittedOlder": omitted})
}

// owns reports whether a shared scheduler record names this project:
// decision records carry the checkout path as repo, merge-lane records carry
// AGENT_GH_REPO as repository or repo. An empty value never matches.
func (s querySource) owns(record logsRecord) bool {
	for _, key := range []string{"repo", "repository"} {
		value := logsString(record.fields[key])
		if value == "" {
			continue
		}
		if filepath.IsAbs(value) && filepath.Clean(value) == s.repo || s.ghRepo != "" && value == s.ghRepo {
			return true
		}
	}
	return false
}

// intakeTickets builds the ledger reader a Runner writes into
// .intake/tickets.md before every Agent turn (OF-33). Labels come from the
// ledger; owner and phase come only from a fresh status snapshot of this
// project, and stay unknown otherwise (OF-427).
func intakeTickets(store issues.Store, repo string, readers evidenceReaders) func(context.Context) ([]intake.TicketContext, error) {
	repo = filepath.Clean(repo)
	return func(ctx context.Context) ([]intake.TicketContext, error) {
		tasks, err := store.List(ctx)
		if err != nil {
			return nil, err
		}
		owners := map[string]statusTicketEvidence{}
		now := readers.now()
		if view := readers.status(now, intake.StatusStaleAfter); view.State == "ok" && now.Sub(view.GeneratedAt) <= intake.StatusStaleAfter {
			for _, t := range projectStatus(view, repo) {
				owners[t.Identifier] = t
			}
		}
		out := make([]intake.TicketContext, 0, len(tasks))
		for _, task := range tasks {
			blockers := make([]string, 0, len(task.BlockedBy))
			for _, b := range task.BlockedBy {
				blockers = append(blockers, b.Issue.Identifier)
			}
			labels := make([]string, 0, len(task.Labels.Nodes))
			for _, l := range task.Labels.Nodes {
				labels = append(labels, l.Name)
			}
			observed := owners[task.Identifier]
			out = append(out, intake.TicketContext{Identifier: task.Identifier, State: task.Metadata.State, Title: task.Title, Blockers: blockers,
				Labels: labels, Owner: observed.Owner, Phase: observed.Phase})
		}
		return out, nil
	}
}
