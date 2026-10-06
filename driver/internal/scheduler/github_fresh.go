package scheduler

import (
	"context"
	"errors"
	"reflect"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

const githubSquash ActionKind = "github-squash"

// Revalidate the selected ticket immediately before a decision can mutate it.
// The snapshot cache helps discovery only; comments may change independently of
// the issue timestamp, including human cancellation and edited merge receipts.
func (r *Runtime) verifyGitHubDecision(ctx context.Context, t Ticket, action ActionKind) error {
	c, ok := r.Clients[t.Repo].(*issues.GitHub)
	if !ok {
		return nil
	}
	task, err := c.Read(ctx, t.ID)
	if err != nil {
		return err
	}
	if task.Metadata.ImportHold {
		return errors.New("task import is held")
	}
	comments, err := c.ListComments(ctx, t.ID)
	if err != nil {
		return err
	}
	fresh := Ticket{ID: t.ID, Identifier: t.Identifier, Repo: t.Repo}
	if err = readOwnership(&fresh, comments); err != nil {
		return err
	}
	readMergeHandoff(&fresh, comments)
	if fresh.Owner != t.Owner || fresh.Phase != t.Phase || fresh.Terminal != t.Terminal || fresh.Run != t.Run || fresh.Attempt != t.Attempt || !reflect.DeepEqual(fresh.Handoff, t.Handoff) || !reflect.DeepEqual(fresh.Progress, t.Progress) || fresh.MergeProblem != t.MergeProblem {
		return errors.New("task ownership or recovery evidence changed; resnapshot required")
	}
	closed := task.State.Name == "Canceled" || task.State.Name == "Cancelled" || task.State.Name == "Duplicate" || (task.State.Name == "Done" && fresh.Owner != machine.OwnerMerge)
	if closed && !DeploymentBarrier(fresh) {
		return errors.New("task was closed before decision")
	}
	if action == Claim || action == Resume || action == Merge || action == githubSquash {
		if err := githubExecutionAllowed(task); err != nil {
			return err
		}
	}
	if action == githubSquash {
		// Existing deployment evidence remains reconcilable, but never grants
		// permission to squash a closed or human-owned ticket.
		if task.NativeState != "open" || fresh.Owner != machine.OwnerMerge {
			return errors.New("task no longer authorizes squash")
		}
	}
	if action == Claim {
		if !task.Complete || task.Blocked() || (task.State.Name != "Todo" && task.State.Name != "Backlog") {
			return errors.New("task no longer claimable")
		}
		ready := false
		for _, l := range task.Labels.Nodes {
			switch l.Name {
			case "ready-for-agent":
				ready = true
			case "auto-impl", "auto-failed":
				return errors.New("task already claimed or failed")
			}
		}
		if !ready {
			return errors.New("task is no longer ready")
		}
	}
	return nil
}

// Labels can be committed before the human handoff comment. They independently
// stop new work, while observation of an already-merged deployment stays allowed.
func githubExecutionAllowed(task issues.Task) error {
	if task.Metadata.ImportHold || task.NativeState != "open" {
		return errors.New("task is held or closed")
	}
	for _, label := range task.Labels.Nodes {
		if label.Name == "auto-failed" {
			return errors.New("failed task requires human recovery")
		}
	}
	return nil
}

func (r *Runtime) verifyGitHubLaunch(ctx context.Context, t Ticket) error {
	c, ok := r.Clients[t.Repo].(*issues.GitHub)
	if !ok {
		return nil
	}
	task, err := c.Read(ctx, t.ID)
	if err != nil {
		return err
	}
	if err = githubExecutionAllowed(task); err != nil {
		return err
	}
	comments, err := c.ListComments(ctx, t.ID)
	if err != nil {
		return err
	}
	var fresh Ticket
	if err = readOwnership(&fresh, comments); err != nil {
		return err
	}
	if fresh.Owner != machine.OwnerExecutor || fresh.Terminal != "-" || fresh.Phase != t.Phase || fresh.Attempt != t.Attempt {
		return errors.New("executor reservation changed before launch")
	}
	return nil
}
