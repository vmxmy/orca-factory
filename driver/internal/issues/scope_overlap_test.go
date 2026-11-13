package issues

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestCreateRejectsOverlappingScope(t *testing.T) {
	for _, tt := range []struct {
		name      string
		existing  string
		scope     string
		state     string
		project   string
		blockers  []string
		related   []string
		unmanaged bool
		wantError bool
	}{
		{name: "same file", existing: "driver/a.go", scope: "driver/a.go", wantError: true},
		{name: "existing glob", existing: "driver/*", scope: "driver/internal/a.go", wantError: true},
		{name: "new glob", existing: "driver/internal/a.go", scope: "driver/*", wantError: true},
		{name: "glob common directory", existing: "driver/a*.go", scope: "driver/*_test.go", wantError: true},
		{name: "glob nested directory", existing: "driver/a*.go", scope: "driver/internal/*_test.go", wantError: true},
		{name: "root glob", existing: "*.go", scope: "driver/*_test.go", wantError: true},
		{name: "question glob", existing: "driver/?.go", scope: "driver/a.go", wantError: true},
		{name: "bracket glob", existing: "driver/[ab].go", scope: "driver/a.go", wantError: true},
		{name: "later scope item", existing: "docs/a.md\ndriver/a.go", scope: "docs/b.md driver/a.go", wantError: true},
		{name: "backlog", existing: "driver/a.go", scope: "driver/a.go", state: "Backlog", wantError: true},
		{name: "in progress", existing: "driver/a.go", scope: "driver/a.go", state: "In Progress", wantError: true},
		{name: "unmanaged", existing: "driver/a.go", scope: "driver/a.go", unmanaged: true, wantError: true},
		{name: "blocker", existing: "driver/a.go", scope: "driver/a.go", blockers: []string{feishuUUIDA}},
		{name: "related", existing: "driver/a.go", scope: "driver/a.go", related: []string{feishuUUIDA}},
		{name: "done", existing: "driver/a.go", scope: "driver/a.go", state: "Done"},
		{name: "canceled", existing: "driver/a.go", scope: "driver/a.go", state: "Canceled"},
		{name: "duplicate", existing: "driver/a.go", scope: "driver/a.go", state: "Duplicate"},
		{name: "other repository", existing: "driver/a.go", scope: "driver/a.go", project: "recP2"},
		{name: "distinct files", existing: "driver/a.go", scope: "driver/b.go"},
		{name: "distinct glob directories", existing: "driver/*", scope: "core/*"},
		{name: "directory boundary", existing: "driver/a/*", scope: "driver/ab/*"},
		{name: "literal misses glob", existing: "driver/*.go", scope: "driver/a.md"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFeishuFake(t)
			state, project := tt.state, tt.project
			if state == "" {
				state = "Todo"
			}
			if project == "" {
				project = "recP1"
			}
			row := feishuTicket("recA", "NO.001", feishuUUIDA, project, state, nil, nil)
			row["SCOPE"] = tt.existing
			row["纳管指纹"] = (FeishuBrief{Todo: "做 recA", Check: "make test", Scope: tt.existing}).Fingerprint()
			if tt.unmanaged {
				row["纳管指纹"] = ""
			}
			f.rows["tblT"] = []feishuRow{row}
			c := newFeishuTest(t, f)
			fastReadBack(c)
			brief := FeishuBrief{Todo: "修改文件", Check: "make test", Scope: tt.scope, Blockers: nonNil(tt.blockers), Related: nonNil(tt.related)}
			created, err := c.Create(context.Background(), CreateInput{Title: "新票", Description: brief.Description(), Blockers: brief.Blockers, Related: brief.Related})
			if tt.wantError {
				if err == nil {
					t.Fatal("Create accepted overlapping SCOPE without a declared dependency")
				}
				for _, want := range []string{"OF-1", "SCOPE", "driver/", "blockers", "related", "重复"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q missing %q", err, want)
					}
				}
				if len(f.writes) != 0 {
					t.Fatalf("rejection wrote %d requests", len(f.writes))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if created.Identifier != "OF-10" || !created.Metadata.Complete || len(f.writes) != 1 {
				t.Fatalf("created=%+v writes=%d", created, len(f.writes))
			}
			if !slices.Equal(created.Metadata.Blockers, brief.Blockers) || !slices.Equal(created.Metadata.Related, brief.Related) {
				t.Fatalf("dependencies changed: %+v", created.Metadata)
			}
		})
	}
}

func closeFeishuTickets(f *feishuFake) {
	for _, row := range f.rows["tblT"] {
		row["状态"] = []string{"Done"}
	}
}
