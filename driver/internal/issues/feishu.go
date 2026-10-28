package issues

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

var errFeishuTaskNotFound = errors.New("task not found")

// errFeishuRowUnsettled marks a row whose 序号 base/v3 has not assigned yet.
// Such a row is not yet a task: reads skip it and retry on a later round.
var errFeishuRowUnsettled = errors.New("feishu ticket row has no 序号 yet")

// errFeishuTableChanged marks a listing whose pages carried different table revs.
var errFeishuTableChanged = errors.New("feishu table changed between pages")

const (
	feishuPageLimit    = 500
	feishuReadAttempts = 3
	// feishuBatchLimit is the base/v3 per-request record limit.
	feishuBatchLimit       = 200
	feishuReadBackTimeout  = 60 * time.Second
	feishuReadBackInterval = 2 * time.Second
	// feishuEventFilterLimit bounds the tickets one 事件 filter names. Every
	// project shares the 事件 table and any write changes its rev, which restarts
	// a multi-page read; at 10, production reads spanned at most 2 pages and 4 s,
	// against 15 pages and 24 s at 100 (OF-293).
	feishuEventFilterLimit = 10
)

var feishuTables = []feishu.TableSpec{
	{Name: "项目", Fields: []feishu.FieldSpec{
		{Name: "名称", Type: "text"},
		{Name: "仓库远端", Type: "text"},
		{Name: "编号前缀", Type: "text"},
		{Name: "启用", Type: "checkbox"},
	}},
	{Name: "票据", Fields: []feishu.FieldSpec{
		{Name: "标题", Type: "text"},
		{Name: "序号", Type: "auto_number"},
		{Name: "规范ID", Type: "text"},
		{Name: "项目", Type: "link"},
		{Name: "状态", Type: "select", Options: []string{"Backlog", "Todo", "In Progress", "Done", "Canceled", "Duplicate"}},
		{Name: "标签", Type: "select", Multiple: true, Options: []string{"ready-for-agent", "auto-impl", "auto-failed"}},
		{Name: "要做", Type: "text"},
		{Name: "CHECK", Type: "text"},
		{Name: "SCOPE", Type: "text"},
		{Name: "正文", Type: "text"},
		{Name: "阻塞于", Type: "link"},
		{Name: "相关", Type: "link"},
		{Name: "估算", Type: "number"},
		{Name: "纳管指纹", Type: "text"},
		{Name: "创建时间", Type: "created_at"},
		{Name: "修改时间", Type: "updated_at"},
	}},
	{Name: "事件", Fields: []feishu.FieldSpec{
		{Name: "摘要", Type: "text"},
		{Name: "票据", Type: "link"},
		{Name: "正文", Type: "text"},
		{Name: "来源", Type: "select", Options: []string{"driver", "resolver", "operator"}},
		{Name: "操作者", Type: "text"},
		{Name: "创建时间", Type: "created_at"},
		{Name: "修改时间", Type: "updated_at"},
	}},
}

// FeishuBrief is the structured ticket face stored in Feishu fields.
// Blockers and Related hold canonical IDs.
type FeishuBrief struct {
	Todo, Check, Scope, Body string
	Blockers, Related        []string
}

func normalizeBriefText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

