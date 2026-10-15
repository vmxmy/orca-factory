package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

// freshExecution rereads the selected backend before executor writes. Discovery
// data never authorizes a dispatch, and auto-failed work belongs to a resolver.
func (e *Executor) freshExecution(ctx context.Context, t Ticket) error {
	c, ok := e.Issues.(issues.Store)
	if !ok {
		return nil
	}
	task, err := c.Read(ctx, t.ID)
	if err != nil {
		return err
	}
	if task.ID != t.ID || task.Identifier != t.Identifier || !task.Complete || task.Metadata.ImportHold || task.NativeState != "open" {
		return errors.New("task held, closed or identity changed")
	}
	repository := c.Repository()
	for _, label := range task.Labels.Nodes {
		if label.Name == "auto-failed" {
			return errors.New("failed task requires resolver")
		}
	}
	comments, err := e.Issues.ListComments(ctx, t.ID)
	if err != nil {
		return err
	}
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	owner := ""
	terminal, run := "", ""
	frozen := map[string]string{}
	var handoff *merge.Handoff
	var progress *merge.Progress
	var resolution *resolver.Record
	for _, comment := range comments {
		if strings.HasPrefix(comment.Body, "转人: ") {
			owner = string(machine.OwnerHuman)
		}
		for _, line := range strings.Split(comment.Body, "\n") {
			if raw, ok := strings.CutPrefix(line, merge.HandoffPrefix); ok {
				var value merge.Handoff
				if json.Unmarshal([]byte(raw), &value) != nil || value.Version != 1 || value.TicketID != t.ID || value.Identifier != t.Identifier || value.Repository != repository || value.QueueID == "" {
					return errors.New("invalid frozen execution handoff")
				}
				if prior, exists := frozen[value.QueueID]; exists && prior != raw {
					return errors.New("conflicting frozen execution handoff")
				}
				frozen[value.QueueID] = raw
				handoff = &value
			}
			if raw, ok := strings.CutPrefix(line, merge.ProgressPrefix); ok {
				var value merge.Progress
				if json.Unmarshal([]byte(raw), &value) != nil || value.Version != 1 || value.TicketID != t.ID || value.Repository != repository || value.QueueID == "" {
					return errors.New("invalid frozen execution progress")
				}
				progress = &value
			}
			if raw, ok := strings.CutPrefix(line, resolver.Prefix); ok {
				var value resolver.Record
				if json.Unmarshal([]byte(raw), &value) != nil || value.Version != 1 || value.Attempt < 1 {
					return errors.New("invalid resolver execution evidence")
				}
				resolution = &value
			}
			if strings.HasPrefix(line, "归属: ") {
				fields := strings.Fields(strings.TrimPrefix(line, "归属: "))
				if len(fields) == 0 {
					return errors.New("invalid ownership comment")
				}
				owner, terminal, run = fields[0], "", ""
				for _, field := range fields[1:] {
					key, value, ok := strings.Cut(field, "=")
					if !ok {
						continue
					}
					switch key {
					case "term":
						terminal = value
					case "run":
						run = value
					case "attempt", "scheduler-attempt":
						number, parseErr := strconv.Atoi(value)
						if parseErr != nil || number < 0 {
							return errors.New("invalid ownership attempt")
						}
					}
				}
			}
		}
	}
	if resolution != nil && resolution.Phase != "applied" {
		return errors.New("unsettled resolver evidence does not authorize executor")
	}
	if progress != nil && (handoff == nil || frozen[progress.QueueID] == "" || progress.QueueID != handoff.QueueID && progress.Phase != "correction" && progress.Phase != "human" || progress.QueueID == handoff.QueueID && (progress.PR != handoff.PR || progress.Head != handoff.Head || progress.Corrections < handoff.Corrections || progress.Infra < handoff.Infra || progress.Attempt < handoff.Attempt || progress.CatchupRounds < handoff.CatchupRounds)) {
		return errors.New("invalid frozen execution progress identity or budget")
	}
	if owner == string(machine.OwnerExecutor) && terminal != "" && terminal != "-" && terminal != e.Config.Terminal {
		cp, readErr := readCheckpoint(e.Config, t)
		if readErr != nil {
			return readErr
		}
		if cp == nil || cp.Run == "" || run != cp.Run || (cp.Task == "" && !cp.Pending) {
			return errors.New("executor reservation belongs to another terminal")
		}
		terminals, readErr := e.Orca.TerminalList(ctx)
		if readErr != nil {
			return readErr
		}
		for _, row := range terminals {
			if row.Handle == terminal {
				return errors.New("previous executor terminal remains active")
			}
		}
		tasks, readErr := e.taskList(ctx, cp.Run)
		if readErr != nil {
			return readErr
		}
		found := false
		for _, task := range tasks {
			if task.ID == cp.Task && task.RunID == cp.Run && (cp.Dispatch == "" || task.DispatchID == "" || task.DispatchID == cp.Dispatch) {
				found = true
			}
		}
		if !found {
			return errors.New("replacement executor recovery identity unconfirmed")
		}
		copy := *cp
		matched, readErr := recoverDispatch(tasks, &copy, "agent/"+t.Identifier+"/"+e.Config.Unit)
		if readErr != nil {
			return readErr
		}
		if matched == nil || matched.Task != cp.Task || matched.Run != cp.Run {
			return errors.New("replacement executor recovery identity unconfirmed")
		}

	}
	if owner != "" && owner != string(machine.OwnerExecutor) && (!e.Config.Merging || owner != string(machine.OwnerMerge)) {
		return errors.New("task ownership no longer permits executor")
	}
	return nil
}

func (e *Executor) pullRequestBody(ctx context.Context, t Ticket) (string, error) {
	c, ok := e.Issues.(issues.Store)
	if !ok {
		return "Fixes " + t.Identifier + "\n\n" + t.Todo, nil
	}
	if err := e.freshExecution(ctx, t); err != nil {
		return "", err
	}
	task, err := c.Read(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if task.URL == "" || task.ID != t.ID || task.Identifier != t.Identifier {
		return "", errors.New("GitHub issue reference unavailable")
	}
	return fmt.Sprintf("关联票: %s\n\nCanonical identifier: %s\nCanonical ID: %s\n\n%s", task.URL, t.Identifier, t.ID, t.Todo), nil
}
