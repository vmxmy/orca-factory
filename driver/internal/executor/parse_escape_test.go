package executor

import (
	"strings"
	"testing"
)

func TestParseTicketLinearMarkdown(t *testing.T) {
	description := "## 要做\n\n1. do it\n\n## 验收\n\nCHECK: \\[\\[ $X == claude\\\\ -p\\* \\]\\] && grep -q 'a\\_b' f\n断言: 这一行不是命令\n\n## SCOPE\n\ndriver/internal/status/\\* mods/x/\\_y.ts\n\n## 依赖\n\n无\n"
	got, err := ParseTicket("id", "ZII-1", "t", description)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[[ $X == claude\ -p* ]] && grep -q 'a_b' f`; got.Check != want {
		t.Fatalf("Check=%q want %q", got.Check, want)
	}
	if want := "driver/internal/status/* mods/x/_y.ts"; got.Scope != want {
		t.Fatalf("Scope=%q want %q", got.Scope, want)
	}
}

func TestBriefForbidsAsk(t *testing.T) {
	if !strings.Contains(briefTemplate, "orca orchestration ask") || !strings.Contains(briefTemplate, "--outcome failed") {
		t.Fatal("brief must forbid ask and require worker_done --outcome failed when blocked")
	}
}

func TestParseTicketHeadingWithNote(t *testing.T) {
	description := "## 要做（接口票：只定义结构）\n\n1. do it\n\n## 验收\n\nCHECK: true\n\n## SCOPE (新建)\n\na/b.go\n"
	got, err := ParseTicket("id", "ZII-1", "t", description)
	if err != nil {
		t.Fatal(err)
	}
	if got.Todo != "1. do it" || got.Scope != "a/b.go" {
		t.Fatalf("Todo=%q Scope=%q", got.Todo, got.Scope)
	}
}

// A worker that sees `declare function h` without an implementation "fixes" it back to
// JSX, which cannot build in the clean checkout the gate uses (no react). The brief must
// say that h is injected by the host and that .tsx views take no JSX.
func TestBriefExplainsInjectedHAndNoJSX(t *testing.T) {
	for _, want := range []string{"declare function h", "injected by the host", "JSX"} {
		if !strings.Contains(briefTemplate, want) {
			t.Fatalf("brief template lacks %q", want)
		}
	}
}
