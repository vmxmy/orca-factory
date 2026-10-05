package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type lostChildService struct {
	recoveryService
	child orca.Task
	cause error
}

func (s *lostChildService) ChildTask(_ context.Context, run, parent, spec, request string) (string, error) {
	s.children++
	s.child = orca.Task{ID: "child_task", RunID: run, ParentID: parent, Spec: spec, Status: "blocked"}
	return "", s.cause
}

type lostChildOrca struct {
	*recoveryOrca
	service   *lostChildService
	alter     func([]orca.Task) []orca.Task
	failStart bool
}

func (o *lostChildOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	if o.service.children == 0 || len(o.starts) > 1 {
		return o.recoveryOrca.TaskList(ctx, run)
	}
	tasks := []orca.Task{o.service.child}
	if o.alter != nil {
		tasks = o.alter(tasks)
	}
	return tasks, nil
}
func (o *lostChildOrca) WorkerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	receipt, err := o.recoveryOrca.WorkerStart(ctx, request, opts)
	if o.failStart && opts.Task != "" {
		return nil, errors.New("worker started but receipt lost")
	}
	return receipt, err
}
func lostChildFixture(t *testing.T) (*Executor, *lostChildOrca, *recoveryTools, *lostChildService, Ticket) {
	t.Helper()
	e, base, tools, _, _, ticket := recoveryFixture(t)
	tools.gateReds = 1
	s := &lostChildService{cause: &infra.Ticket{Err: errors.New("rejected: request_mismatch ghp_private")}}
	o := &lostChildOrca{recoveryOrca: base, service: s}
	e.Recovery = s
	e.Orca = o
	_, err := e.Execute(context.Background(), ticket)
	var classified *infra.Ticket
	if err == nil || !errors.As(err, &classified) || strings.Contains(err.Error(), "ghp_private") {
		t.Fatalf("lost safe error chain: %v", err)
	}
	cp, err := readCheckpoint(e.Config, ticket)
	if err != nil || cp.PendingParent != "task_one" || !cp.Pending || cp.Task != "" || cp.Dispatch != "" {
		t.Fatalf("lost child intent: %+v %v", cp, err)
	}
	cp.Infra = 1
	cp.Attempt = 2
	if err := saveCheckpoint(e.Config, ticket, *cp); err != nil {
		t.Fatal(err)
	}
	e.Config.Terminal = "replacement_terminal"
	return e, o, tools, s, ticket
}
func TestLostCorrectionChildStartsOnceAfterTerminalReplacement(t *testing.T) {
	e, o, tools, s, ticket := lostChildFixture(t)
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge || len(o.starts) != 2 || s.children != 1 || s.sessions != 1 {
		t.Fatalf("child recovery failed: %+v err=%v starts=%d children=%d sessions=%d", result, err, len(o.starts), s.children, s.sessions)
	}
	if o.starts[1].Task != "child_task" || o.starts[1].Run != "run_one" || o.starts[1].Worktree != "path:/original/worktree" || result.State.Corrections != 1 || result.State.Infra != 1 || result.State.Attempt != 2 || tools.gateCalls != 2 || tools.reviewCalls != 1 {
		t.Fatalf("lost worktree/budget/validation: result=%+v opts=%+v gates=%d reviews=%d", result, o.starts[1], tools.gateCalls, tools.reviewCalls)
	}
	e.Config.Terminal = "third_terminal"
	e.GitHub.(*fakeGitHub).existing = &result.PR
	e.GitHub.(*fakeGitHub).existing.Head.SHA = tools.head
	for restart := 0; restart < 2; restart++ {
		resumed, err := e.Start(context.Background(), ticket)
		if err != nil || resumed.State.Infra != 1 {
			t.Fatalf("third/later Start double-counted historical budget: %+v err=%v", resumed.State, err)
		}
	}
	if len(o.starts) != 2 || s.children != 1 {
		t.Fatalf("mutation replayed: starts=%d children=%d", len(o.starts), s.children)
	}
}
func TestLostCorrectionChildRejectsUnprovenIdentity(t *testing.T) {
	for _, kind := range []string{"missing-run", "wrong-run", "wrong-parent", "ambiguous", "dispatch", "status", "result", "missing-worktree", "worker-intent", "changed-child", "missing-after-adoption"} {
		t.Run(kind, func(t *testing.T) {
			e, o, _, s, ticket := lostChildFixture(t)
			o.alter = func(tasks []orca.Task) []orca.Task {
				switch kind {
				case "missing-after-adoption":
					return nil
				case "missing-run":
					tasks[0].RunID = ""
				case "wrong-run":
					tasks[0].RunID = "other"
				case "wrong-parent":
					tasks[0].ParentID = "other"
				case "ambiguous":
					tasks = append(tasks, tasks[0])
				case "dispatch":
					tasks[0].DispatchID = "ctx_existing"
				case "status":
					tasks[0].Status = "pending"
				case "result":
					tasks[0].Result = json.RawMessage(`{"provenance":"other"}`)
				}
				return tasks
			}
			if kind == "missing-worktree" || kind == "worker-intent" || kind == "changed-child" || kind == "missing-after-adoption" {
				cp, _ := readCheckpoint(e.Config, ticket)
				if kind == "missing-worktree" {
					cp.Worktree = ""
				} else if kind == "missing-after-adoption" {
					cp.Task, cp.Pending = "child_task", false
				} else if kind == "changed-child" {
					cp.Task = "previously_adopted_child"
				} else {
					cp.PendingParent = ""
					cp.Task = "child_task"
				}
				if err := saveCheckpoint(e.Config, ticket, *cp); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(checkpointPath(e.Config, ticket))
			_, err := e.Start(context.Background(), ticket)
			after, _ := os.ReadFile(checkpointPath(e.Config, ticket))
			if err == nil || len(o.starts) != 1 || s.children != 1 || string(before) != string(after) {
				t.Fatalf("unproven child replayed: err=%v starts=%d children=%d cp=%s -> %s", err, len(o.starts), s.children, before, after)
			}
		})
	}
}
func TestRecoveredChildWorkerUncertaintyDoesNotReplay(t *testing.T) {
	e, o, _, s, ticket := lostChildFixture(t)
	o.failStart = true
	if _, err := e.Start(context.Background(), ticket); err == nil || len(o.starts) != 2 {
		t.Fatalf("expected uncertain recovered worker: err=%v starts=%d", err, len(o.starts))
	}
	cp, _ := readCheckpoint(e.Config, ticket)
	if !cp.Pending || cp.PendingParent != "" || cp.Task != "child_task" {
		t.Fatalf("missing worker intent: %+v", cp)
	}
	e.Config.Terminal = "third_terminal"
	if _, err := e.Start(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(o.starts) != 2 || s.children != 1 {
		t.Fatal("uncertain worker replayed")
	}
}

func TestLostCorrectionChildExhaustedBudgetDoesNotStart(t *testing.T) {
	e, o, _, s, ticket := lostChildFixture(t)
	cp, _ := readCheckpoint(e.Config, ticket)
	cp.Infra = 3
	if err := saveCheckpoint(e.Config, ticket, *cp); err != nil {
		t.Fatal(err)
	}
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerHuman || result.State.Infra != 3 || s.children != 1 || len(o.starts) != 1 {
		t.Fatalf("exhausted budget authorized child: %+v err=%v starts=%d", result, err, len(o.starts))
	}
}
