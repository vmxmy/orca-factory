// Package merge owns one FIFO merge lane per repository. A driver keeps one
// Channel for its lifetime and calls Next again when a waiting dependency changes.
// Release and feature PRs deliberately have no separate queues or priorities.
package merge

import (
	"context"
	"errors"
	"strings"
	"sync"
)

var ErrGateRed = errors.New("merged-result gate red")

var ErrMainAdvanced = errors.New("main advanced during merge validation")

// ErrReviewRequired is returned by Snapshot when policy needs an independent
// resolver review before squash. Next dequeues the PR with Review so a review
// never blocks later PRs in the same repository.
var ErrReviewRequired = errors.New("lane 2 requires resolver review")

// ErrHumanRequired is returned by Snapshot when policy forbids any agent
// approval (lane 3: secrets, deploy files, or any AGENT_LANE3_GLOBS match).
// Next dequeues the PR with HumanDirect so lane 3 never blocks later PRs in
// the same repository; lane 3 is excluded from resolver and automerge
// authorization, so no resolver review is attempted first.
var ErrHumanRequired = errors.New("lane 3 requires human approval")

// SnapshotError names which Snapshot step failed. A GitHub implementation
// builds Reason from its own driver-authored text or from an external error
// already passed through an existing sanitizer; Next never reads raw external
// text itself, so a credential or response body can never reach the reason.
type SnapshotError struct {
	Step, Reason string
}

func (e *SnapshotError) Error() string { return e.Step + ": " + e.Reason }

// snapshotFailureReason reports the failed step when the implementation
// identified one, and a generic reason otherwise rather than risk
// unsanitized text from an error it did not construct.
func snapshotFailureReason(err error) string {
	var se *SnapshotError
	if errors.As(err, &se) && se.Step != "" && se.Reason != "" {
		return se.Error()
	}
	return "unspecified"
}

type Request struct {
	Repository     string // canonical owner/repository; identical for every local checkout
	Path           string // local checkout used by Git implementations
	Number         int
	Ticket         string // ticket identifier, for logs only
	QueueID        string // frozen handoff queue_id; with the head it identifies a gate verdict
	DeployWorkflow string // AGENT_DEPLOY_WORKFLOW
	SmokeCommand   string // AGENT_SMOKE_CMD
	CatchupRounds  int    // carry Result.Rounds when the scheduler resubmits a correction
}

type Snapshot struct {
	Head, Main, CIHead string
	MergeableState     string
	CIGreen, Behind    bool
	// CIFailed and FailedChecks describe CIHead's check-runs when they finished
	// with at least one failing conclusion. Only trusted when CIHead == Head.
	CIFailed     bool
	FailedChecks []string
}

// GitHub supplies fresh snapshots and a head-pinned squash with authoritative
// merged read-back. Squash rechecks main before writing, returns ErrMainAdvanced
// when validation became stale, and returns the actual squash commit SHA.
// All external operations in implementations must use runx and bounded contexts.
type GitHub interface {
	Snapshot(context.Context, Request) (Snapshot, error)
	Squash(context.Context, Request, string, string) (string, error)
}

// Git computes git merge-tree --write-tree for immutable head/main IDs and runs
// a callback in a temporary worktree containing that exact result. WithWorktree
// owns cleanup even on callback error or cancellation; it never checks out head.
// Cleanup errors preserve the callback error and wrap ErrWorktreeCleanup.
type Git interface {
	MergeTree(context.Context, Request, string, string) (tree string, conflict bool, err error)
	WithWorktree(context.Context, Request, string, string, string, func(context.Context, string) error) error
}

type DeploymentState string

const (
	DeploymentPending DeploymentState = "pending"
	DeploymentPassed  DeploymentState = "passed"
	DeploymentFailed  DeploymentState = "deploy-failed"
	SmokeFailed       DeploymentState = "smoke-failed"
)

// Deployment checks the configured workflow at the squash SHA, then the smoke
// command. Passed means both succeeded; no clock or polling sleeps are needed.
type Deployment interface {
	Check(context.Context, Request, string) (DeploymentState, error)
}
type Status string

const (
	Empty       Status = "empty"
	Waiting     Status = "waiting"
	Merged      Status = "merged"
	Correction  Status = "scheduler-new-executor"
	Human       Status = "human"
	Review      Status = "review"
	HumanDirect Status = "human-direct"
	Paused      Status = "paused"
	// Validating means a merged-result gate for the repository is still
	// running. It is not a decision: nothing is persisted or charged.
	Validating Status = "validating"
)

type Result struct {
	Status              Status
	Request             Request
	MergeSHA, Reason    string
	Corrections, Rounds int // correction delta and accumulated catch-up rounds
	// Main is the main a red merged-result gate validated; the snapshot of
	// the round that reports it may already see a newer main.
	Main string
}

