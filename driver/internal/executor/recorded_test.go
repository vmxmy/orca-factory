package executor

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func recorded(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/recorded/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecordedTicketDescriptions(t *testing.T) {
	for _, c := range []struct{ file, check, scope string }{
		{"linear-description-escaped.md", `set -a; . scripts/agent-gates/agent-graph.env; set +a; [[ $AGENT_ROLE_REVIEWER_CMD == claude\ -p* ]] && ! grep -q 'AGENT_ROLE_REVIEWER_CMD -"' core/COORDINATOR.md scripts/agent-gates/COORDINATOR.md && bin/orca-factory check . >/dev/null`, "core/COORDINATOR.md scripts/agent-gates/COORDINATOR.md scripts/agent-gates/agent-graph.env"},
		{"linear-description-check-assertion.md", "", "driver/internal/status/* driver/internal/scheduler/* driver/internal/executor/* mods/swarm-board/hooks/driver-status.ts mods/swarm-board/hooks/register.tsx"},
		{"linear-description-heading-note.md", "", "driver/internal/usage/* driver/internal/ledger/* mods/swarm-board/types/index.d.ts"},
	} {
		got, err := ParseTicket("id", "ZII-1", "t", string(recorded(t, c.file)))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if c.check != "" && got.Check != c.check {
			t.Fatalf("%s: Check=%q", c.file, got.Check)
		}
		if got.Scope != c.scope {
			t.Fatalf("%s: Scope=%q", c.file, got.Scope)
		}
		if got.Todo == "" || containsAny(got.Check, "断言", `\*`) {
			t.Fatalf("%s: Todo=%q Check=%q", c.file, got.Todo, got.Check)
		}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

func TestRecordedReviewerOutput(t *testing.T) {
	if v, err := reviewVerdict(string(recorded(t, "reviewer-output-fenced.txt"))); err != nil || v != "red" {
		t.Fatalf("verdict=%q err=%v", v, err)
	}
}

func TestRecordedTaskList(t *testing.T) {
	var envelope struct {
		Result struct{ Tasks []orca.Task } `json:"result"`
	}
	if err := json.Unmarshal(recorded(t, "orca-task-list.json"), &envelope); err != nil || len(envelope.Result.Tasks) == 0 {
		t.Fatalf("tasks=%v err=%v", envelope.Result.Tasks, err)
	}
	task := envelope.Result.Tasks[0]
	o := &recordedTasks{tasks: envelope.Result.Tasks}
	e := &Executor{Orca: o, Sleep: func(context.Context, time.Duration) error { t.Fatal("settled task was not recognised"); return nil }}
	report, err := e.wait(context.Background(), task.RunID, task.ID, "ctx_unknown_to_task_list", time.Millisecond, func() error { return nil })
	if err != nil || report == nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

type recordedTasks struct {
	fakeOrca
	tasks []orca.Task
}

func (r *recordedTasks) TaskList(context.Context, string) ([]orca.Task, error) { return r.tasks, nil }

func TestRecordedWorkerWorktree(t *testing.T) {
	raw := recorded(t, "git-worktree-list-porcelain.bin")
	c := &Commands{Run: func(context.Context, runx.Spec) ([]byte, error) { return raw, nil }}
	path, err := c.Worktree(context.Background(), "/repo", "agent/ZII-893/executor")
	if err != nil || !strings.HasSuffix(path, "/ZII-893-executor") {
		t.Fatalf("worker worktree for agent/ZII-893/executor: path=%q err=%v", path, err)
	}
}
