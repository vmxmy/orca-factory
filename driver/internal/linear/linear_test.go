package linear

import "testing"

func TestIssueBlocked(t *testing.T) {
	blocker := func(kind, stateType string) InverseRelation {
		r := InverseRelation{Type: kind}
		r.Issue.Identifier = "X-1"
		r.Issue.State.Type = stateType
		return r
	}
	cases := []struct {
		name  string
		nodes []InverseRelation
		more  bool
		want  bool
	}{
		{"none", nil, false, false},
		{"open blocker", []InverseRelation{blocker("blocks", "started")}, false, true},
		{"done blocker", []InverseRelation{blocker("blocks", "completed")}, false, false},
		{"canceled blocker", []InverseRelation{blocker("blocks", "canceled")}, false, false},
		{"duplicate blocker", []InverseRelation{blocker("blocks", "duplicate")}, false, false},
		{"related only", []InverseRelation{blocker("related", "started")}, false, false},
		{"truncated is conservative", nil, true, true},
	}
	for _, c := range cases {
		issue := Issue{InverseRelations: Connection[InverseRelation]{Nodes: c.nodes, PageInfo: PageInfo{HasNextPage: c.more}}}
		if got := issue.Blocked(); got != c.want {
			t.Fatalf("%s: Blocked()=%v want %v", c.name, got, c.want)
		}
	}
}
