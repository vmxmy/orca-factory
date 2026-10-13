package executor

import (
	"os"
	"path/filepath"
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

func TestDriverWorkerRoleForbidsAsk(t *testing.T) {
	role, err := os.ReadFile(filepath.Join("..", "..", "..", "core", "roles", "driver-worker.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(role), "orca orchestration ask") || !strings.Contains(string(role), "--outcome failed") {
		t.Fatal("driver-worker role must forbid ask and require worker_done --outcome failed when blocked")
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