// Fingerprint is the adoption fingerprint over 要做, CHECK, SCOPE and the
// sorted canonical dependency IDs. 正文 is excluded.
func (b FeishuBrief) Fingerprint() string {
	blockers := slices.Sorted(slices.Values(b.Blockers))
	related := slices.Sorted(slices.Values(b.Related))
	if blockers == nil {
		blockers = []string{}
	}
	if related == nil {
		related = []string{}
	}
	raw, err := json.Marshal([]any{"orca-feishu-adopt-v1", normalizeBriefText(b.Todo), normalizeBriefText(b.Check), normalizeBriefText(b.Scope), blockers, related})
	if err != nil {
		panic(err) // strings and string slices always marshal
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}

// Description renders the intake ticket template read by ParseTicket and the
// scheduler's declared scope.
func (b FeishuBrief) Description() string {
	deps, err := json.Marshal(AdoptInput{Blockers: nonNil(b.Blockers), Related: nonNil(b.Related)})
	if err != nil {
		panic(err) // string slices always marshal
	}
	var s strings.Builder
	if body := normalizeBriefText(b.Body); body != "" {
		s.WriteString(body + "\n\n")
	}
	s.WriteString("## 要做\n" + normalizeBriefText(b.Todo) + "\n\n")
	s.WriteString("## 验收\nCHECK: " + strings.Join(strings.Split(normalizeBriefText(b.Check), "\n"), " ") + "\n\n")
	s.WriteString("## SCOPE\n" + strings.Join(strings.Fields(b.Scope), " ") + "\n\n")
	s.WriteString("## 依赖\nDEPENDENCIES: " + string(deps) + "\n")
	return s.String()
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

type feishuTable struct {
	id     string
	fields map[string]string // name -> field ID
}

// FeishuOptions tunes write read-back; zero values select the defaults.
type FeishuOptions struct {
	ReadBackTimeout time.Duration
	Now             func() time.Time
	Sleep           func(context.Context, time.Duration) error
}

// Feishu reads and writes tasks in one Feishu Base whose 项目 row matches the
// repository.
type Feishu struct {
	client          *feishu.Client
	base            string
	baseURL         string
	repo            string
	tables          map[string]feishuTable
	readBackTimeout time.Duration
	now             func() time.Time
	sleep           func(context.Context, time.Duration) error
	mu              sync.Mutex
	cached          map[string]Task
}

var _ Store = (*Feishu)(nil)

// feishuBase accepts a base token or a Base URL whose path carries /base/<token>.
func feishuBase(raw string) (token, baseURL string, err error) {
	if !strings.Contains(raw, "/") {
		if raw == "" {
			return "", "", errors.New("AGENT_FEISHU_BASE required")
		}
		return raw, "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", "", errors.New("invalid AGENT_FEISHU_BASE URL")
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	i := slices.Index(segments, "base")
	if i < 0 || i+1 >= len(segments) || segments[i+1] == "" {
		return "", "", errors.New("AGENT_FEISHU_BASE URL lacks /base/<token>")
	}
	token = segments[i+1]
	return token, u.Scheme + "://" + u.Host + "/base/" + token, nil
}

func newFeishuFromEnv(ctx context.Context, env map[string]string) (*Feishu, error) {
	token, baseURL, err := feishuBase(env["AGENT_FEISHU_BASE"])
	if err != nil {
		return nil, err
	}
	machine, err := feishuMachineSettings()
	if err != nil {
		return nil, err
	}
	client, err := feishu.New(feishu.Options{Endpoint: machine["AGENT_FEISHU_ENDPOINT"], AppID: machine["AGENT_FEISHU_APP_ID"], AppSecret: machine["AGENT_FEISHU_APP_SECRET"]})
	if err != nil {
		return nil, err
	}
	return NewFeishu(ctx, client, token, baseURL, env["AGENT_FEISHU_PROJECT"], FeishuOptions{})
}

var feishuMachineKeys = []string{"AGENT_FEISHU_ENDPOINT", "AGENT_FEISHU_APP_ID", "AGENT_FEISHU_APP_SECRET"}

// feishuMachineSettings reads the app credentials and endpoint from the process
// environment, falling back to the machine driver.env: executors and resolvers
// start in Orca terminals that do not inherit the service EnvironmentFile.
func feishuMachineSettings() (map[string]string, error) {
	settings := map[string]string{}
	for _, key := range feishuMachineKeys {
		settings[key] = os.Getenv(key)
	}
	if settings["AGENT_FEISHU_APP_ID"] != "" && settings["AGENT_FEISHU_APP_SECRET"] != "" {
		return settings, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return settings, nil
	}
	f, err := os.Open(filepath.Join(home, ".config", "orca-factory", "driver.env"))
	if errors.Is(err, fs.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return nil, errors.New("driver.env could not be read")
	}
	file, parseErr := config.ParseEnv(f)
	closeErr := f.Close()
	if parseErr != nil || closeErr != nil {
		return nil, errors.New("driver.env is invalid")
	}
	for _, key := range feishuMachineKeys {
		if settings[key] == "" {
			settings[key] = file[key]
		}
	}
	return settings, nil
}

// NewFeishu locates the 项目, 票据 and 事件 tables and validates their schema.
// baseURL may be empty; it only forms record links.
func NewFeishu(ctx context.Context, client *feishu.Client, base, baseURL, repo string, o FeishuOptions) (*Feishu, error) {
	if client == nil || base == "" || strings.TrimSpace(repo) == "" {
		return nil, errors.New("feishu backend requires client, base and AGENT_FEISHU_PROJECT")
	}
	listed, err := client.Tables(ctx, base)
	if err != nil {
		return nil, err
	}
	if o.ReadBackTimeout <= 0 {
		o.ReadBackTimeout = feishuReadBackTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	c := &Feishu{client: client, base: base, baseURL: baseURL, repo: repo, tables: map[string]feishuTable{}, readBackTimeout: o.ReadBackTimeout, now: o.Now, sleep: o.Sleep}
	for _, spec := range feishuTables {
		i := slices.IndexFunc(listed, func(t feishu.Table) bool { return t.Name == spec.Name })
		if i < 0 || slices.ContainsFunc(listed[i+1:], func(t feishu.Table) bool { return t.Name == spec.Name }) {
			return nil, fmt.Errorf("feishu table %s missing or duplicated", spec.Name)
		}
		fields, err := client.Fields(ctx, base, listed[i].ID)
		if err != nil {
			return nil, err
		}
		if err = feishu.ValidateSchema(spec, fields); err != nil {
			return nil, err
		}
		ids := map[string]string{}
		for _, f := range fields {
			ids[f.Name] = f.ID
		}
		c.tables[spec.Name] = feishuTable{id: listed[i].ID, fields: ids}
	}
	return c, nil
}

func (c *Feishu) Repository() string { return c.repo }

// readAll pages through one listing; every page must carry the same table rev.
// A write landing mid-read restarts the listing, a bounded number of times.
func (c *Feishu) readAll(ctx context.Context, table string, filter *feishu.Filter) ([]feishu.Record, error) {
	var err error
	for range feishuReadAttempts {
		var records []feishu.Record
		if records, err = c.readPages(ctx, table, filter); !errors.Is(err, errFeishuTableChanged) {
			return records, err
		}
	}
	return nil, err
}

func (c *Feishu) readPages(ctx context.Context, table string, filter *feishu.Filter) ([]feishu.Record, error) {
	id := c.tables[table].id
	records := []feishu.Record{}
	var rev int64
	limit := feishuPageLimit
	for offset := 0; ; {
		var page feishu.Page
		var err error
		if filter == nil {
			page, err = c.client.ListRecords(ctx, c.base, id, offset, limit)
		} else {
			page, err = c.client.ListRecordsWhere(ctx, c.base, id, *filter, offset, limit)
		}
		// Record sizes vary too widely for a fixed page to stay under the 10 MiB
		// response cap: 200 production events measured 34 KB to 7.5 MB (OF-293).
		// Later pages keep the smaller size.
		var apiErr *feishu.APIError
		if errors.As(err, &apiErr) && apiErr.Code == feishu.CodeResponseTooLarge && limit > 1 {
			limit /= 2
			continue
		}
		if err != nil {
			return nil, err
		}
		if offset == 0 {
			rev = page.Rev
		} else if page.Rev != rev {
			return nil, fmt.Errorf("%w: %s (rev %d, %d)", errFeishuTableChanged, table, rev, page.Rev)
		}
		records = append(records, page.Records...)
		if !page.HasMore {
			return records, nil
		}
		if len(page.Records) == 0 {
			return nil, fmt.Errorf("feishu table %s returned an empty page with more pending", table)
		}
		offset += len(page.Records)
	}
}

func (c *Feishu) cell(table string, r feishu.Record, name string) json.RawMessage {
	v := r.Fields[c.tables[table].fields[name]]
	if string(v) == "null" {
		return nil
	}
	return v
}

func (c *Feishu) text(table string, r feishu.Record, name string) (string, error) {
	var s string
	if raw := c.cell(table, r, name); raw != nil && json.Unmarshal(raw, &s) != nil {
		return "", fmt.Errorf("feishu field %s is not text", name)
	}
	return s, nil
}

func (c *Feishu) options(table string, r feishu.Record, name string) ([]string, error) {
	values := []string{}
	if raw := c.cell(table, r, name); raw != nil && json.Unmarshal(raw, &values) != nil {
		return nil, fmt.Errorf("feishu field %s is not a select", name)
	}
	return values, nil
}

func (c *Feishu) links(table string, r feishu.Record, name string) ([]string, error) {
	var refs []feishu.LinkRef
	if raw := c.cell(table, r, name); raw != nil && json.Unmarshal(raw, &refs) != nil {
		return nil, fmt.Errorf("feishu field %s is not a link", name)
	}
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	return ids, nil
}

func (c *Feishu) timestamp(table string, r feishu.Record, name string) (time.Time, error) {
	s, err := c.text(table, r, name)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("feishu field %s is not a timestamp", name)
	}
	return t, nil
}

type feishuProject struct {
	prefix  string
	enabled bool
}

// LedgerProject is one row of the ledger's 项目 table, enabled or not, for a
// caller outside Store that resolves a project by its repository remote (the
// Feishu intake gateway's project selection, OF-32).
type LedgerProject struct {
	Name, Remote, Prefix string
	Enabled              bool
}

func (c *Feishu) ledgerProjectRow(r feishu.Record) (LedgerProject, error) {
	name, err := c.text("项目", r, "名称")
	if err != nil {
		return LedgerProject{}, err
	}
	remote, err := c.text("项目", r, "仓库远端")
	if err != nil {
		return LedgerProject{}, err
	}
	prefix, err := c.text("项目", r, "编号前缀")
	if err != nil {
		return LedgerProject{}, err
	}
	var enabled bool
	if raw := c.cell("项目", r, "启用"); raw != nil && json.Unmarshal(raw, &enabled) != nil {
		return LedgerProject{}, errors.New("feishu field 启用 is not a checkbox")
	}
	return LedgerProject{Name: name, Remote: remote, Prefix: prefix, Enabled: enabled}, nil
}

// LedgerProjects lists every row of the ledger's 项目 table.
func (c *Feishu) LedgerProjects(ctx context.Context) ([]LedgerProject, error) {
	rows, err := c.readAll(ctx, "项目", nil)
	if err != nil {
		return nil, err
	}
	out := make([]LedgerProject, 0, len(rows))
	for _, r := range rows {
		row, err := c.ledgerProjectRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

func (c *Feishu) projects(ctx context.Context) (map[string]feishuProject, string, error) {
	rows, err := c.readAll(ctx, "项目", nil)
	if err != nil {
		return nil, "", err
	}
	all := map[string]feishuProject{}
	own := ""
	for _, r := range rows {
		row, err := c.ledgerProjectRow(r)
		if err != nil {
			return nil, "", err
		}
		all[r.ID] = feishuProject{prefix: row.Prefix, enabled: row.Enabled}
		if row.Remote == c.repo {
			if own != "" {
				return nil, "", errors.New("feishu project repository duplicated")
			}
			own = r.ID
		}
	}
	if own == "" || !component.MatchString(all[own].prefix) {
		return nil, "", errors.New("feishu project missing or without a valid 编号前缀")
	}
	return all, own, nil
}

var autoNumber = regexp.MustCompile(`([0-9]+)$`)

// feishuTicketRow is one ticket record before dependencies are resolved.
type feishuTicketRow struct {
	record      string
	project     string
	canonical   string
	fingerprint string
	number      int
	state       string
	labels      []string
	title       string
	brief       FeishuBrief
	estimate    *float64
	blockers    []string // record IDs
	related     []string
	created     time.Time
	updated     time.Time
}

func (c *Feishu) row(r feishu.Record) (feishuTicketRow, error) {
	const t = "票据"
	row := feishuTicketRow{record: r.ID}
	var err error
	for name, target := range map[string]*string{"规范ID": &row.canonical, "纳管指纹": &row.fingerprint, "标题": &row.title, "要做": &row.brief.Todo, "CHECK": &row.brief.Check, "SCOPE": &row.brief.Scope, "正文": &row.brief.Body} {
		if *target, err = c.text(t, r, name); err != nil {
			return row, err
		}
	}
	if row.canonical != "" && !uuid.MatchString(row.canonical) {
		return row, errors.New("feishu 规范ID is not a UUID")
	}
	serial, err := c.text(t, r, "序号")
	if err != nil {
		return row, err
	}
	if serial == "" {
		return row, errFeishuRowUnsettled
	}
	m := autoNumber.FindStringSubmatch(serial)
	if m == nil {
		return row, errors.New("feishu 序号 lacks a number")
	}
	if row.number, err = strconv.Atoi(m[1]); err != nil || row.number <= 0 {
		return row, errors.New("feishu 序号 is invalid")
	}
	states, err := c.options(t, r, "状态")
	if err != nil {
		return row, err
	}
	if len(states) > 1 || len(states) == 1 && !validState(states[0]) {
		return row, errors.New("feishu 状态 is invalid")
	}
	if len(states) == 1 {
		row.state = states[0]
	}
	if row.labels, err = c.options(t, r, "标签"); err != nil {
		return row, err
	}
	projects, err := c.links(t, r, "项目")
	if err != nil {
		return row, err
	}
	if len(projects) == 1 {
		row.project = projects[0]
	}
	if row.blockers, err = c.links(t, r, "阻塞于"); err != nil {
		return row, err
	}
	if row.related, err = c.links(t, r, "相关"); err != nil {
		return row, err
	}
	if raw := c.cell(t, r, "估算"); raw != nil {
		var v float64
		if json.Unmarshal(raw, &v) != nil {
			return row, errors.New("feishu 估算 is not a number")
		}
		row.estimate = &v
	}
	if row.created, err = c.timestamp(t, r, "创建时间"); err != nil {
		return row, err
	}
	row.updated, err = c.timestamp(t, r, "修改时间")
	return row, err
}

func (row feishuTicketRow) id() string {
	if row.canonical != "" {
		return row.canonical
	}
	sum := sha256.Sum256([]byte("feishu-record:" + row.record))
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func closedState(state string) bool {
	switch stateType(state) {
	case "completed", "canceled", "duplicate":
		return true
	}
	return false
}

// List reads this project's tickets and the records their links reference.
func (c *Feishu) List(ctx context.Context) ([]Task, error) {
	projects, own, err := c.projects(ctx)
	if err != nil {
		return nil, err
	}
	filter := feishu.Filter{Logic: "and", Conditions: []feishu.Condition{{Field: "项目", Operator: "intersects", Value: []feishu.LinkRef{{ID: own}}}}}
	records, err := c.readAll(ctx, "票据", &filter)
	if err != nil {
		return nil, err
	}
	known := map[string]feishuTicketRow{}
	rows := make([]feishuTicketRow, 0, len(records))
	for _, r := range records {
		row, err := c.row(r)
		if errors.Is(err, errFeishuRowUnsettled) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if row.project != own {
			return nil, errors.New("feishu ticket filter returned another project")
		}
		known[row.record] = row
		rows = append(rows, row)
	}
	missing := []string{}
	for _, row := range rows {
		for _, ref := range append(slices.Clone(row.blockers), row.related...) {
			if _, ok := known[ref]; !ok && !slices.Contains(missing, ref) {
				missing = append(missing, ref)
			}
		}
	}
	for chunk := range slices.Chunk(missing, feishuBatchLimit) {
		page, err := c.client.BatchGet(ctx, c.base, c.tables["票据"].id, chunk)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Records {
			row, err := c.row(r)
			if errors.Is(err, errFeishuRowUnsettled) {
				continue
			}
			if err != nil {
				return nil, err
			}
			known[row.record] = row
		}
	}
	tasks := make([]Task, 0, len(rows))
	ids := map[string]bool{}
	for _, row := range rows {
		task := c.task(row, known, projects)
		if ids[task.ID] || ids[task.Identifier] {
			return nil, errors.New("duplicate canonical task identity")
		}
		ids[task.ID], ids[task.Identifier] = true, true
		tasks = append(tasks, task)
	}
	c.mu.Lock()
	c.cached = map[string]Task{}
	for _, t := range tasks {
		c.cached[t.ID] = t
		c.cached[t.Identifier] = t
	}
	c.mu.Unlock()
	return tasks, nil
}

func identifier(row feishuTicketRow, projects map[string]feishuProject) string {
	return projects[row.project].prefix + "-" + strconv.Itoa(row.number)
}

// task maps a row like the GitHub backend: unmanaged, held, disabled or
// dependency-incomplete rows stay visible but report HasNextPage, so they
// are never claimable.
func (c *Feishu) task(row feishuTicketRow, known map[string]feishuTicketRow, projects map[string]feishuProject) Task {
	managed := row.canonical != "" && row.fingerprint != ""
	incomplete := !managed || row.state == "" || !projects[row.project].enabled
	state := row.state
	if state == "" {
		state = "Backlog"
	}
	labels := task.Connection[task.NamedID]{Nodes: []task.NamedID{}}
	for _, l := range row.labels {
		labels.Nodes = append(labels.Nodes, task.NamedID{ID: l, Name: l})
	}
	m := Metadata{ID: row.id(), Identifier: identifier(row, projects), Project: c.repo, State: state, Estimate: row.estimate, Blockers: []string{}, Related: []string{}, Complete: managed}
	inverse := []task.InverseRelation{}
	relations := []task.Relation{}
	for _, ref := range row.blockers {
		dep, ok := known[ref]
		r := task.InverseRelation{ID: ref, Type: "blocks"}
		r.Issue.ID, r.Issue.Identifier, r.Issue.State.Type = ref, ref, "started"
		if ok && dep.canonical != "" {
			r.ID, r.Issue.ID, r.Issue.Identifier = dep.canonical, dep.canonical, identifier(dep, projects)
			if closedState(dep.state) {
				r.Issue.State.Type = stateType(dep.state)
			}
			m.Blockers = append(m.Blockers, dep.canonical)
		} else {
			incomplete = true
		}
		inverse = append(inverse, r)
	}
	for _, ref := range row.related {
		dep, ok := known[ref]
		r := task.Relation{ID: ref, Type: "related", RelatedIssue: task.IssueRef{ID: ref}}
		if ok && dep.canonical != "" {
			r.ID, r.RelatedIssue = dep.canonical, task.IssueRef{ID: dep.canonical, Identifier: identifier(dep, projects)}
			m.Related = append(m.Related, dep.canonical)
		} else {
			incomplete = true
		}
		relations = append(relations, r)
	}
	brief := row.brief
	brief.Blockers, brief.Related = m.Blockers, m.Related
	m.ImportHold = managed && brief.Fingerprint() != row.fingerprint
	incomplete = incomplete || m.ImportHold
	nativeState := "open"
	if closedState(state) {
		nativeState = "closed"
	}
	link := ""
	if c.baseURL != "" {
		link = c.baseURL + "?table=" + url.QueryEscape(c.tables["票据"].id) + "&record=" + url.QueryEscape(row.record)
	}
	return Task{
		nativeID: row.record, NativeState: nativeState, NativeUpdatedAt: row.updated,
		Issue: task.Issue{ID: m.ID, Identifier: m.Identifier, Project: task.NamedID{ID: c.repo, Name: c.repo}, State: task.NamedID{ID: stateType(state), Name: state}, Labels: labels, CreatedAt: row.created, UpdatedAt: row.updated, URL: link, Held: m.ImportHold, Title: row.title,
			Relations:        task.Connection[task.Relation]{Nodes: relations, PageInfo: task.PageInfo{HasNextPage: incomplete}},
			InverseRelations: task.Connection[task.InverseRelation]{Nodes: inverse, PageInfo: task.PageInfo{HasNextPage: incomplete}}},
		URL: link, Title: row.title, Description: brief.Description(), Estimate: row.estimate, Metadata: m, Complete: !incomplete,
		Comments: task.Connection[task.Comment]{Nodes: []task.Comment{}}, BlockedBy: inverse,
	}
}

func (c *Feishu) Snapshot(ctx context.Context, projects []string, _ time.Time) ([]task.Issue, error) {
	for _, p := range projects {
		if p != c.repo {
			return nil, errors.New("snapshot repository mismatch")
		}
	}
	tasks, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]task.Issue, 0, len(tasks))
	for _, t := range tasks {
		rows = append(rows, t.Issue)
	}
	return rows, nil
}

func (c *Feishu) Read(ctx context.Context, id string) (Task, error) {
	rows, err := c.List(ctx)
	if err != nil {
		return Task{}, err
	}
	for _, v := range rows {
		if v.ID == id || v.Identifier == id {
			return v, nil
		}
	}
	return Task{}, errFeishuTaskNotFound
}

func (c *Feishu) cachedTask(ctx context.Context, id string) (Task, error) {
	c.mu.Lock()
	t, ok := c.cached[id]
	c.mu.Unlock()
	if ok {
		return t, nil
	}
	return c.Read(ctx, id)
}

func (c *Feishu) Description(ctx context.Context, id string) (string, error) {
	t, err := c.cachedTask(ctx, id)
	return t.Description, err
}

func (c *Feishu) FreshIssue(ctx context.Context, id string) (task.Issue, error) {
	t, err := c.Read(ctx, id)
	return t.Issue, err
}

// CommentsError names the ticket whose own ListComments fails with Err.
type CommentsError struct {
	ID  string
	Err error
}

func (e *CommentsError) Error() string { return e.ID + ": " + e.Err.Error() }
func (e *CommentsError) Unwrap() error { return e.Err }

// ListComments returns the ticket's events oldest first. An event whose
// 修改时间 differs from its 创建时间 was edited and is an evidence conflict.
func (c *Feishu) ListComments(ctx context.Context, id string) ([]task.Comment, error) {
	rows, err := c.SnapshotComments(ctx, []string{id})
	if err != nil {
		return nil, errors.Unwrap(err)
	}
	return rows[id], nil
}

// SnapshotComments lists the 事件 of many tickets with one filtered listing per
// chunk of tickets, instead of one listing per ticket. Each ticket gets exactly
// what its own ListComments returns; the first ticket in ids order whose read
// fails ends the call with a *CommentsError.
func (c *Feishu) SnapshotComments(ctx context.Context, ids []string) (map[string][]task.Comment, error) {
	out := make(map[string][]task.Comment, len(ids))
	for chunk := range slices.Chunk(ids, feishuEventFilterLimit) {
		reads := c.readEvents(ctx, chunk)
		for i, id := range chunk {
			if reads[i].err != nil {
				return nil, &CommentsError{ID: id, Err: reads[i].err}
			}
			out[id] = reads[i].history
		}
	}
	return out, nil
}

type ticketEvents struct {
	history []task.Comment
	err     error
}

// readEvents returns one result per ticket of chunk, in chunk order. A
// failure that cannot be traced to one ticket fails every ticket of the chunk.
func (c *Feishu) readEvents(ctx context.Context, chunk []string) []ticketEvents {
	reads := make([]ticketEvents, len(chunk))
	readers := map[string][]int{}
	refs := []feishu.LinkRef{}
	for i, id := range chunk {
		t, err := c.cachedTask(ctx, id)
		if err != nil {
			reads[i].err = err
			continue
		}
		reads[i].history = []task.Comment{}
		if readers[t.nativeID] == nil {
			refs = append(refs, feishu.LinkRef{ID: t.nativeID})
		}
		readers[t.nativeID] = append(readers[t.nativeID], i)
	}
	if len(refs) == 0 {
		return reads
	}
	failAll := func(err error) []ticketEvents {
		for i := range reads {
			if reads[i].err == nil {
				reads[i].err = err
			}
		}
		return reads
	}
	filter := feishu.Filter{Logic: "and", Conditions: []feishu.Condition{{Field: "票据", Operator: "intersects", Value: refs}}}
	records, err := c.readAll(ctx, "事件", &filter)
	if err != nil {
		return failAll(err)
	}
	for _, r := range records {
		owners, err := c.links("事件", r, "票据")
		if err != nil {
			return failAll(err)
		}
		comment, eventErr := c.event(r)
		if len(owners) != 1 {
			eventErr = fmt.Errorf("feishu event %s evidence conflict: linked to %d tickets", r.ID, len(owners))
		}
		for _, owner := range owners {
			for _, i := range readers[owner] {
				switch {
				case reads[i].err != nil:
				case eventErr != nil:
					reads[i].err = eventErr
				default:
					reads[i].history = append(reads[i].history, comment)
				}
			}
		}
	}
	for i := range reads {
		if reads[i].err != nil {
			continue
		}
		history := reads[i].history
		sort.SliceStable(history, func(a, b int) bool { return history[a].CreatedAt.Before(history[b].CreatedAt) })
		reads[i].history, reads[i].err = decodeImportedHistory(history)
	}
	return reads
}

func (c *Feishu) event(r feishu.Record) (task.Comment, error) {
	const table = "事件"
	body, err := c.text(table, r, "正文")
	if err != nil {
		return task.Comment{}, err
	}
	created, err := c.timestamp(table, r, "创建时间")
	if err != nil {
		return task.Comment{}, err
	}
	updated, err := c.timestamp(table, r, "修改时间")
	if err != nil {
		return task.Comment{}, err
	}
	if !updated.Equal(created) {
		return task.Comment{}, fmt.Errorf("feishu event %s evidence conflict: edited after creation", r.ID)
	}
	return task.Comment{ID: r.ID, Body: body, CreatedAt: created, UpdatedAt: updated}, nil
}
