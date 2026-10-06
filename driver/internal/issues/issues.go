// Package issues selects exactly one task backend per repository. Legacy model
// types remain shared with Linear so shipped executor state retains its identity.
package issues

import (
	"context"
	"crypto/rand"
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
	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

type Client interface {
	Snapshot(context.Context, []string, time.Time) ([]linear.Issue, error)
	ListComments(context.Context, string) ([]linear.Comment, error)
	UpdateIssue(context.Context, string, string, []string, []string) (linear.Issue, error)
	CreateComment(context.Context, string, string) (linear.Comment, error)
}

func Backend(env map[string]string) (string, error) {
	switch env["AGENT_ISSUE_BACKEND"] {
	case "", "linear":
		return "linear", nil
	case "github":
		return "github", nil
	default:
		return "", errors.New("unknown AGENT_ISSUE_BACKEND")
	}
}
func Project(env map[string]string) string {
	if env["AGENT_ISSUE_BACKEND"] == "github" {
		return env["AGENT_GH_REPO"]
	}
	return env["AGENT_LINEAR_PROJECT"]
}
func New(ctx context.Context, env map[string]string, tuning config.Tuning) (Client, error) {
	b, err := Backend(env)
	if err != nil {
		return nil, err
	}
	if b == "linear" {
		return linear.New(ctx, linear.Options{TeamID: env["AGENT_LINEAR_TEAM"], HTTPTimeout: tuning.Linear.HTTPTimeout})
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
	linear.Issue
	Number      int                               `json:"number"`
	URL         string                            `json:"url"`
	Title       string                            `json:"title"`
	Description string                            `json:"description"`
	Estimate    *float64                          `json:"estimate"`
	Comments    linear.Connection[linear.Comment] `json:"comments"`
	Metadata    Metadata                          `json:"metadata"`
	Complete    bool                              `json:"complete"`
	BlockedBy   []linear.InverseRelation          `json:"blockedBy"`
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
	comments          map[string]commentHistory
	commentGeneration uint64
}

var component = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const marker = "<!-- orca-task-v1: "

func NewGitHub(repo string, gh *github.Client) (*GitHub, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !component.MatchString(owner) || !component.MatchString(name) || gh == nil {
		return nil, errors.New("invalid GitHub task repository")
	}
	return &GitHub{repo: repo, owner: owner, name: name, gh: gh}, nil
}
func (c *GitHub) Repository() string { return c.repo }
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
	if json.Unmarshal([]byte(raw), &fields) != nil || fields["blockers"] == nil || fields["related"] == nil {
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
	if err != nil {
		return Task{}, err
	}
	if m.Project != c.repo {
		return Task{}, errors.New("task metadata repository mismatch")
	}
	nativeUpdatedAt := v.UpdatedAt
	if !m.CreatedAt.IsZero() {
		v.CreatedAt = m.CreatedAt
	}
	if !m.UpdatedAt.IsZero() {
		v.UpdatedAt = m.UpdatedAt
	}
	effectiveIdentifier := m.Identifier
	if effectiveIdentifier == "" {
		effectiveIdentifier = "GH-" + strconv.Itoa(v.Number)
	}
	state := m.State
	// Native closure is authoritative for cancellation, while an inconsistent reopen
	// is held. Neither can manufacture a runnable Todo from stale metadata.
	incomplete := m.ImportHold
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
	labels := linear.Connection[linear.NamedID]{Nodes: []linear.NamedID{}}
	for _, l := range v.Labels {
		labels.Nodes = append(labels.Nodes, linear.NamedID{ID: l.Name, Name: l.Name})
	}
	return Task{NativeState: v.State, NativeStateReason: v.StateReason, NativeUpdatedAt: nativeUpdatedAt, Issue: linear.Issue{ID: m.ID, Identifier: effectiveIdentifier, Project: linear.NamedID{ID: c.repo, Name: c.repo}, State: linear.NamedID{ID: stateType(state), Name: state}, Priority: m.Priority, Labels: labels, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, Relations: linear.Connection[linear.Relation]{Nodes: []linear.Relation{}}, InverseRelations: linear.Connection[linear.InverseRelation]{Nodes: []linear.InverseRelation{}, PageInfo: linear.PageInfo{HasNextPage: incomplete}}}, Number: v.Number, URL: v.URL, Title: v.Title, Description: strings.TrimSuffix(strings.SplitN(v.Body, marker, 2)[0], "\n\n"), Estimate: m.Estimate, Metadata: m, Complete: !incomplete, Comments: linear.Connection[linear.Comment]{Nodes: []linear.Comment{}}, BlockedBy: []linear.InverseRelation{}}, nil
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
		if !strings.Contains(v.Body, marker) {
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
			r := linear.InverseRelation{ID: id, Type: "blocks"}
			r.Issue.ID = id
			r.Issue.Identifier = id
			if n, ok := ids[id]; ok {
				r.Issue.Identifier = rows[n].Identifier
				r.Issue.State.Type = stateType(rows[n].State.Name)
			} else {
				t.InverseRelations.PageInfo.HasNextPage = true
				t.Complete = false
			}
			t.InverseRelations.Nodes = append(t.InverseRelations.Nodes, r)
			t.BlockedBy = append(t.BlockedBy, r)
		}
		for _, id := range t.Metadata.Related {
			r := linear.Relation{ID: id, Type: "related", RelatedIssue: linear.IssueRef{ID: id}}
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
func (c *GitHub) Snapshot(ctx context.Context, projects []string, _ time.Time) ([]linear.Issue, error) {
	for _, p := range projects {
		if p != c.repo {
			return nil, errors.New("snapshot repository mismatch")
		}
	}
	tasks, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]linear.Issue, 0, len(tasks))
	for _, t := range tasks {
		if t.Metadata.ImportHold {
			continue
		}
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
func (c *GitHub) ListComments(ctx context.Context, id string) ([]linear.Comment, error) {
	t, err := c.cachedTask(ctx, id)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	generation := c.commentGeneration
	c.mu.Unlock()
	raw, err := c.gh.IssueComments(ctx, c.owner, c.name, t.Number)
	if err != nil {
		return nil, err
	}
	rows := []linear.Comment{}
	for _, v := range raw {
		row := linear.Comment{ID: strconv.FormatInt(v.ID, 10), Body: v.Body, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
		decoded, decodeErr := DecodeImportedComments(row)
		err = decodeErr
		if err != nil {
			return nil, err
		}
		rows = append(rows, decoded...)
	}
	c.mu.Lock()
	if c.comments == nil {
		c.comments = map[string]commentHistory{}
	}
	if generation == c.commentGeneration {
		c.comments[t.ID] = commentHistory{readAt: time.Now(), version: t.NativeUpdatedAt, rows: append([]linear.Comment(nil), rows...)}
	}
	c.mu.Unlock()
	return rows, nil
}
func (c *GitHub) CreateComment(ctx context.Context, id, body string) (linear.Comment, error) {
	c.invalidateComments()
	defer c.invalidateComments()
	t, err := c.Read(ctx, id)
	if err != nil {
		return linear.Comment{}, err
	}
	v, err := c.gh.AddIssueComment(ctx, c.owner, c.name, t.Number, body)
	return linear.Comment{ID: strconv.FormatInt(v.ID, 10), Body: v.Body, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}, err
}
func (c *GitHub) SetMetadata(ctx context.Context, id string, m Metadata) (Task, error) {
	c.invalidateComments()
	defer c.invalidateComments()
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
func (c *GitHub) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (linear.Issue, error) {
	c.invalidateComments()
	defer c.invalidateComments()
	t, err := c.Read(ctx, id)
	if err != nil {
		return linear.Issue{}, err
	}
	m := t.Metadata
	if state != "" {
		if !validState(state) {
			return linear.Issue{}, errors.New("unknown task state")
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
		return linear.Issue{}, err
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
		return linear.Issue{}, err
	}
	result, err := c.task(v)
	return result.Issue, err
}
func (c *GitHub) Create(ctx context.Context, in CreateInput) (Task, error) {
	if in.ID == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return Task{}, err
		}
		b[6] = (b[6] & 15) | 64
		b[8] = (b[8] & 63) | 128
		in.ID = fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
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
	var t Task
	if err != nil {
		if in.ImportDigest != "" {
			return Task{}, err
		}
		recovered, readErr := c.Read(ctx, in.ID)
		if readErr != nil {
			return Task{}, fmt.Errorf("create unconfirmed for canonical ID %s: %w", in.ID, err)
		}
		t = recovered
	} else {
		t, err = c.task(v)
		if err != nil {
			return Task{}, err
		}
	}
	if stateType(in.State) == "completed" || stateType(in.State) == "canceled" || stateType(in.State) == "duplicate" {
		if _, err = c.UpdateIssue(ctx, t.ID, in.State, nil, nil); err != nil {
			return Task{}, err
		}
		return c.Read(ctx, t.ID)
	}
	return t, nil
}

// GitHubUsage is additive to the legacy usage contract and never probes Linear.
func (c *GitHub) GitHubUsage() github.Usage { return c.gh.Usage() }

type commentHistory struct {
	readAt  time.Time
	version time.Time
	rows    []linear.Comment
}

func (c *GitHub) invalidateComments() {
	c.mu.Lock()
	c.comments = nil
	c.commentGeneration++
	c.mu.Unlock()
}

// SnapshotComments reuses a fully read logical history only when the latest
// complete issue enumeration confirms its native updated_at has not changed.
// Recovery and ambiguous-write readback use ListComments and always revalidate.
func (c *GitHub) SnapshotComments(ctx context.Context, id string) ([]linear.Comment, error) {
	t, err := c.cachedTask(ctx, id)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	h, ok := c.comments[t.ID]
	c.mu.Unlock()
	if ok && time.Since(h.readAt) < 5*time.Minute && !t.NativeUpdatedAt.IsZero() && h.version.Equal(t.NativeUpdatedAt) {
		return append([]linear.Comment(nil), h.rows...), nil
	}
	return c.ListComments(ctx, id)
}