type entry struct {
	request Request
	rounds  int
}
type deployed struct {
	request Request
	sha     string
}
type lane struct {
	mu                           sync.Mutex
	busy                         bool
	queue                        []*entry
	previous                     *deployed
	paused                       bool
	deployWorkflow, smokeCommand string
}
type Channel struct {
	mu          sync.Mutex
	lanes       map[string]*lane
	github      GitHub
	git         Git
	gate        Gate
	validations *Validations
	deploy      Deployment
}

// New binds a lane to validations, which outlive the Channel: a scheduler
// builds a Channel per inspection and finds its running gates there.
func New(h GitHub, g Git, gate Gate, v *Validations, d Deployment) *Channel {
	return &Channel{lanes: make(map[string]*lane), github: h, git: g, gate: gate, validations: v, deploy: d}
}
func (c *Channel) lane(repo string) *lane {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.lanes[repo]
	if l == nil {
		l = &lane{}
		c.lanes[repo] = l
	}
	return l
}

// Enqueue is the arrival ordering point. Repeated delivery of a queued PR does
// not move it or reset its rounds. Caller retains results across process restart.
func (c *Channel) Enqueue(r Request) error {
	if r.Repository == "" || r.Number <= 0 || r.CatchupRounds < 0 {
		return errors.New("invalid merge request")
	}
	l := c.lane(r.Repository)
	l.mu.Lock()
	defer l.mu.Unlock()
	// Deployment policy is repository-wide and survives requests that omit it.
	if (l.deployWorkflow != "" && r.DeployWorkflow != "" && l.deployWorkflow != r.DeployWorkflow) || (l.smokeCommand != "" && r.SmokeCommand != "" && l.smokeCommand != r.SmokeCommand) {
		return errors.New("conflicting repository deployment policy")
	}
	if r.DeployWorkflow != "" {
		l.deployWorkflow = r.DeployWorkflow
	}
	if r.SmokeCommand != "" {
		l.smokeCommand = r.SmokeCommand
	}
	for _, e := range l.queue {
		if e.request.Number == r.Number {
			return nil
		}
	}
	l.queue = append(l.queue, &entry{request: r, rounds: r.CatchupRounds})
	return nil
}

