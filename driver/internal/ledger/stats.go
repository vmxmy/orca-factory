package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Stats includes every row on a ledger issue whose update or comment timestamps
// overlap the last seven days. It is an issue window, not a row timestamp filter.
func (l *Ledger) Stats(ctx context.Context, all bool, out, warnings io.Writer) error {
	issues, err := l.store.ListIssues(ctx)
	if err != nil {
		return err
	}
	if warnings == nil {
		warnings = io.Discard
	}
	cutoff := l.now().Add(-7 * 24 * time.Hour)
	rows := []map[string]any{}
	seen := map[string]bool{}
	for _, issue := range issues {
		if _, ok := Number(issue.Title); !ok {
			continue
		}
		comments, err := l.store.ListComments(ctx, issue.ID)
		if err != nil {
			return err
		}
		include := all || !issue.UpdatedAt.Before(cutoff)
		for _, comment := range comments {
			if !comment.CreatedAt.Before(cutoff) || !comment.UpdatedAt.Before(cutoff) {
				include = true
			}
		}
		if !include {
			continue
		}
		for _, comment := range comments {
			for _, line := range strings.Split(comment.Body, "\n") {
				raw, ok := strings.CutPrefix(line, "ledger ")
				if !ok {
					continue
				}
				row, err := parseRow(raw)
				if err != nil {
					fmt.Fprintln(warnings, "ledger: warning: invalid ledger comment skipped")
					continue
				}
				key, _ := json.Marshal([]any{row["ticket"], row["started_at"]})
				if seen[string(key)] {
					continue
				}
				seen[string(key)] = true
				rows = append(rows, row)
			}
		}
	}
	return writeStats(out, rows)
}

func number(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }
func average(rows []map[string]any, key string) string {
	sum := 0.0
	count := 0
	for _, row := range rows {
		if n, ok := row[key].(float64); ok {
			sum += n
			count++
		}
	}
	if count == 0 {
		return "0"
	}
	return number(math.Round(sum/float64(count)*100) / 100)
}
func percent(rows []map[string]any) string {
	green, count := 0, 0
	for _, row := range rows {
		value := row["first_gate"]
		if value == nil || value == false {
			continue
		}
		count++
		if value == "green" {
			green++
		}
	}
	if count == 0 {
		return "0"
	}
	return number(math.Round(float64(green) * 100 / float64(count)))
}
func display(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}
func jqRank(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case bool:
		return 1
	case float64:
		return 2
	case string:
		return 3
	case []any:
		return 4
	default:
		return 5
	}
}
func less(a, b any) bool {
	if jqRank(a) != jqRank(b) {
		return jqRank(a) < jqRank(b)
	}
	if x, ok := a.(float64); ok {
		return x < b.(float64)
	}
	return display(a) < display(b)
}
func writeStats(out io.Writer, rows []map[string]any) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "runs=0")
		return err
	}
	var result strings.Builder
	outcomes := map[string]int{}
	reds := 0.0
	for _, row := range rows {
		if s, ok := row["outcome"].(string); ok {
			outcomes[s]++
		}
		if n, ok := row["ci_reds"].(float64); ok {
			reds += n
		}
	}
	fmt.Fprintf(&result, "runs=%d merged=%d escalated=%d no_ticket=%d\n", len(rows), outcomes["merged"], outcomes["escalated"], outcomes["no_ticket"])
	fmt.Fprintf(&result, "avg_corrections=%s ci_reds=%s avg_minutes=%s\n", average(rows, "corrections"), number(reds), average(rows, "minutes"))
	for _, field := range []string{"lane", "tier"} {
		type group struct {
			value any
			rows  []map[string]any
		}
		groups := map[string]*group{}
		for _, row := range rows {
			value := row[field]
			if field == "lane" && value == nil {
				continue
			}
			if field == "tier" {
				if row["outcome"] == "no_ticket" {
					continue
				}
				if value == nil || value == false {
					value = "unknown"
				}
			}
			encoded, _ := json.Marshal(value)
			key := string(encoded)
			if groups[key] == nil {
				groups[key] = &group{value: value}
			}
			groups[key].rows = append(groups[key].rows, row)
		}
		sorted := make([]*group, 0, len(groups))
		for _, g := range groups {
			sorted = append(sorted, g)
		}
		sort.Slice(sorted, func(i, j int) bool { return less(sorted[i].value, sorted[j].value) })
		for _, g := range sorted {
			fmt.Fprintf(&result, "%s=%s runs=%d first_gate_green_pct=%s\n", field, display(g.value), len(g.rows), percent(g.rows))
		}
	}
	_, err := io.WriteString(out, result.String())
	return err
}
