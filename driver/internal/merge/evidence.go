package merge

import "time"

const HandoffPrefix = "merge-handoff-v1: "
const ProgressPrefix = "merge-progress-v1: "

// Handoff is frozen in the Linear ownership comment. Never reconstruct its
// timestamp from mutable issue metadata or infer validation from an open PR.
type Handoff struct {
	Version       int       `json:"version"`
	TicketID      string    `json:"ticket_id"`
	Identifier    string    `json:"identifier"`
	Repository    string    `json:"repository"`
	Unit          string    `json:"unit"`
	PR            int       `json:"pr"`
	Head          string    `json:"head"`
	Baseline      string    `json:"baseline"`
	GateSpec      string    `json:"gate_spec"`
	GateOutput    string    `json:"gate_output"`
	ReviewOutput  string    `json:"review_output"`
	Lane          int       `json:"lane"`
	Run           string    `json:"run"`
	Task          string    `json:"task"`
	Dispatch      string    `json:"dispatch"`
	Worktree      string    `json:"worktree"`
	Attempt       int       `json:"attempt"`
	Corrections   int       `json:"corrections"`
	Infra         int       `json:"infra"`
	CatchupRounds int       `json:"catchup_rounds"`
	EnqueuedAt    time.Time `json:"enqueued_at"`
	QueueID       string    `json:"queue_id"`
}

// Progress is an external receipt, keyed by the immutable handoff. Consumers
// needing deployment recovery must scan these comments independently of active
// issue filtering. #141 emits merge-intent and merged; no deployment is inferred.
type Progress struct {
	Deployment    *DeploymentProgress `json:"deployment,omitempty"`
	Corrections   int                 `json:"corrections,omitempty"`
	CatchupRounds int                 `json:"catchup_rounds,omitempty"`
	Infra         int                 `json:"infra,omitempty"`
	Attempt       int                 `json:"attempt,omitempty"`
	Reason        string              `json:"reason,omitempty"`
	// MainUnknown is allowed only for a merged PR observed without a prior intent.
	MainUnknown bool   `json:"main_unknown,omitempty"`
	Version     int    `json:"version"`
	QueueID     string `json:"queue_id"`
	TicketID    string `json:"ticket_id"`
	Repository  string `json:"repository"`
	PR          int    `json:"pr"`
	Head        string `json:"head"`
	Main        string `json:"main"`
	MergeSHA    string `json:"merge_sha"`
	Phase       string `json:"phase"`
}

// DeploymentProgress extends v1 receipts without changing the frozen Handoff.
// Smoke is stored only in durable evidence; observation views omit the command.
type DeploymentProgress struct {
	Recovery     *DeploymentRecovery `json:"recovery,omitempty"`
	SmokeStarted bool                `json:"smoke_started,omitempty"`
	SmokePassed  bool                `json:"smoke_passed,omitempty"`
	Workflow     string              `json:"workflow,omitempty"`
	Smoke        string              `json:"smoke,omitempty"`
	State        string              `json:"state"`
	Reason       string              `json:"reason,omitempty"`
}

// Recovery records the operator's reconciliation, not inferred credential health.
type DeploymentRecovery struct {
	Action   string    `json:"action"`
	Operator string    `json:"operator"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}
