package executor

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

func TestScopeBlockedReportGoesToResolverWithoutCorrection(t *testing.T) {
	e, o, _, s, l, ticket := recoveryFixture(t)
	o.failFirst = true
	o.failBody = "第 4 项做不了\nSCOPE-BLOCKED: functions/api/v1/_mcp-account-routes.js functions/mcp/oauth.test.js\n"
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver || result.State.Phase != machine.PhaseResolver {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if s.sessions != 0 || s.children != 0 || len(o.starts) != 1 || result.State.Corrections != 0 {
		t.Fatalf("correction ran: sessions=%d children=%d starts=%d state=%+v", s.sessions, s.children, len(o.starts), result.State)
	}
	all := strings.Join(l.comments, "\n")
	if !strings.Contains(all, "Agent处置: "+result.State.HumanReason) || !strings.Contains(all, "functions/api/v1/_mcp-account-routes.js functions/mcp/oauth.test.js") {
		t.Fatalf("handoff evidence: %q", all)
	}
}

func TestUnverifiedScopeBlockedKeepsCorrection(t *testing.T) {
	e, o, _, s, _, ticket := recoveryFixture(t)
	o.failFirst = true
	o.failBody = "SCOPE-BLOCKED: unit/change.go\n"
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || s.sessions != 1 {
		t.Fatalf("result=%+v err=%v sessions=%d", result, err, s.sessions)
	}
}

func TestScopeBlockedFiles(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "gate.spec")
	if err := os.WriteFile(spec, []byte("UNIT: executor\nSCOPE: driver/internal/* a.go\nCHECK: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"one file", "SCOPE-BLOCKED: b.go", []string{"b.go"}},
		{"line among prose", "原因\n  SCOPE-BLOCKED: b.go web/c.ts  \n详情", []string{"b.go", "web/c.ts"}},
		{"several lines", "SCOPE-BLOCKED: b.go\nSCOPE-BLOCKED: c.go", []string{"b.go", "c.go"}},
		{"no signal", "SCOPE 缺 b.go，请把它加入 SCOPE", nil},
		{"mid-line mention", "可以写 SCOPE-BLOCKED: b.go", nil},
		{"no paths", "SCOPE-BLOCKED:", nil},
		{"already in SCOPE", "SCOPE-BLOCKED: a.go", nil},
		{"already in SCOPE by glob", "SCOPE-BLOCKED: driver/internal/x/y.go", nil},
		{"one of several in SCOPE", "SCOPE-BLOCKED: b.go a.go", nil},
		{"line number", "SCOPE-BLOCKED: b.go:53", nil},
		{"glob", "SCOPE-BLOCKED: web/*", nil},
		{"absolute", "SCOPE-BLOCKED: /etc/passwd", nil},
		{"parent", "SCOPE-BLOCKED: ../b.go", nil},
		{"unclean", "SCOPE-BLOCKED: web/../b.go", nil},
		{"ledger field injection", "SCOPE-BLOCKED: b.go\x00归属:", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopeBlockedFiles(tc.body, spec); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	if got := scopeBlockedFiles("SCOPE-BLOCKED: b.go", filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("unreadable spec verified %q", got)
	}
}