// Next performs at most one merge attempt. Waiting never allows a later PR to
// overtake the head. Different repositories can be processed concurrently.
// A smoke/deploy failure pauses until an operator creates a recovered channel;
// reconstructing one requires restoring the outstanding deployment first.
// A Merged result may accompany ErrWorktreeCleanup; the merge is still complete.
func (c *Channel) Next(ctx context.Context, repo string) (result Result, resultErr error) {
	var cleanupErr error
	defer func() {
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()
	l := c.lane(repo)
	l.mu.Lock()
	if l.paused {
		l.mu.Unlock()
		return Result{Status: Paused, Reason: "deployment requires human recovery"}, nil
	}
	if l.busy {
		l.mu.Unlock()
		return Result{Status: Waiting, Reason: "repository lane busy"}, nil
	}
	l.busy = true
	var e *entry
	if len(l.queue) > 0 {
		e = l.queue[0]
		e.request.DeployWorkflow = l.deployWorkflow
		e.request.SmokeCommand = l.smokeCommand
	}
	previous := l.previous
	l.mu.Unlock()
	defer func() { l.mu.Lock(); l.busy = false; l.mu.Unlock() }()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if previous != nil {
		if c.deploy == nil {
			return Result{}, errors.New("deployment checker required")
		}
		state, err := c.deploy.Check(ctx, previous.request, previous.sha)
		if err != nil {
			return Result{}, errors.New("deployment check unavailable")
		}
		switch state {
		case DeploymentPassed:
			l.mu.Lock()
			l.previous = nil
			l.mu.Unlock()
		case DeploymentFailed, SmokeFailed:
			l.mu.Lock()
			l.paused = true
			l.mu.Unlock()
			return Result{Status: Human, Request: previous.request, MergeSHA: previous.sha, Reason: string(state)}, nil
		default:
			return Result{Status: Waiting, Request: previous.request, MergeSHA: previous.sha, Reason: "waiting for deployment and smoke"}, nil
		}
	}
	if e == nil {
		return Result{Status: Empty}, nil
	}
	r := Result{Request: e.request, Rounds: e.rounds}
	finish := func(status Status, reason string) (Result, error) {
		r.Status, r.Reason, r.Rounds = status, reason, e.rounds
		if status == Merged || status == Correction || status == Human || status == Review || status == HumanDirect {
			l.mu.Lock()
			l.queue = l.queue[1:]
			l.mu.Unlock()
		}
		return r, nil
	}
	// notReady escalates a confirmed, finished check-run failure at the pinned
	// head to Correction instead of leaving the ticket waiting forever; CI
	// still running, or any other not-ready cause, stays Waiting.
	notReady := func(s Snapshot, reason string) (Result, error) {
		if ciFailed(s) {
			r.Corrections = 1
			return finish(Correction, ciFailureReason(s))
		}
		return finish(Waiting, reason)
	}
	stale := func() (Result, error) {
		e.rounds++
		if e.rounds >= 2 {
			return finish(Human, "main advanced for two rounds")
		}
		return finish(Waiting, "main advanced; validate again")
	}
	if e.rounds >= 2 {
		return finish(Human, "catch-up budget exhausted")
	}
	if c.github == nil {
		return r, errors.New("GitHub required")
	}
	if (e.request.DeployWorkflow != "" || e.request.SmokeCommand != "") && c.deploy == nil {
		return r, errors.New("deployment checker required")
	}
	s, err := c.github.Snapshot(ctx, e.request)
	if errors.Is(err, ErrReviewRequired) {
		return finish(Review, err.Error())
	}
	if errors.Is(err, ErrHumanRequired) {
		return finish(HumanDirect, err.Error())
	}
	if err != nil {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		return finish(Waiting, "merge snapshot unavailable: "+snapshotFailureReason(err))
	}
	if s.Head == "" || s.Main == "" {
		return r, errors.New("incomplete merge snapshot")
	}
	if s.Behind {
		if c.git == nil {
			return r, errors.New("merge validation dependencies required")
		}
		tree, conflict, err := c.git.MergeTree(ctx, e.request, s.Head, s.Main)
		if err != nil {
			return r, errors.New("merge tree unavailable")
		}
		if conflict {
			e.rounds++
			r.Corrections = 1
			if e.rounds >= 2 {
				return finish(Human, "conflict after two catch-up rounds")
			}
			return finish(Correction, "return to scheduler to start a new executor for conflict")
		}
		if tree == "" {
			return r, errors.New("empty merge tree")
		}
		if !mergeReady(s) {
			return notReady(s, "head CI or mergeability not ready")
		}
		if c.gate == nil {
			return r, errors.New("merge gate required")
		}
		verdict, validated, err := c.validations.Validate(ctx, Candidate{Request: e.request, Head: s.Head, Main: s.Main, Tree: tree}, c.git, c.gate)
		if verdict == "" {
			return r, err
		}
		// A cleanup failure is reported beside the verdict without spending a correction.
		if errors.Is(err, ErrWorktreeCleanup) {
			cleanupErr = ErrWorktreeCleanup
		}
		switch verdict {
		case VerdictRunning:
			return finish(Validating, "merged-result gate running")
		case VerdictStale:
			return stale()
		case VerdictRed:
			r.Corrections, r.Main = 1, validated
			return finish(Correction, "merged-result gate failed; return to scheduler")
		}
	} else if !mergeReady(s) {
		return notReady(s, "head CI or mergeability not ready")
	} else if !c.validations.Idle(Candidate{Request: e.request, Head: s.Head, Main: s.Main}) {
		// One gate or squash per repository at a time, even after the queue head changed.
		return finish(Validating, "previous merged-result gate running")
	}
	fresh, err := c.github.Snapshot(ctx, e.request)
	if err != nil {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		return finish(Waiting, "merge recheck unavailable: "+snapshotFailureReason(err))
	}
	checked := Candidate{Request: e.request, Head: s.Head, Main: s.Main}
	if fresh.Main != s.Main {
		c.validations.Forget(checked)
		return stale()
	}
	if fresh.Head != s.Head {
		return finish(Waiting, "head validation changed")
	}
	if !mergeReady(fresh) {
		return notReady(fresh, "head validation changed")
	}
	sha, err := c.github.Squash(ctx, e.request, s.Head, s.Main)
	if errors.Is(err, ErrMainAdvanced) {
		c.validations.Forget(checked)
		return stale()
	}
	if err != nil {
		return r, errors.New("squash merge unconfirmed")
	}
	if sha == "" {
		return r, errors.New("squash commit missing")
	}
	r.MergeSHA = sha
	if e.request.DeployWorkflow != "" || e.request.SmokeCommand != "" {
		l.mu.Lock()
		l.previous = &deployed{request: e.request, sha: sha}
		l.mu.Unlock()
	}
	return finish(Merged, "")
}
func mergeReady(s Snapshot) bool {
	return s.MergeableState == "clean" && s.CIGreen && s.CIHead == s.Head
}

// ciFailed reports a confirmed, finished CI failure at the pinned head. A
// stale CIHead must never report a failure for the wrong commit.
func ciFailed(s Snapshot) bool {
	return s.CIHead == s.Head && s.CIFailed
}

func ciFailureReason(s Snapshot) string {
	if len(s.FailedChecks) == 0 {
		return "head CI failed"
	}
	return "head CI failed: " + strings.Join(s.FailedChecks, ", ")
}
