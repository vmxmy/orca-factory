package scheduler

import (
	"context"
	"errors"
	"reflect"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

const githubSquash ActionKind = "github-squash"

func GitHubCleanupAllowed(task issues.Task, comments []linear.Comment) bool {
	if !task.Complete || task.Metadata.ImportHold || task.NativeState != "closed" {
		return false
	}
	ticket := Ticket{ID: task.ID, Identifier: task.Identifier}
	if readOwnership(&ticket, comments) != nil {
		return false
	}
	readMergeHandoff(&ticket, comments)
	if ticket.MergeProblem != "" || DeploymentBarrier(ticket) || (ticket.Owner != "" && ticket.Owner != machine.OwnerNone) {
		return false
	}
	return ticket.Handoff == nil || ticket.Progress != nil && ticket.Progress.Phase == "merged" && ticket.Progress.QueueID == ticket.Handoff.QueueID && ticket.Progress.TicketID == task.ID
}

// forgetProjection makes the next snapshot reread a ticket whose cached
// comments failed a fresh decision or are about to be superseded by a write.
func (r *Runtime) forgetProjection(t Ticket) {
	delete(r.projections[r.Clients[t.Repo]], t.ID)
}

// Revalidate the selected ticket immediately before a decision can mutate it.
// The snapshot cache helps discovery only; comments may change independently of
// the issue timestamp, including human cancellation and edited merge receipts.
func (r *Runtime) verifyFreshDecision(ctx context.Context, t Ticket, action ActionKind) error {
	if client, ok := r.Clients[t.Repo].(*linear.Client); ok && action != Claim {
		issue, err := r.verifyFreshOwnership(ctx, client, t)
		if err != nil {
			return err
		}
		if action == Resume && t.Owner == machine.OwnerResolver || action != Resume && action != Merge && action != githubSquash {
			return nil
		}
		if _, _, failed := issueEligibility(issue); failed {
			return errors.New("failed task requires human recovery")
		}
		return nil
	}
	c, ok := r.Clients[t.Repo].(issues.Store)
	if !ok {
		return nil
	}
	task, err := c.Read(ctx, t.ID)
	if err != nil {
		return err
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
	fresh.Resolution, err = readResolution(comments)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh.Resolution, t.Resolution) {
		return errors.New("resolver evidence changed; resnapshot required")
	}
	if fresh.Owner != t.Owner || fresh.Phase != t.Phase || fresh.Terminal != t.Terminal || fresh.Run != t.Run || fresh.Attempt != t.Attempt || !reflect.DeepEqual(fresh.Handoff, t.Handoff) || !reflect.DeepEqual(fresh.Progress, t.Progress) || fresh.MergeProblem != t.MergeProblem {
		return errors.New("task ownership or recovery evidence changed; resnapshot required")
	}
	closed := task.State.Name == "Canceled" || task.State.Name == "Cancelled" || task.State.Name == "Duplicate" || (task.State.Name == "Done" && fresh.Owner != machine.OwnerMerge)
	if closed && !DeploymentBarrier(fresh) {
		return errors.New("task was closed before decision")
	}
	if action == Resolve && (!task.Metadata.Complete || task.Metadata.ImportHold || task.NativeState != "open" && !DeploymentBarrier(fresh)) {
		return errors.New("resolver requires managed open task or historical deployment barrier")
	}
	if action == Claim || action == Resume || action == Merge || action == githubSquash {
		if action == Resume && t.Owner == machine.OwnerResolver {
			return nil
		}
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
	if !task.Complete || task.Metadata.ImportHold || task.NativeState != "open" {
		return errors.New("task is held or closed")
	}
	for _, label := range task.Labels.Nodes {
		if label.Name == "auto-failed" {
			return errors.New("failed task requires human recovery")
		}
	}
	return nil
}

func (r *Runtime) verifyFreshLaunch(ctx context.Context, t Ticket) error {
	client := r.Clients[t.Repo]
	switch c := client.(type) {
	case *linear.Client:
		if t.Owner != machine.OwnerResolver {
			issue, err := r.claimIssue(ctx, c, t)
			if err != nil {
				return err
			}
			if _, _, failed := issueEligibility(issue); failed {
				return errors.New("failed task requires human recovery")
			}
		}
	case issues.Store:
		task, err := c.Read(ctx, t.ID)
		if err != nil {
			return err
		}
		if t.Owner != machine.OwnerResolver {
			if err = githubExecutionAllowed(task); err != nil {
				return err
			}
		} else if !task.Metadata.Complete || task.Metadata.ImportHold || task.NativeState != "open" && !DeploymentBarrier(t) {
			return errors.New("resolver task is held or closed without deployment barrier")
		}
	default:
		return nil
	}
	comments, err := client.ListComments(ctx, t.ID)
	if err != nil {
		return err
	}
	fresh := Ticket{ID: t.ID, Identifier: t.Identifier, Repo: t.Repo}
	if err = readOwnership(&fresh, comments); err != nil {
		return err
	}
	readMergeHandoff(&fresh, comments)
	fresh.Resolution, err = readResolution(comments)
	if err != nil {
		return err
	}
	if fresh.MergeProblem != t.MergeProblem || !reflect.DeepEqual(fresh.Handoff, t.Handoff) || !reflect.DeepEqual(fresh.Progress, t.Progress) || !reflect.DeepEqual(fresh.Resolution, t.Resolution) {
		return errors.New("recovery evidence changed before launch")
	}
	expected := machine.OwnerExecutor
	expectedRun := ""
	if t.Owner == machine.OwnerResolver {
		expected = machine.OwnerResolver
		if t.Run != "" {
			expectedRun = t.Run
		}
	}
	if fresh.Owner != expected || fresh.Terminal != "-" || fresh.Run != expectedRun || fresh.Phase != t.Phase || fresh.Attempt != t.Attempt {
		return errors.New("executor reservation changed before launch")
	}
	return nil
}
