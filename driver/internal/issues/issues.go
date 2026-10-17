// Package issues selects exactly one task backend per repository. The shared
// model types keep their historical shape so shipped executor state retains its identity.
package issues

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// Store is a backend that keeps canonical task records with driver metadata.
// Callers depend on it instead of a concrete backend.
type Store interface {
	Snapshot(context.Context, []string, time.Time) ([]task.Issue, error)
	ListComments(context.Context, string) ([]task.Comment, error)
	UpdateIssue(context.Context, string, string, []string, []string) (task.Issue, error)
	CreateComment(context.Context, string, string) (task.Comment, error)
	Repository() string
	List(context.Context) ([]Task, error)
	Read(context.Context, string) (Task, error)
	Description(context.Context, string) (string, error)
	FreshIssue(context.Context, string) (task.Issue, error)
	Create(context.Context, CreateInput) (Task, error)
	Adopt(context.Context, string, []byte) (Task, error)
	Edit(context.Context, string, []byte) (Task, error)
	SetMetadata(context.Context, string, Metadata) (Task, error)
}

// Importer is a Store that can create records carrying an imported identity.
type Importer interface {
	Store
	CreateImported(context.Context, CreateInput) (Task, error)
	RawComments(context.Context, Task) ([]string, error)
}

var _ Importer = (*GitHub)(nil)

func Backend(env map[string]string) (string, error) {
	switch backend := env["AGENT_ISSUE_BACKEND"]; backend {
	case "github", "feishu":
		return backend, nil
	default:
		return "", errors.New("AGENT_ISSUE_BACKEND must be github or feishu")
	}
}
func Project(env map[string]string) string {
	backend, _ := Backend(env)
	switch backend {
	case "github":
		return env["AGENT_GH_REPO"]
	case "feishu":
		return env["AGENT_FEISHU_PROJECT"]
	}
	return ""
}
func New(ctx context.Context, env map[string]string, tuning config.Tuning) (Store, error) {
	b, err := Backend(env)
	if err != nil {
		return nil, err
	}
	if b == "feishu" {
		return newFeishuFromEnv(ctx, env)
	}
	gh, err := github.New(github.Options{BaseURL: env["AGENT_GH_API_URL"], HTTPTimeout: tuning.GitHub.HTTPTimeout})
	if err != nil {
		return nil, err
	}
	return NewGitHub(env["AGENT_GH_REPO"], gh)
}

