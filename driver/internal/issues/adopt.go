package issues

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type AdoptInput struct {
	Blockers []string `json:"blockers"`
	Related  []string `json:"related"`
}

func (c *GitHub) Adopt(ctx context.Context, id string, raw []byte) (Task, error) {
	var in AdoptInput
	if err := decodeImportJSON(raw, &in); err != nil {
		return Task{}, err
	}
	if in.Blockers == nil || in.Related == nil || hasDuplicateStrings(in.Blockers) || hasDuplicateStrings(in.Related) {
		return Task{}, errors.New("explicit complete dependency lists required")
	}
	task, err := c.Read(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if task.Metadata.Complete {
		return Task{}, errors.New("task already managed")
	}
	if task.NativeState != "open" {
		return Task{}, errors.New("closed task cannot be adopted")
	}
	for _, field := range []string{"要做", "CHECK", "SCOPE"} {
		pattern := regexp.MustCompile(`(?m)^\s*(?:#{1,6}\s*)?` + field + `\s*(?::|：|\n)\s*\S`)
		if !pattern.MatchString(task.Description) {
			return Task{}, errors.New("adopt requires 要做, CHECK and SCOPE")
		}
	}
	var declared AdoptInput
	dependencyLine := regexp.MustCompile(`(?m)^DEPENDENCIES:\s*(\{[^\n]+\})\s*$`).FindStringSubmatch(task.Description)
	if len(dependencyLine) != 2 || decodeImportJSON([]byte(dependencyLine[1]), &declared) != nil || declared.Blockers == nil || declared.Related == nil || strings.Join(declared.Blockers, "\x00") != strings.Join(in.Blockers, "\x00") || strings.Join(declared.Related, "\x00") != strings.Join(in.Related, "\x00") {
		return Task{}, errors.New("explicit body DEPENDENCIES must match adoption input")
	}
	for _, refs := range [][]string{in.Blockers, in.Related} {
		for _, ref := range refs {
			dependency, err := c.Read(ctx, ref)
			if err != nil || dependency.ID != ref || dependency.ID == task.ID || !dependency.Complete {
				return Task{}, errors.New("dependency identity or completeness unavailable")
			}
		}
	}
	m := task.Metadata
	m.Complete = true
	m.Blockers = in.Blockers
	m.Related = in.Related
	return c.SetMetadata(ctx, task.ID, m)
}

func (c *GitHub) FreshIssue(ctx context.Context, id string) (task.Issue, error) {
	task, err := c.Read(ctx, id)
	return task.Issue, err
}
