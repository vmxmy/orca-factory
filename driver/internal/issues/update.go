package issues

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Edit changes only explicitly supplied task fields. Frozen identity and import
// provenance are not editable through this public patch operation.
func (c *GitHub) Edit(ctx context.Context, id string, raw []byte) (Task, error) {
	c.invalidateComments()
	defer c.invalidateComments()
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
	m := t.Metadata
	for k, v := range patch {
		var target any
		switch k {
		case "title":
			target = &t.Title
		case "description":
			target = &t.Description
		case "estimate":
			target = &m.Estimate
		case "priority":
			target = &m.Priority
		case "blockers":
			target = &m.Blockers
		case "related":
			target = &m.Related
		case "parent":
			target = &m.Parent
		default:
			return Task{}, errors.New("unknown or immutable task patch field")
		}
		if json.Unmarshal(v, target) != nil {
			return Task{}, errors.New("invalid task patch field")
		}
	}
	if m.Priority < 0 || m.Priority > 4 || m.Estimate != nil && *m.Estimate < 0 || m.Blockers == nil || m.Related == nil {
		return Task{}, errors.New("invalid task metadata patch")
	}
	for _, ref := range append(append([]string{}, m.Blockers...), m.Related...) {
		dependency, readErr := c.Read(ctx, ref)
		if readErr != nil || dependency.ID != ref || dependency.ID == t.ID || !dependency.Metadata.Complete {
			return Task{}, errors.New("patch dependency is not a complete canonical task")
		}
	}
	if strings.TrimSpace(t.Title) == "" {
		return Task{}, errors.New("title required")
	}
	body, err := render(t.Description, m)
	if err != nil {
		return Task{}, err
	}
	v, err := c.gh.EditIssue(ctx, c.owner, c.name, t.Number, map[string]any{"title": t.Title, "body": body})
	if err != nil {
		return Task{}, err
	}
	return c.task(v)
}