type Metadata struct {
	ImportDigest string    `json:"importDigest,omitempty"`
	CreatedAt    time.Time `json:"createdAt,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
	Parent       string    `json:"parent,omitempty"`
	ID           string    `json:"id"`
	Identifier   string    `json:"identifier"`
	Project      string    `json:"project"`
	State        string    `json:"state"`
	Priority     int       `json:"priority"`
	Estimate     *float64  `json:"estimate,omitempty"`
	Blockers     []string  `json:"blockers"`
	Related      []string  `json:"related"`
	Complete     bool      `json:"complete"`
	ImportHold   bool      `json:"importHold,omitempty"`
}
type Task struct {
	NativeState       string    `json:"nativeState"`
	NativeStateReason string    `json:"nativeStateReason"`
	NativeUpdatedAt   time.Time `json:"nativeUpdatedAt"`
	nativeID          string
	task.Issue
	Number      int                           `json:"number"`
	URL         string                        `json:"url"`
	Title       string                        `json:"title"`
	Description string                        `json:"description"`
	Estimate    *float64                      `json:"estimate"`
	Comments    task.Connection[task.Comment] `json:"comments"`
	Metadata    Metadata                      `json:"metadata"`
	Complete    bool                          `json:"complete"`
	BlockedBy   []task.InverseRelation        `json:"blockedBy"`
}
type CreateInput struct {
	ImportDigest string    `json:"importDigest,omitempty"`
	CreatedAt    time.Time `json:"createdAt,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
	Parent       string    `json:"parent,omitempty"`
	ID           string    `json:"id"`
	Identifier   string    `json:"identifier"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	State        string    `json:"state"`
	Labels       []string  `json:"labels"`
	Priority     int       `json:"priority"`
	Estimate     *float64  `json:"estimate"`
	Blockers     []string  `json:"blockers"`
	Related      []string  `json:"related"`
	ImportHold   bool      `json:"importHold"`
}
type GitHub struct {
	repo, owner, name string
	gh                *github.Client
	mu                sync.Mutex
	cached            map[string]Task
}

var component = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// invalidCheckRed matches CHECK failures caused by the command itself rather
// than an assertion: such a CHECK stays red after the fix, so the gate can
// never pass (ZY-92).
var invalidCheckRed = regexp.MustCompile(`(?i)unknown option|\busage\b|no test files found|command not found|(^|[^0-9])0 tests|no tests to run`)

// requireValidCheckRed refuses a ticket whose intake self-check records an
// invalid red reason. Only the recorded reason is inspected; CHECK never runs here.
func requireValidCheckRed(description string) error {
	for _, line := range strings.Split(description, "\n") {
		reason, ok := strings.CutPrefix(strings.TrimSpace(line), "- CHECK 红因")
		if ok && invalidCheckRed.MatchString(reason) {
			return fmt.Errorf("CHECK 红因 is not an assertion failure, rewrite CHECK and rerun it on main: %s", strings.TrimLeft(reason, "：: "))
		}
	}
	return nil
}

const marker = "<!-- orca-task-v1: "

func NewGitHub(repo string, gh *github.Client) (*GitHub, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !component.MatchString(owner) || !component.MatchString(name) || gh == nil {
		return nil, errors.New("invalid GitHub task repository")
	}
	return &GitHub{repo: repo, owner: owner, name: name, gh: gh}, nil
}
func (c *GitHub) Repository() string           { return c.repo }
func (c *GitHub) Usage() github.Usage          { return c.gh.Usage() }
func DecodeInput(raw []byte, target any) error { return decodeImportJSON(raw, target) }
func ParseMetadata(body string) (Metadata, error) {
	var m Metadata
	if strings.Count(body, marker) != 1 {
		return m, errors.New("missing or duplicate task metadata")
	}
	tail := strings.SplitN(body, marker, 2)[1]
	raw, _, ok := strings.Cut(tail, " -->")
	if !ok || decodeImportJSON([]byte(raw), &m) != nil || !uuid.MatchString(m.ID) || (m.Identifier != "" && !component.MatchString(m.Identifier)) || (m.Identifier == "" && m.ImportDigest != "") || !m.Complete || m.Project == "" || !validState(m.State) {
		return m, errors.New("invalid or incomplete task metadata")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || fields["blockers"] == nil || fields["related"] == nil || m.Blockers == nil || m.Related == nil {
		return m, errors.New("dependency metadata is incomplete")
	}
	return m, nil
}
func validState(s string) bool {
	switch s {
	case "Todo", "Backlog", "In Progress", "Done", "Canceled", "Cancelled", "Duplicate":
		return true
	}
	return false
}
func stateType(s string) string {
	switch s {
	case "Done":
		return "completed"
	case "Canceled", "Cancelled":
		return "canceled"
	case "Duplicate":
		return "duplicate"
	case "In Progress":
		return "started"
	case "Backlog":
		return "backlog"
	default:
		return "unstarted"
	}
}
func render(description string, m Metadata) (string, error) {
	if strings.Contains(description, marker) {
		return "", errors.New("description contains reserved task metadata")
	}
	b, err := json.Marshal(m)
	return description + "\n\n" + marker + string(b) + " -->", err
}
func (c *GitHub) task(v github.IssueRecord) (Task, error) {
	m, err := ParseMetadata(v.Body)
	var nativeID string
	if v.NodeID != "" {
		sum := sha256.Sum256([]byte("github-node:" + v.NodeID))
		nativeID = fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	}
	managed := strings.Contains(v.Body, marker)
	if !managed {
		if v.NodeID == "" {
			return Task{}, errors.New("native issue node ID missing")
		}
		m = Metadata{ID: nativeID, Identifier: "GH-" + strconv.Itoa(v.Number), Project: c.repo, State: "Todo", Blockers: []string{}, Related: []string{}}
	} else if err != nil {
		return Task{}, err
	}
	if m.Project != c.repo {
		return Task{}, errors.New("task metadata repository mismatch")
	}
	nativeUpdatedAt := v.UpdatedAt
	if !m.CreatedAt.IsZero() {
		v.CreatedAt = m.CreatedAt
	}
	effectiveIdentifier := m.Identifier
	if effectiveIdentifier == "" {
		effectiveIdentifier = "GH-" + strconv.Itoa(v.Number)
	}
	state := m.State
	// Native closure is authoritative for cancellation, while an inconsistent reopen
	// is held. Neither can manufacture a runnable Todo from stale metadata.
	incomplete := m.ImportHold || !managed
	if v.State == "closed" {
		if v.StateReason == "not_planned" {
			if state != "Cancelled" && state != "Duplicate" {
				state = "Canceled"
			}
		} else {
			state = "Done"
		}
	} else if stateType(state) == "completed" || stateType(state) == "canceled" || stateType(state) == "duplicate" {
		incomplete = true
	}
	labels := task.Connection[task.NamedID]{Nodes: []task.NamedID{}}
	for _, l := range v.Labels {
		labels.Nodes = append(labels.Nodes, task.NamedID{ID: l.Name, Name: l.Name})
	}
	return Task{nativeID: nativeID, NativeState: v.State, NativeStateReason: v.StateReason, NativeUpdatedAt: nativeUpdatedAt, Issue: task.Issue{ID: m.ID, Identifier: effectiveIdentifier, Project: task.NamedID{ID: c.repo, Name: c.repo}, State: task.NamedID{ID: stateType(state), Name: state}, Priority: m.Priority, Labels: labels, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, URL: v.URL, Held: m.ImportHold, Title: v.Title, Number: v.Number, Relations: task.Connection[task.Relation]{Nodes: []task.Relation{}}, InverseRelations: task.Connection[task.InverseRelation]{Nodes: []task.InverseRelation{}, PageInfo: task.PageInfo{HasNextPage: incomplete}}}, Number: v.Number, URL: v.URL, Title: v.Title, Description: strings.TrimSuffix(strings.SplitN(v.Body, marker, 2)[0], "\n\n"), Estimate: m.Estimate, Metadata: m, Complete: !incomplete, Comments: task.Connection[task.Comment]{Nodes: []task.Comment{}}, BlockedBy: []task.InverseRelation{}}, nil
}
func (c *GitHub) List(ctx context.Context) ([]Task, error) {
	raw, err := c.gh.Issues(ctx, c.owner, c.name)
	if err != nil {
		return nil, err
	}
	rows := []Task{}
	ids := map[string]int{}
	names := map[string]bool{}
	for _, v := range raw {
		if len(v.PullRequest) > 0 && string(v.PullRequest) != "null" {
			continue
		}
		t, err := c.task(v)
		if err != nil {
			return nil, err
		}
		if _, ok := ids[t.ID]; ok || names[t.Identifier] {
			return nil, errors.New("duplicate canonical task identity")
		}
		ids[t.ID] = len(rows)
		names[t.Identifier] = true
		rows = append(rows, t)
	}
	for i := range rows {
		t := &rows[i]
		if t.Metadata.Parent != "" {
			if _, ok := ids[t.Metadata.Parent]; !ok {
				t.Complete = false
				t.InverseRelations.PageInfo.HasNextPage = true
			}
		}
		for _, id := range t.Metadata.Blockers {
			r := task.InverseRelation{ID: id, Type: "blocks"}
			r.Issue.ID = id
			r.Issue.Identifier = id
			if n, ok := ids[id]; ok {
				r.Issue.Identifier = rows[n].Identifier
				r.Issue.State.Type = stateType(rows[n].State.Name)
				// Only native closure resolves a blocker; a reopened or held task
				// keeps stale completed metadata.
				if rows[n].NativeState != "closed" {
					r.Issue.State.Type = "started"
				}
			} else {
				t.InverseRelations.PageInfo.HasNextPage = true
				t.Complete = false
			}
			t.InverseRelations.Nodes = append(t.InverseRelations.Nodes, r)
			t.BlockedBy = append(t.BlockedBy, r)
		}
		for _, id := range t.Metadata.Related {
			r := task.Relation{ID: id, Type: "related", RelatedIssue: task.IssueRef{ID: id}}
			if n, ok := ids[id]; ok {
				r.RelatedIssue.Identifier = rows[n].Identifier
			} else {
				t.Relations.PageInfo.HasNextPage = true
				t.InverseRelations.PageInfo.HasNextPage = true
				t.Complete = false
			}
			t.Relations.Nodes = append(t.Relations.Nodes, r)
		}
	}
	c.mu.Lock()
	c.cached = map[string]Task{}
	for _, v := range rows {
		c.cached[v.ID] = v
		c.cached[v.Identifier] = v
		c.cached[strconv.Itoa(v.Number)] = v
	}
	c.mu.Unlock()
	return rows, nil
}
func (c *GitHub) cachedTask(ctx context.Context, id string) (Task, error) {
	c.mu.Lock()
	t, ok := c.cached[id]
	c.mu.Unlock()
	if ok {
		return t, nil
	}
	return c.Read(ctx, id)
}
func (c *GitHub) Description(ctx context.Context, id string) (string, error) {
	t, err := c.cachedTask(ctx, id)
	return t.Description, err
}
func (c *GitHub) Snapshot(ctx context.Context, projects []string, _ time.Time) ([]task.Issue, error) {
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
func (c *GitHub) Read(ctx context.Context, id string) (Task, error) {
	rows, err := c.List(ctx)
	if err != nil {
		return Task{}, err
	}
	for _, v := range rows {
		if v.ID == id || v.Identifier == id || strconv.Itoa(v.Number) == id {
			return v, nil
		}
	}
	return Task{}, errors.New("task not found")
}
func (c *GitHub) ListComments(ctx context.Context, id string) ([]task.Comment, error) {
	t, err := c.cachedTask(ctx, id)
	if err != nil {
		return nil, err
	}
	raw, err := c.gh.IssueComments(ctx, c.owner, c.name, t.Number)
	if err != nil {
		return nil, err
	}
	history := make([]task.Comment, 0, len(raw))
	for _, v := range raw {
		history = append(history, task.Comment{ID: strconv.FormatInt(v.ID, 10), Body: v.Body, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt})
	}
	return decodeImportedHistory(history)
}
func (c *GitHub) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	t, err := c.Read(ctx, id)
	if err != nil {
		return task.Comment{}, err
	}
	v, err := c.gh.AddIssueComment(ctx, c.owner, c.name, t.Number, body)
	return task.Comment{ID: strconv.FormatInt(v.ID, 10), Body: v.Body, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}, err
}
func (c *GitHub) SetMetadata(ctx context.Context, id string, m Metadata) (Task, error) {
	t, err := c.Read(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if m.ID != t.ID || m.Identifier != t.Metadata.Identifier || m.Project != c.repo {
		return Task{}, errors.New("canonical identity is immutable")
	}
	body, err := render(t.Description, m)
	if err != nil {
		return Task{}, err
	}
	if _, err = ParseMetadata(body); err != nil {
		return Task{}, err
	}
	v, err := c.gh.EditIssue(ctx, c.owner, c.name, t.Number, map[string]any{"body": body})
	if err != nil {
		return Task{}, err
	}
	return c.task(v)
}
func (c *GitHub) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (task.Issue, error) {
	t, err := c.Read(ctx, id)
	if err != nil {
		return task.Issue{}, err
	}
	if !t.Metadata.Complete {
		return task.Issue{}, errors.New("explicit adoption required before update")
	}
	m := t.Metadata
	if state != "" {
		if !validState(state) {
			return task.Issue{}, errors.New("unknown task state")
		}
		m.State = state
	}
	labels := map[string]bool{}
	for _, l := range t.Labels.Nodes {
		labels[l.Name] = true
	}
	for _, l := range add {
		labels[l] = true
	}
	for _, l := range remove {
		delete(labels, l)
	}
	names := []string{}
	for l := range labels {
		names = append(names, l)
	}
	sort.Strings(names)
	body, err := render(t.Description, m)
	if err != nil {
		return task.Issue{}, err
	}
	input := map[string]any{"body": body, "labels": names}
	if state != "" {
		input["state"] = "open"
		if stateType(state) == "completed" || stateType(state) == "canceled" || stateType(state) == "duplicate" {
			input["state"] = "closed"
			input["state_reason"] = "completed"
			if state != "Done" {
				input["state_reason"] = "not_planned"
			}
		}
	}
	v, err := c.gh.EditIssue(ctx, c.owner, c.name, t.Number, input)
	if err != nil {
		return task.Issue{}, err
	}
	result, err := c.task(v)
	return result.Issue, err
}
func (c *GitHub) Create(ctx context.Context, in CreateInput) (Task, error) {
	if in.ID != "" || in.Identifier != "" || in.ImportDigest != "" || !in.CreatedAt.IsZero() || !in.UpdatedAt.IsZero() || in.ImportHold {
		return Task{}, errors.New("native create cannot set imported identity, provenance or hold")
	}
	if strings.TrimSpace(in.Title) == "" || strings.Contains(in.Description, marker) {
		return Task{}, errors.New("invalid native create input")
	}
	if err := requireValidCheckRed(in.Description); err != nil {
		return Task{}, err
	}
	if (in.Blockers == nil || in.Related == nil) && in.State != "" && in.State != "Todo" {
		return Task{}, errors.New("explicit adoption required for native create state")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return Task{}, err
	}
	createMarker := fmt.Sprintf("<!-- orca-native-create-v1: %x -->", nonce)
	description := in.Description + "\n\n" + createMarker
	v, err := c.gh.CreateIssue(ctx, c.owner, c.name, map[string]any{"title": in.Title, "body": description, "labels": in.Labels})
	if err != nil {
		rows, readErr := c.gh.Issues(ctx, c.owner, c.name)
		found := false
		if readErr == nil {
			for _, row := range rows {
				if strings.Contains(row.Body, createMarker) {
					if found {
						return Task{}, errors.New("ambiguous native create marker")
					}
					v = row
					found = true
				}
			}
		}
		if !found {
			return Task{}, fmt.Errorf("native create unconfirmed; inspect marker %s before retry: %w", createMarker, err)
		}
	}
	task, err := c.task(v)
	if err != nil {
		return Task{}, err
	}
	if in.Blockers == nil || in.Related == nil {
		return task, nil
	}
	if err := c.awaitListed(ctx, task.ID); err != nil {
		return Task{}, fmt.Errorf("native issue %s remains unmanaged: %w", task.Identifier, err)
	}
	raw, err := json.Marshal(AdoptInput{Blockers: in.Blockers, Related: in.Related})
	if err != nil {
		return Task{}, err
	}
	adopted, err := c.Adopt(ctx, task.ID, raw)
	if err != nil {
		return Task{}, fmt.Errorf("native issue %s remains unmanaged: %w", task.Identifier, err)
	}
	m := adopted.Metadata
	m.Priority = in.Priority
	m.Estimate = in.Estimate
	m.ImportHold = in.ImportHold
	if _, err = c.SetMetadata(ctx, task.ID, m); err != nil {
		return Task{}, err
	}
	if in.State != "" && in.State != "Todo" {
		if _, err = c.UpdateIssue(ctx, task.ID, in.State, nil, nil); err != nil {
			return Task{}, err
		}
	}
	return c.Read(ctx, task.ID)
}

// RawComments returns stored comment bodies without driver parsing.
func (c *GitHub) RawComments(ctx context.Context, t Task) ([]string, error) {
	rows, err := c.gh.IssueComments(ctx, c.owner, c.name, t.Number)
	if err != nil {
		return nil, err
	}
	bodies := make([]string, len(rows))
	for i, row := range rows {
		bodies[i] = row.Body
	}
	return bodies, nil
}

func (c *GitHub) CreateImported(ctx context.Context, in CreateInput) (Task, error) {
	if in.ID == "" || in.ImportDigest == "" || !in.ImportHold {
		return Task{}, errors.New("validated held import identity required")
	}
	if in.State == "" {
		in.State = "Todo"
	}
	if strings.TrimSpace(in.Title) == "" {
		return Task{}, errors.New("title required")
	}
	m := Metadata{ImportDigest: in.ImportDigest, CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt, Parent: in.Parent, ID: in.ID, Identifier: in.Identifier, Project: c.repo, State: in.State, Priority: in.Priority, Estimate: in.Estimate, Blockers: in.Blockers, Related: in.Related, Complete: true, ImportHold: in.ImportHold}
	body, err := render(in.Description, m)
	if err != nil {
		return Task{}, err
	}
	if _, err = ParseMetadata(body); err != nil {
		return Task{}, err
	}
	rows, err := c.List(ctx)
	if err != nil {
		return Task{}, err
	}
	for _, v := range rows {
		if v.ID == in.ID || v.Identifier == in.Identifier {
			return Task{}, errors.New("task identity already exists")
		}
	}
	v, err := c.gh.CreateIssue(ctx, c.owner, c.name, map[string]any{"title": in.Title, "body": body, "labels": in.Labels})
	if err != nil {
		return Task{}, err
	}
	t, err := c.task(v)
	if err != nil {
		return Task{}, err
	}
	if err = c.awaitListed(ctx, t.ID); err != nil {
		return Task{}, err
	}
	if stateType(in.State) == "completed" || stateType(in.State) == "canceled" || stateType(in.State) == "duplicate" {
		if _, err = c.UpdateIssue(ctx, t.ID, in.State, nil, nil); err != nil {
			return Task{}, err
		}
		return c.Read(ctx, t.ID)
	}
	return t, nil
}

// GitHub's issue list lags a moment behind creation, and every later write
// resolves canonical IDs through that list.
func (c *GitHub) awaitListed(ctx context.Context, id string) error {
	for attempt := 0; attempt < 60; attempt++ {
		rows, err := c.List(ctx)
		if err != nil {
			return err
		}
		for _, v := range rows {
			if v.ID == id {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("created task not listed by GitHub within bounded retries: %w", infra.ErrWriteUnconfirmed)
}

// GitHubUsage reports the GitHub client's rate-limit usage.
func (c *GitHub) GitHubUsage() github.Usage { return c.gh.Usage() }

// Comment edits need not change the issue timestamp; each page revalidates its ETag.
func (c *GitHub) SnapshotComments(ctx context.Context, id string) ([]task.Comment, error) {
	return c.ListComments(ctx, id)
}
