package issues

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func (c *Feishu) requireScopeDependencies(ctx context.Context, in CreateInput) error {
	brief, _, err := parseFeishuDescription(in.Description)
	if err != nil {
		return err
	}
	scope := strings.Fields(brief.Scope)
	if len(scope) == 0 {
		return nil
	}
	projects, own, _, err := c.projects(ctx)
	if err != nil {
		return err
	}
	filter := feishu.Filter{Logic: "and", Conditions: []feishu.Condition{{Field: "项目", Operator: "intersects", Value: []feishu.LinkRef{{ID: own}}}}}
	records, _, err := c.readAll(ctx, "票据", &filter)
	if err != nil {
		return fmt.Errorf("read tickets for SCOPE overlap: %w", err)
	}
	var conflicts []string
	for _, record := range records {
		row, err := c.row(record)
		if err != nil {
			return fmt.Errorf("read ticket for SCOPE overlap: %w", err)
		}
		if row.project != own || closedState(row.state) {
			continue
		}
		if in.Blockers != nil && in.Related != nil && (slices.Contains(in.Blockers, row.id()) || slices.Contains(in.Related, row.id())) {
			continue
		}
		for _, a := range scope {
			for _, b := range strings.Fields(row.brief.Scope) {
				if scopeEntriesOverlap(a, b) {
					conflicts = append(conflicts, fmt.Sprintf("%s (%s): %q 与 %q", identifier(row, projects), row.id(), a, b))
				}
			}
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("SCOPE 与未完成票重叠：%s；请将冲突票 UUID 加入 blockers，或加入 related；确认重复则不建票", strings.Join(conflicts, "; "))
	}
	return nil
}

func scopeEntriesOverlap(a, b string) bool {
	if task.ScopeMatch(a, b) || task.ScopeMatch(b, a) {
		return true
	}
	ai, bi := strings.IndexAny(a, "*?["), strings.IndexAny(b, "*?[")
	if ai < 0 || bi < 0 {
		return false
	}
	// Two globs conservatively overlap when their fixed directories overlap.
	ad := a[:strings.LastIndex(a[:ai], "/")+1]
	bd := b[:strings.LastIndex(b[:bi], "/")+1]
	return strings.HasPrefix(ad, bd) || strings.HasPrefix(bd, ad)
}
