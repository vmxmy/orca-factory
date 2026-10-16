package issues

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// ErrOwnershipConflict means a read-back found values neither written by this
// call nor present before it: another writer changed the ticket.
var ErrOwnershipConflict = errors.New("feishu ticket changed by another writer")

const feishuSummaryRunes = 200

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// cells keys values by field ID, which survives field renames.
func (c *Feishu) cells(table string, byName map[string]any) map[string]any {
	out := make(map[string]any, len(byName))
	for name, v := range byName {
		out[c.tables[table].fields[name]] = v
	}
	return out
}

func linkRefs(records ...string) []feishu.LinkRef {
	refs := make([]feishu.LinkRef, 0, len(records))
	for _, r := range records {
		refs = append(refs, feishu.LinkRef{ID: r})
	}
	return refs
}

// checkWrite rejects silently ignored fields: the driver only writes fields
// the schema check declared writable.
func checkWrite(res feishu.WriteResult, records int) error {
	if len(res.IgnoredFields) > 0 {
		names := make([]string, 0, len(res.IgnoredFields))
		for _, f := range res.IgnoredFields {
			names = append(names, f.Name)
		}
		return fmt.Errorf("feishu ignored fields %s", strings.Join(names, ", "))
	}
	if len(res.RecordIDs) != records {
		return errors.New("feishu write returned an unexpected record count")
	}
	return nil
}

func (c *Feishu) updateTicket(ctx context.Context, record string, byName map[string]any) error {
	res, err := c.client.BatchUpdate(ctx, c.base, c.tables["票据"].id, map[string]map[string]any{record: c.cells("票据", byName)})
	if err != nil {
		return err
	}
	return checkWrite(res, 1)
}

// readBack polls check until it reports done. Writes may stay invisible for
// a while (spec R2), so a timeout leaves the outcome unknown.
func (c *Feishu) readBack(ctx context.Context, check func() (bool, error)) error {
	deadline := c.now().Add(c.readBackTimeout)
	for {
		done, err := check()
		if err != nil || done {
			return err
		}
		if !c.now().Before(deadline) {
			return fmt.Errorf("feishu read-back not settled within %s: %w", c.readBackTimeout, infra.ErrWriteUnconfirmed)
		}
		if err = c.sleep(ctx, feishuReadBackInterval); err != nil {
			return err
		}
	}
}

