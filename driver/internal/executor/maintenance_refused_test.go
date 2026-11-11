package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type refusedChildOrca struct {
	*recoveryOrca
	refused bool
}

func (o *refusedChildOrca) WorkerStart(ctx context.Context, request string, opts orca.StartOptions) (orca.Receipt, error) {
	if opts.Task == "child_task" && !o.refused {
		o.refused = true
		return nil, fmt.Errorf("worker start: %w", maintenance.ErrRefused)
	}
	return o.recoveryOrca.WorkerStart(ctx, request, opts)
}
func (o *refusedChildOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	if o.refused && len(o.starts) == 1 {
		return []orca.Task{{ID: "child_task", RunID: "run_one", ParentID: "task_one", Status: "ready"}}, nil
	}
	return o.recoveryOrca.TaskList(ctx, run)
}

// recordedRefusedChildOrca replays the run OF-458 left behind: task-list and
// worker-list come from maintenance-refused-child.json through the Orca client,
// and the maintenance gate refuses the replayed start again.
type recordedRefusedChildOrca struct {
	*recoveryOrca
	client *orca.Client
}

func (o *recordedRefusedChildOrca) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	return o.client.TaskList(ctx, run)
}
func (o *recordedRefusedChildOrca) Workers(ctx context.Context, opts orca.ListOptions) ([]orca.Worker, error) {
	return o.client.Workers(ctx, opts)
}
func (o *recordedRefusedChildOrca) WorkerStart(_ context.Context, _ string, opts orca.StartOptions) (orca.Receipt, error) {
	o.starts = append(o.starts, opts)
	return nil, fmt.Errorf("worker start: %w", maintenance.ErrRefused)
}

// OF-458: the drain refused the correction dispatch after task-create, leaving
// a ready child with no dispatch. The replacement executor replays that child;
// any evidence of a worker on it, or an unreadable worker-list, still refuses.
func TestRecordedMaintenanceRefusedChild(t *testing.T) {
	const run, parent, child = "run_37842cdf5951", "task_fc83d5da1a16", "task_4c97bee5725f"
	const worktree = "/home/laoxu/orca/workspaces/orca-factory/OF-458-executor"
	var sample map[string]json.RawMessage
	if err := json.Unmarshal(recorded(t, "maintenance-refused-child.json"), &sample); err != nil {
		t.Fatal(err)
	}
	keep := func(s string) string { return s }
	for _, tc := range []struct {
		name           string
		tasks, workers func(string) string
		dispatch       string
	}{
		{name: "recorded", tasks: keep, workers: keep},
		{name: "ready child bound to a dispatch", tasks: keep, workers: func(s string) string {
			return strings.ReplaceAll(s, `"taskId": "`+parent+`"`, `"taskId": "`+child+`"`)
		}},
		{name: "worker list unreadable", tasks: keep, workers: func(string) string { return "" }},
		{name: "worker list incomplete", tasks: keep, workers: func(s string) string {
			return strings.Replace(s, `"hasMore": false`, `"hasMore": true`, 1)
		}},
		{name: "child dispatched", workers: keep, tasks: func(s string) string {
			return strings.Replace(s, `"status": "ready"`, `"status": "dispatched"`, 1)
		}},
		{name: "child abandoned", workers: keep, tasks: func(s string) string {
			return strings.Replace(s, `"status": "ready"`, `"status": "blocked"`, 1)
		}},
		{name: "child result", workers: keep, tasks: func(s string) string {
			return strings.Replace(s, `"result": null`, `"result": "{\"provenance\":\"other\"}"`, 1)
		}},
		{name: "different dispatch", tasks: keep, workers: keep, dispatch: "ctx_other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, base, _, s, l, ticket := recoveryFixture(t)
			dir := t.TempDir()
			tasks, workers := filepath.Join(dir, "tasks.json"), filepath.Join(dir, "workers.json")
			if err := os.WriteFile(tasks, []byte(tc.tasks(string(sample["task-list"]))), 0600); err != nil {
				t.Fatal(err)
			}
			if body := tc.workers(string(sample["worker-list"])); body != "" {
				if err := os.WriteFile(workers, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			script := filepath.Join(dir, "orca")
			if err := os.WriteFile(script, []byte("#!/bin/sh\ncase \"$2\" in\ntask-list) cat '"+tasks+"' ;;\nworker-list) cat '"+workers+"' ;;\n*) exit 1 ;;\nesac\n"), 0700); err != nil {
				t.Fatal(err)
			}
			o := &recordedRefusedChildOrca{recoveryOrca: base, client: &orca.Client{Path: script}}
			e.Orca = o
			e.Config.Terminal = "replacement_terminal"
			if err := saveCheckpoint(e.Config, ticket, checkpoint{Run: run, PendingParent: parent, Pending: true, Dispatch: tc.dispatch, Worktree: worktree, Corrections: 1}); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(checkpointPath(e.Config, ticket))
			_, err := e.Start(context.Background(), ticket)
			if tc.name != "recorded" {
				after, _ := os.ReadFile(checkpointPath(e.Config, ticket))
				if err == nil || !strings.Contains(err.Error(), "unconfirmed correction child") || len(o.starts) != 0 || s.children != 0 || string(before) != string(after) {
					t.Fatalf("unproven child replayed: err=%v starts=%+v children=%d cp=%s -> %s", err, o.starts, s.children, before, after)
				}
				return
			}
			if !errors.Is(err, maintenance.ErrRefused) || s.children != 0 || len(o.starts) != 1 {
				t.Fatalf("recorded child not replayed: err=%v starts=%+v children=%d", err, o.starts, s.children)
			}
			if got := o.starts[0]; got.Task != child || got.Run != run || got.Worktree != "path:"+worktree {
				t.Fatalf("replayed wrong child: %+v", got)
			}
			if last := l.comments[len(l.comments)-1]; !strings.HasPrefix(last, maintenance.RefusedLine) {
				t.Fatalf("second refusal not recorded: %q", last)
			}
			cp, err := readCheckpoint(e.Config, ticket)
			if err != nil || cp.PendingParent != parent || cp.Task != child || cp.Pending {
				t.Fatalf("second refusal left an unreplayable checkpoint: %+v err=%v", cp, err)
			}
		})
	}
}

// A correction dispatch the maintenance gate refused never reached Orca: the
// executor records a refusal instead of a failure, and the replacement replays
// the already-created child once.
func TestCorrectionMaintenanceRefusedReplaysChild(t *testing.T) {
	e, base, tools, s, l, ticket := recoveryFixture(t)
	tools.gateReds = 1
	o := &refusedChildOrca{recoveryOrca: base}
	e.Orca = o
	if _, err := e.Execute(context.Background(), ticket); !errors.Is(err, maintenance.ErrRefused) {
		t.Fatalf("refusal not reported: %v", err)
	}
	if last := l.comments[len(l.comments)-1]; !strings.HasPrefix(last, maintenance.RefusedLine) {
		t.Fatalf("refusal not recorded: %q", last)
	}
	l.comments = append(l.comments, "阶段: 写 correction\n归属: 执行器 term=- run=- attempt=1 scheduler-attempt=1")
	e.Config.Terminal = "replacement_terminal"
	result, err := e.Start(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerMerge {
		t.Fatalf("replay failed: %+v err=%v", result, err)
	}
	if s.children != 1 || len(o.starts) != 2 || o.starts[1].Task != "child_task" {
		t.Fatalf("child not replayed once: children=%d starts=%+v", s.children, o.starts)
	}
}
