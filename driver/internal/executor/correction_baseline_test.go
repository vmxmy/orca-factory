package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// The remote main advanced after the checkpoint was written. Validation of a
// repaired head uses its merge-base with that current main.
type correctionBaselineTools struct {
	*recoveryTools
	main string
}

func (f *correctionBaselineTools) Baseline(context.Context, string) (string, error) {
	return f.main, nil
}
func (f *correctionBaselineTools) MergeBase(context.Context, string, string) (string, error) {
	return f.main, nil
}

// The child exists, but its dispatch was never started before the crash. Start
// must reconcile that evidence and launch it in the original worktree once.
type correctionBaselineOrca struct{ *recoveryOrca }

func (o *correctionBaselineOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	if len(o.starts) == 0 {
		return []orca.Task{{ID: "child_task", RunID: run, ParentID: "task_one", Status: "blocked"}}, nil
	}
	return o.recoveryOrca.TaskList(ctx, run)
}

func TestStartCorrectionAfterMainAdvances(t *testing.T) {
	for _, repaired := range []bool{false, true} {
		name := "main_only_is_not_a_repair"
		if repaired {
			name = "repair_is_reviewed_against_current_main"
		}
		t.Run(name, func(t *testing.T) {
			e, o, tools, recovery, tracker, ticket := recoveryFixture(t)
			oldMain, currentMain := strings.Repeat("a", 40), strings.Repeat("b", 40)
			tools.head = currentMain
			if repaired {
				tools.head = strings.Repeat("c", 40)
			}
			e.Tools = &correctionBaselineTools{recoveryTools: tools, main: currentMain}
			e.Orca = &correctionBaselineOrca{o}
			e.Config.Terminal = "replacement_executor"
			cp := checkpoint{
				Run: "run_one", Task: "child_task", PendingParent: "task_one", Pending: true,
				Worktree: "/original/worktree", Baseline: oldMain,
				Corrections: 2, Infra: 1, Attempt: 3, CatchupRounds: 1,
			}
			if err := saveCheckpoint(e.Config, ticket, cp); err != nil {
				t.Fatal(err)
			}

			result, err := e.Start(context.Background(), ticket)
			if err != nil {
				t.Fatal(err)
			}
			if len(o.starts) != 1 || len(o.runRequests) != 0 || len(o.used) != 1 || o.used[0] != cp.Run || recovery.sessions != 0 || recovery.children != 0 {
				t.Fatalf("recovery replayed work: starts=%+v runs=%v used=%v sessions=%d children=%d", o.starts, o.runRequests, o.used, recovery.sessions, recovery.children)
			}
			start := o.starts[0]
			if start.Run != cp.Run || start.Task != cp.Task || start.Worktree != "path:"+cp.Worktree || start.BaseBranch != "" || start.Repo != "" || start.Spec != "" || start.Name != "" {
				t.Fatalf("recovery must reuse the original child/worktree, not create a branch: %+v", start)
			}
			if result.State.Corrections != cp.Corrections || result.State.Infra != cp.Infra || result.State.Attempt != cp.Attempt {
				t.Fatalf("recovery changed budgets: %+v", result.State)
			}
			if !repaired {
				if result.State.Owner != machine.OwnerResolver || result.State.HumanReason != "worker reported success but branch did not advance" || result.PR.Number != 0 || tools.gateCalls != 0 || tools.reviewCalls != 0 {
					t.Fatalf("current main is not a repair: result=%+v gates=%d reviews=%d", result, tools.gateCalls, tools.reviewCalls)
				}
				return
			}
			if result.State.Owner != machine.OwnerMerge || tools.gateCalls != 1 || tools.reviewCalls != 1 || tools.reviewBase != currentMain {
				t.Fatalf("repair must validate against current main: result=%+v gates=%d reviews=%d reviewBase=%q", result, tools.gateCalls, tools.reviewCalls, tools.reviewBase)
			}
			var handoff merge.Handoff
			for _, body := range tracker.comments {
				for _, line := range strings.Split(body, "\n") {
					if raw, ok := strings.CutPrefix(line, merge.HandoffPrefix); ok {
						if err := json.Unmarshal([]byte(raw), &handoff); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if handoff.Baseline != currentMain || handoff.Head != tools.head || handoff.Worktree != cp.Worktree || handoff.Corrections != cp.Corrections || handoff.Infra != cp.Infra || handoff.Attempt != cp.Attempt || handoff.CatchupRounds != cp.CatchupRounds {
				t.Fatalf("handoff lost validated baseline, worktree or budgets: %+v", handoff)
			}
		})
	}
}