func (c *Feishu) awaitTask(ctx context.Context, id string, settled func(Task) bool) (Task, error) {
	var task Task
	err := c.readBack(ctx, func() (bool, error) {
		t, err := c.Read(ctx, id)
		if errors.Is(err, errFeishuTaskNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		task = t
		return settled(t), nil
	})
	return task, err
}

func (c *Feishu) ticketRow(ctx context.Context, record string) (feishuTicketRow, error) {
	page, err := c.client.BatchGet(ctx, c.base, c.tables["票据"].id, []string{record})
	if err != nil {
		return feishuTicketRow{}, err
	}
	if len(page.Records) != 1 || page.Records[0].ID != record {
		return feishuTicketRow{}, errors.New("feishu ticket record unavailable")
	}
	return c.row(page.Records[0])
}

// feishuIndex holds every ticket row of the Base, across projects.
type feishuIndex struct {
	byRecord    map[string]feishuTicketRow
	byCanonical map[string]feishuTicketRow
}

func (c *Feishu) ticketIndex(ctx context.Context) (feishuIndex, error) {
	records, err := c.readAll(ctx, "票据", nil)
	if err != nil {
		return feishuIndex{}, err
	}
	x := feishuIndex{byRecord: map[string]feishuTicketRow{}, byCanonical: map[string]feishuTicketRow{}}
	for _, r := range records {
		row, err := c.row(r)
		if errors.Is(err, errFeishuRowUnsettled) {
			continue
		}
		if err != nil {
			return feishuIndex{}, err
		}
		x.byRecord[row.record] = row
		if row.canonical == "" {
			continue
		}
		if _, ok := x.byCanonical[row.canonical]; ok {
			return feishuIndex{}, errors.New("duplicate canonical task identity")
		}
		x.byCanonical[row.canonical] = row
	}
	return x, nil
}

// adopted reports a managed row whose fingerprint still matches its brief.
func (x feishuIndex) adopted(row feishuTicketRow) bool {
	if row.canonical == "" || row.fingerprint == "" {
		return false
	}
	brief := row.brief
	for _, deps := range []struct {
		records []string
		target  *[]string
	}{{row.blockers, &brief.Blockers}, {row.related, &brief.Related}} {
		for _, ref := range deps.records {
			dep, ok := x.byRecord[ref]
			if !ok || dep.canonical == "" {
				return false
			}
			*deps.target = append(*deps.target, dep.canonical)
		}
	}
	return brief.Fingerprint() == row.fingerprint
}

// links resolves canonical dependency IDs to adopted records other than self.
func (x feishuIndex) links(self string, ids []string) ([]feishu.LinkRef, error) {
	records := make([]string, 0, len(ids))
	for _, id := range ids {
		dep, ok := x.byCanonical[id]
		if !ok || id == self || !x.adopted(dep) {
			return nil, errors.New("dependency identity or completeness unavailable")
		}
		records = append(records, dep.record)
	}
	return linkRefs(records...), nil
}

func requireBrief(b FeishuBrief) error {
	if normalizeBriefText(b.Todo) == "" || normalizeBriefText(b.Check) == "" || normalizeBriefText(b.Scope) == "" {
		return errors.New("adopt requires 要做, CHECK and SCOPE")
	}
	return nil
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// feishuState maps the accepted spelling Cancelled to the Base's option.
func feishuState(s string) string {
	if s == "Cancelled" {
		return "Canceled"
	}
	return s
}

func sameEstimate(a, b *float64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// parseFeishuDescription splits an intake ticket face into the structured
// fields FeishuBrief.Description renders. Sections other than 要做 and SCOPE,
// and lines of 验收 and 依赖 besides CHECK and DEPENDENCIES, stay in Body.
// The declared dependencies are returned when a DEPENDENCIES line exists.
func parseFeishuDescription(description string) (FeishuBrief, *AdoptInput, error) {
	type section struct {
		heading string
		lines   []string
	}
	sections := []section{{}}
	for _, line := range strings.Split(strings.ReplaceAll(description, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "## ") {
			sections = append(sections, section{heading: line})
			continue
		}
		sections[len(sections)-1].lines = append(sections[len(sections)-1].lines, line)
	}
	var brief FeishuBrief
	var declared *AdoptInput
	body := []string{}
	for _, s := range sections {
		name := strings.TrimSpace(strings.TrimPrefix(s.heading, "## "))
		if i := strings.IndexAny(name, "（("); i > 0 {
			name = strings.TrimSpace(name[:i])
		}
		content := strings.TrimSpace(strings.Join(s.lines, "\n"))
		rest := []string{}
		switch name {
		case "要做":
			brief.Todo = content
			continue
		case "SCOPE":
			brief.Scope = strings.Join(strings.Fields(content), "\n")
			continue
		case "验收", "依赖":
			for _, line := range s.lines {
				trimmed := strings.TrimSpace(line)
				if v, ok := strings.CutPrefix(trimmed, "CHECK:"); ok && name == "验收" && brief.Check == "" {
					brief.Check = strings.TrimSpace(v)
					continue
				}
				if v, ok := strings.CutPrefix(trimmed, "DEPENDENCIES:"); ok && name == "依赖" && declared == nil {
					var deps AdoptInput
					if err := decodeImportJSON([]byte(strings.TrimSpace(v)), &deps); err != nil {
						return FeishuBrief{}, nil, errors.New("invalid DEPENDENCIES line")
					}
					declared = &deps
					continue
				}
				rest = append(rest, line)
			}
			if content = strings.TrimSpace(strings.Join(rest, "\n")); content == "" {
				continue
			}
		}
		switch {
		case s.heading == "" && content == "":
		case s.heading == "":
			body = append(body, content)
		case content == "":
			body = append(body, s.heading)
		default:
			body = append(body, s.heading+"\n"+content)
		}
	}
	brief.Body = strings.Join(body, "\n\n")
	return brief, declared, nil
}

func sameStateLabels(a, b feishuTicketRow) bool {
	return a.state == b.state && slices.Equal(slices.Sorted(slices.Values(a.labels)), slices.Sorted(slices.Values(b.labels)))
}

// UpdateIssue writes state and the merged label set in one batch_update, then
// reads the record back until both match. Reading the old values means the
// write is not visible yet; reading anything else is an ownership conflict,
// recorded as a handoff event before the error returns.
func (c *Feishu) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (task.Issue, error) {
	t, err := c.Read(ctx, id)
	if err != nil {
		return task.Issue{}, err
	}
	if !t.Metadata.Complete {
		return task.Issue{}, errors.New("explicit adoption required before update")
	}
	if state != "" && !validState(state) {
		return task.Issue{}, errors.New("unknown task state")
	}
	before, err := c.ticketRow(ctx, t.nativeID)
	if err != nil {
		return task.Issue{}, err
	}
	want := before
	labels := slices.Clone(before.labels)
	for _, l := range add {
		if !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	labels = slices.DeleteFunc(labels, func(l string) bool { return slices.Contains(remove, l) })
	slices.Sort(labels)
	want.labels = labels
	cells := map[string]any{"标签": labels}
	if state != "" {
		want.state = feishuState(state)
		cells["状态"] = []string{want.state}
	}
	if err = c.updateTicket(ctx, t.nativeID, cells); err != nil {
		return task.Issue{}, err
	}
	err = c.readBack(ctx, func() (bool, error) {
		got, err := c.ticketRow(ctx, t.nativeID)
		switch {
		case err != nil:
			return false, err
		case sameStateLabels(got, want):
			return true, nil
		case sameStateLabels(got, before):
			return false, nil
		}
		return false, c.conflict(ctx, t.ID, want, got)
	})
	if err != nil {
		return task.Issue{}, err
	}
	issue := t.Issue
	if want.state != "" {
		issue.State = task.NamedID{ID: stateType(want.state), Name: want.state}
	}
	issue.Labels = task.Connection[task.NamedID]{Nodes: []task.NamedID{}}
	for _, l := range labels {
		issue.Labels.Nodes = append(issue.Labels.Nodes, task.NamedID{ID: l, Name: l})
	}
	return issue, nil
}

func (c *Feishu) conflict(ctx context.Context, id string, want, got feishuTicketRow) error {
	body := fmt.Sprintf("转人: 飞书账本归属冲突，读回值与本次写入不同\n写入: 状态=%s 标签=%s\n读回: 状态=%s 标签=%s\n归属: 人",
		want.state, strings.Join(want.labels, ","), got.state, strings.Join(got.labels, ","))
	conflict := fmt.Errorf("%w: task %s", ErrOwnershipConflict, id)
	if _, err := c.CreateComment(ctx, id, body); err != nil {
		return errors.Join(conflict, err)
	}
	return conflict
}

// CreateComment appends one 事件 row whose 正文 is body unchanged.
func (c *Feishu) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	t, err := c.cachedTask(ctx, id)
	if err != nil {
		return task.Comment{}, err
	}
	summary, _, _ := strings.Cut(body, "\n")
	summary = strings.TrimSuffix(summary, "\r")
	if runes := []rune(summary); len(runes) > feishuSummaryRunes {
		summary = string(runes[:feishuSummaryRunes])
	}
	row := c.cells("事件", map[string]any{"摘要": summary, "票据": linkRefs(t.nativeID), "正文": body, "来源": []string{"driver"}})
	res, err := c.client.BatchCreate(ctx, c.base, c.tables["事件"].id, []map[string]any{row})
	if err != nil {
		return task.Comment{}, err
	}
	if err = checkWrite(res, 1); err != nil {
		return task.Comment{}, err
	}
	now := c.now().UTC()
	return task.Comment{ID: res.RecordIDs[0], Body: body, CreatedAt: now, UpdatedAt: now}, nil
}

// Create writes the structured ticket face parsed from the description and a
// new canonical ID. Explicit dependency lists also adopt the ticket.
func (c *Feishu) Create(ctx context.Context, in CreateInput) (Task, error) {
	if in.ID != "" || in.Identifier != "" || in.ImportDigest != "" || !in.CreatedAt.IsZero() || !in.UpdatedAt.IsZero() || in.ImportHold {
		return Task{}, errors.New("native create cannot set imported identity, provenance or hold")
	}
	if in.Parent != "" || in.Priority != 0 {
		return Task{}, errors.New("feishu backend stores no parent or priority")
	}
	if strings.TrimSpace(in.Title) == "" {
		return Task{}, errors.New("invalid native create input")
	}
	adopt := in.Blockers != nil && in.Related != nil
	state := in.State
	if state == "" {
		state = "Todo"
	}
	if !adopt && state != "Todo" {
		return Task{}, errors.New("explicit adoption required for native create state")
	}
	if !validState(state) {
		return Task{}, errors.New("unknown task state")
	}
	brief, declared, err := parseFeishuDescription(in.Description)
	if err != nil {
		return Task{}, err
	}
	_, own, err := c.projects(ctx)
	if err != nil {
		return Task{}, err
	}
	canonical, err := newUUID()
	if err != nil {
		return Task{}, err
	}
	row := map[string]any{"标题": in.Title, "项目": linkRefs(own), "状态": []string{feishuState(state)}, "标签": nonNil(in.Labels),
		"要做": brief.Todo, "CHECK": brief.Check, "SCOPE": brief.Scope, "正文": brief.Body, "规范ID": canonical}
	if in.Estimate != nil {
		row["估算"] = *in.Estimate
	}
	if adopt {
		if hasDuplicateStrings(in.Blockers) || hasDuplicateStrings(in.Related) {
			return Task{}, errors.New("explicit complete dependency lists required")
		}
		if err = requireBrief(brief); err != nil {
			return Task{}, err
		}
		if declared != nil && (!slices.Equal(declared.Blockers, in.Blockers) || !slices.Equal(declared.Related, in.Related)) {
			return Task{}, errors.New("explicit body DEPENDENCIES must match adoption input")
		}
		if err = c.dependencyCells(ctx, row, canonical, brief, in.Blockers, in.Related); err != nil {
			return Task{}, err
		}
	}
	res, err := c.client.BatchCreate(ctx, c.base, c.tables["票据"].id, []map[string]any{c.cells("票据", row)})
	if err != nil {
		return Task{}, err
	}
	if err = checkWrite(res, 1); err != nil {
		return Task{}, err
	}
	return c.awaitTask(ctx, canonical, func(t Task) bool { return !adopt || t.Metadata.Complete && !t.Held })
}

// CreateLedgerProject adds row to the 项目 table for an onboarded repository
// (OF-50) and reads it back. An identical existing row is returned without a
// write, so a retry after an unconfirmed write converges; a row whose remote
// or 编号前缀 another row already uses is refused.
func (c *Feishu) CreateLedgerProject(ctx context.Context, row LedgerProject) (LedgerProject, error) {
	find := func() (bool, error) {
		rows, err := c.LedgerProjects(ctx)
		if err != nil {
			return false, err
		}
		for _, r := range rows {
			if r == row {
				return true, nil
			}
			if r.Remote == row.Remote || r.Prefix == row.Prefix {
				return false, fmt.Errorf("项目表已有仓库远端 %s 或编号前缀 %s 的行", r.Remote, r.Prefix)
			}
		}
		return false, nil
	}
	if found, err := find(); err != nil || found {
		return row, err
	}
	cells := c.cells("项目", map[string]any{"名称": row.Name, "仓库远端": row.Remote, "编号前缀": row.Prefix, "启用": row.Enabled})
	res, err := c.client.BatchCreate(ctx, c.base, c.tables["项目"].id, []map[string]any{cells})
	if err != nil {
		return LedgerProject{}, err
	}
	if err = checkWrite(res, 1); err != nil {
		return LedgerProject{}, err
	}
	return row, c.readBack(ctx, find)
}

// EnableLedgerProject sets 启用=true for the 项目 row whose 仓库远端 is
// remote (OF-54) and records operator and the time as a 事件 row. A row
// already enabled is returned unchanged with no write and no event, so a
// repeated approver click converges without a duplicate event.
func (c *Feishu) EnableLedgerProject(ctx context.Context, remote, operator string) (LedgerProject, error) {
	records, err := c.readAll(ctx, "项目", nil)
	if err != nil {
		return LedgerProject{}, err
	}
	for _, r := range records {
		row, err := c.ledgerProjectRow(r)
		if err != nil {
			return LedgerProject{}, err
		}
		if row.Remote != remote {
			continue
		}
		if row.Enabled {
			return row, nil
		}
		res, err := c.client.BatchUpdate(ctx, c.base, c.tables["项目"].id, map[string]map[string]any{r.ID: c.cells("项目", map[string]any{"启用": true})})
		if err != nil {
			return LedgerProject{}, err
		}
		if err = checkWrite(res, 1); err != nil {
			return LedgerProject{}, err
		}
		enabled := row
		enabled.Enabled = true
		if err = c.readBack(ctx, func() (bool, error) {
			rows, err := c.LedgerProjects(ctx)
			if err != nil {
				return false, err
			}
			i := slices.IndexFunc(rows, func(x LedgerProject) bool { return x.Remote == remote })
			return i >= 0 && rows[i].Enabled, nil
		}); err != nil {
			return LedgerProject{}, err
		}
		body := fmt.Sprintf("启用：%s %s", operator, c.now().Format(time.RFC3339))
		if err = c.recordProjectEvent(ctx, "启用 "+enabled.Prefix, body, operator); err != nil {
			return LedgerProject{}, err
		}
		return enabled, nil
	}
	return LedgerProject{}, fmt.Errorf("项目表无仓库远端 %s 的行", remote)
}

// recordProjectEvent appends one 事件 row not tied to any ticket (OF-54,
// unlike CreateComment): 票据 stays empty, 操作者 carries who acted.
func (c *Feishu) recordProjectEvent(ctx context.Context, summary, body, operator string) error {
	row := c.cells("事件", map[string]any{"摘要": summary, "正文": body, "来源": []string{"driver"}, "操作者": operator})
	res, err := c.client.BatchCreate(ctx, c.base, c.tables["事件"].id, []map[string]any{row})
	if err != nil {
		return err
	}
	return checkWrite(res, 1)
}

// dependencyCells adds the dependency links and the adoption fingerprint.
func (c *Feishu) dependencyCells(ctx context.Context, row map[string]any, self string, brief FeishuBrief, blockers, related []string) error {
	x, err := c.ticketIndex(ctx)
	if err != nil {
		return err
	}
	if row["阻塞于"], err = x.links(self, blockers); err != nil {
		return err
	}
	if row["相关"], err = x.links(self, related); err != nil {
		return err
	}
	brief.Blockers, brief.Related = blockers, related
	row["纳管指纹"] = brief.Fingerprint()
	return nil
}

// Adopt writes the dependency links and the fingerprint over the current
// face. A held task (face edited after adoption) is adopted again.
func (c *Feishu) Adopt(ctx context.Context, id string, raw []byte) (Task, error) {
	var in AdoptInput
	if err := decodeImportJSON(raw, &in); err != nil {
		return Task{}, err
	}
	if in.Blockers == nil || in.Related == nil || hasDuplicateStrings(in.Blockers) || hasDuplicateStrings(in.Related) {
		return Task{}, errors.New("explicit complete dependency lists required")
	}
	t, err := c.Read(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if t.Metadata.Complete && !t.Held {
		return Task{}, errors.New("task already managed")
	}
	if t.NativeState != "open" {
		return Task{}, errors.New("closed task cannot be adopted")
	}
	current, err := c.ticketRow(ctx, t.nativeID)
	if err != nil {
		return Task{}, err
	}
	if err = requireBrief(current.brief); err != nil {
		return Task{}, err
	}
	row := map[string]any{}
	canonical := current.canonical
	if canonical == "" {
		if canonical, err = newUUID(); err != nil {
			return Task{}, err
		}
		row["规范ID"] = canonical
	}
	if err = c.dependencyCells(ctx, row, canonical, current.brief, in.Blockers, in.Related); err != nil {
		return Task{}, err
	}
	if err = c.updateTicket(ctx, t.nativeID, row); err != nil {
		return Task{}, err
	}
	return c.awaitTask(ctx, canonical, func(t Task) bool {
		return t.Metadata.Complete && !t.Held && slices.Equal(t.Metadata.Blockers, in.Blockers) && slices.Equal(t.Metadata.Related, in.Related)
	})
}

// Edit changes only supplied fields of an adopted, unheld task and refreshes
// the fingerprint, because the operator restated the face explicitly.
func (c *Feishu) Edit(ctx context.Context, id string, raw []byte) (Task, error) {
	var patch map[string]json.RawMessage
	if decodeImportJSON(raw, &patch) != nil || patch == nil {
		return Task{}, errors.New("invalid task patch")
	}
	t, err := c.Read(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if !t.Metadata.Complete {
		return Task{}, errors.New("explicit adoption required before edit")
	}
	if t.Held {
		return Task{}, errors.New("held task requires adopt before edit")
	}
	current, err := c.ticketRow(ctx, t.nativeID)
	if err != nil {
		return Task{}, err
	}
	title, estimate, brief := current.title, current.estimate, current.brief
	blockers, related := t.Metadata.Blockers, t.Metadata.Related
	var description string
	var declared *AdoptInput
	targets := map[string]any{"title": &title, "description": &description, "estimate": &estimate, "blockers": &blockers, "related": &related}
	for k, v := range patch {
		target, ok := targets[k]
		if !ok {
			return Task{}, errors.New("unknown or immutable task patch field")
		}
		if json.Unmarshal(v, target) != nil {
			return Task{}, errors.New("invalid task patch field")
		}
	}
	if _, ok := patch["description"]; ok {
		if brief, declared, err = parseFeishuDescription(description); err != nil {
			return Task{}, err
		}
	}
	if estimate != nil && *estimate < 0 || blockers == nil || related == nil || hasDuplicateStrings(blockers) || hasDuplicateStrings(related) || strings.TrimSpace(title) == "" {
		return Task{}, errors.New("invalid task metadata patch")
	}
	if declared != nil && (!slices.Equal(declared.Blockers, blockers) || !slices.Equal(declared.Related, related)) {
		return Task{}, errors.New("explicit body DEPENDENCIES must match the patch")
	}
	row := map[string]any{}
	for key, cells := range map[string]map[string]any{
		"title":       {"标题": title},
		"description": {"要做": brief.Todo, "CHECK": brief.Check, "SCOPE": brief.Scope, "正文": brief.Body},
		"estimate":    {"估算": estimate},
	} {
		if _, ok := patch[key]; ok {
			for name, v := range cells {
				row[name] = v
			}
		}
	}
	if err = c.dependencyCells(ctx, row, t.ID, brief, blockers, related); err != nil {
		return Task{}, err
	}
	if _, ok := patch["blockers"]; !ok {
		delete(row, "阻塞于")
	}
	if _, ok := patch["related"]; !ok {
		delete(row, "相关")
	}
	if err = c.updateTicket(ctx, t.nativeID, row); err != nil {
		return Task{}, err
	}
	brief.Blockers, brief.Related = blockers, related
	return c.awaitTask(ctx, t.ID, func(t Task) bool {
		return t.Title == title && sameEstimate(t.Estimate, estimate) && t.Metadata.Complete && !t.Held && t.Description == brief.Description()
	})
}

// SetMetadata writes state, estimate and dependencies, adopting an unmanaged
// task without requiring a brief, as the GitHub backend does. Fields the Base
// does not store must stay empty.
func (c *Feishu) SetMetadata(ctx context.Context, id string, m Metadata) (Task, error) {
	t, err := c.Read(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if m.ID != t.ID || m.Identifier != t.Metadata.Identifier || m.Project != c.repo {
		return Task{}, errors.New("canonical identity is immutable")
	}
	if t.Held {
		return Task{}, errors.New("held task requires adopt before metadata changes")
	}
	if m.ImportDigest != "" || m.Parent != "" || m.Priority != 0 || m.ImportHold || !m.CreatedAt.IsZero() || !m.UpdatedAt.IsZero() {
		return Task{}, errors.New("feishu backend stores no import provenance, parent or priority")
	}
	if !m.Complete || !validState(m.State) || m.Blockers == nil || m.Related == nil || hasDuplicateStrings(m.Blockers) || hasDuplicateStrings(m.Related) || m.Estimate != nil && *m.Estimate < 0 {
		return Task{}, errors.New("invalid or incomplete task metadata")
	}
	current, err := c.ticketRow(ctx, t.nativeID)
	if err != nil {
		return Task{}, err
	}
	state := feishuState(m.State)
	row := map[string]any{"状态": []string{state}, "估算": m.Estimate}
	if current.canonical == "" {
		row["规范ID"] = t.ID
	}
	if err = c.dependencyCells(ctx, row, t.ID, current.brief, m.Blockers, m.Related); err != nil {
		return Task{}, err
	}
	if err = c.updateTicket(ctx, t.nativeID, row); err != nil {
		return Task{}, err
	}
	return c.awaitTask(ctx, t.ID, func(t Task) bool {
		return t.State.Name == state && sameEstimate(t.Estimate, m.Estimate) && t.Metadata.Complete && !t.Held && slices.Equal(t.Metadata.Blockers, m.Blockers) && slices.Equal(t.Metadata.Related, m.Related)
	})
}
