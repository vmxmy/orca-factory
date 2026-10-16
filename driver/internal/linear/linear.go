// Package linear holds the issue and comment shapes shared by the issue backends.
package linear

import "time"

type NamedID struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type IssueRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
}

// InverseRelation is a relation whose source is another issue; a `blocks`
// inverse relation means that issue blocks this one.
type InverseRelation struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Issue struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		State      struct {
			Type string `json:"type"`
		} `json:"state"`
	} `json:"issue"`
}
type Relation struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	RelatedIssue IssueRef `json:"relatedIssue"`
}
type PageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}
type Connection[T any] struct {
	Nodes    []T      `json:"nodes"`
	PageInfo PageInfo `json:"pageInfo"`
}
type Issue struct {
	Priority   int                  `json:"priority"`
	CreatedAt  time.Time            `json:"createdAt"`
	Project    NamedID              `json:"project"`
	ID         string               `json:"id"`
	Identifier string               `json:"identifier"`
	State      NamedID              `json:"state"`
	Labels     Connection[NamedID]  `json:"labels"`
	Relations  Connection[Relation] `json:"relations"`
	UpdatedAt  time.Time            `json:"updatedAt"`
	// URL, Held, Title and Number are backend projections, never JSON fields.
	URL    string `json:"-"`
	Held   bool   `json:"-"`
	Title  string `json:"-"`
	Number int    `json:"-"`

	InverseRelations Connection[InverseRelation] `json:"inverseRelations"`
}

// Blocked reports whether any blocker is unresolved. A truncated blocker list
// counts as blocked so that a partial read can never admit a blocked ticket.
func (i Issue) Blocked() bool {
	if i.InverseRelations.PageInfo.HasNextPage {
		return true
	}
	for _, r := range i.InverseRelations.Nodes {
		if r.Type != "blocks" {
			continue
		}
		switch r.Issue.State.Type {
		case "completed", "canceled", "duplicate":
		default:
			return true
		}
	}
	return false
}

type Comment struct {
	Source    string    `json:"source,omitempty"`
	User      *NamedID  `json:"user,omitempty"`
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
