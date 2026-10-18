package scheduler

import (
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

// Validate every durable transition, not just the latest snapshot. A malformed
// historical reset remains a barrier even if later records look consistent.
func deploymentTransition(old, next *merge.Progress) bool {
	n := next.Deployment
	if old == nil || old.Deployment == nil {
		if n == nil {
			return true
		}
		// A recorded same-queue policy of "no deployment" is frozen too.
		if old != nil && old.QueueID == next.QueueID && (old.Phase == "merge-intent" || old.Phase == "merged") {
			return false
		}
		return n.State == "pending" && !n.SmokeStarted && !n.SmokePassed && n.Recovery == nil
	}
	d := old.Deployment
	if n == nil || old.QueueID != next.QueueID || old.TicketID != next.TicketID || old.Repository != next.Repository || old.PR != next.PR || old.Head != next.Head || (old.MergeSHA != "" && old.MergeSHA != next.MergeSHA) || d.Workflow != n.Workflow || d.Smoke != n.Smoke || d.Self != n.Self {
		return false
	}
	if !sameDeploymentRecovery(d.Recovery, n.Recovery) {
		r := n.Recovery
		if r == nil || strings.TrimSpace(r.Operator) == "" || strings.TrimSpace(r.Reason) == "" || strings.ContainsAny(r.Operator+r.Reason, "\r\n") || r.At.IsZero() || (d.Recovery != nil && !r.At.After(d.Recovery.At)) || (d.State != "paused" && d.State != "smoke-intent") {
			return false
		}
		switch r.Action {
		case "confirm-passed":
			return n.State == "passed" && n.SmokeStarted == d.SmokeStarted && n.SmokePassed == d.SmokePassed
		case "retry-workflow":
			return d.Workflow != "" && !d.SmokeStarted && n.State == "pending" && !n.SmokeStarted && !n.SmokePassed
		case "retry-smoke":
			return d.Smoke != "" && d.SmokeStarted && n.State == retrySmokeState(n.Self) && !n.SmokeStarted && !n.SmokePassed
		default:
			return false
		}
	}
	// Only a new explicit recovery may clear execution evidence.
	if d.SmokeStarted && !n.SmokeStarted || d.SmokePassed && !n.SmokePassed {
		return false
	}
	switch d.State {
	case "pending":
		switch n.State {
		case "pending", "paused":
			return !n.SmokeStarted && !n.SmokePassed
		case "deploy-intent":
			return n.Self && !n.SmokeStarted && !n.SmokePassed
		case "smoke-intent":
			return !n.Self && n.Smoke != "" && n.SmokeStarted && !n.SmokePassed
		case "passed":
			return n.Smoke == "" && !n.SmokeStarted && !n.SmokePassed
		}
	case "deploy-intent":
		switch n.State {
		case "deploy-intent", "paused":
			return !n.SmokeStarted && !n.SmokePassed
		case "smoke-intent":
			return n.SmokeStarted && !n.SmokePassed
		}
	case "smoke-intent":
		if n.State == "passed" {
			return n.SmokeStarted && n.SmokePassed
		}
		return (n.State == "smoke-intent" || n.State == "paused") && n.SmokeStarted && !n.SmokePassed
	case "paused", "passed":
		return n.State == d.State && n.SmokeStarted == d.SmokeStarted && n.SmokePassed == d.SmokePassed
	}
	return false
}

func sameDeploymentRecovery(a, b *merge.DeploymentRecovery) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Action == b.Action && a.Operator == b.Operator && a.Reason == b.Reason && a.At.Equal(b.At)
}

// A self deployment retries only its smoke; pending would reinstall it.
func retrySmokeState(self bool) string {
	if self {
		return "deploy-intent"
	}
	return "pending"
}
