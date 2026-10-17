// Package machine implements the driver ticket state machine.
package machine

import "fmt"

type Phase string

const (
	PhaseUnclaimed   Phase = "待领"
	PhaseWorker      Phase = "worker 运行中"
	PhaseBranchCheck Phase = "分支检查"
	PhaseGate        Phase = "gate"
	PhaseReview      Phase = "审查"
	PhaseCorrection  Phase = "写 correction"
	PhaseOpenPR      Phase = "开 PR"
	PhaseMergeQueue  Phase = "排队合并"
	PhaseMerged      Phase = "已合并"
	PhaseAwaitHuman  Phase = "等人批"
	PhaseHuman       Phase = "转人" // Historical durable receipts.
	PhaseResolver    Phase = "Agent处置"
)

type Owner string

const (
	OwnerNone     Owner = "无"
	OwnerExecutor Owner = "执行器"
	OwnerMerge    Owner = "合并通道"
	OwnerHuman    Owner = "人" // Historical durable receipts.
	OwnerResolver Owner = "处置Agent"
)

// BoardDone is Orca's default Done column, reused for merged tickets.
const BoardDone = "completed"

// boardColumns gives each phase its own Orca workspace board column, installed
// by `orca-factory board-columns`. The historical 转人 receipt shares 等人批.
var boardColumns = map[Phase]string{
	PhaseUnclaimed:   "driver-unclaimed",
	PhaseWorker:      "driver-worker",
	PhaseBranchCheck: "driver-branch-check",
	PhaseGate:        "driver-gate",
	PhaseReview:      "driver-review",
	PhaseCorrection:  "driver-correction",
	PhaseOpenPR:      "driver-open-pr",
	PhaseMergeQueue:  "driver-merge-queue",
	PhaseResolver:    "driver-resolver",
	PhaseAwaitHuman:  "driver-await-human",
	PhaseHuman:       "driver-await-human",
	PhaseMerged:      BoardDone,
}

// BoardColumn maps a phase to the Orca board column that shows it.
func BoardColumn(p Phase) string {
	return boardColumns[p]
}

type Dependency string

const (
	DepClaude Dependency = "Claude"
	DepCodex  Dependency = "Codex"
	DepGitHub Dependency = "GitHub"
)

type Event string

const (
	EvInfraFailure Event = "infraFailure"
	EvClaim        Event = "claim"
	EvWorkerOK     Event = "workerOk"
	EvWorkerFail   Event = "workerFail"
	EvWorkerStall  Event = "workerStall"
	EvBranchOK     Event = "branchOk"
	EvBranchNo     Event = "branchNo"
	EvGateGreen    Event = "gateGreen"
	EvGateRed      Event = "gateRed"
	EvReviewGreen  Event = "reviewGreen"
	EvReviewRed    Event = "reviewRed"
	EvReviewEmpty  Event = "reviewEmpty"
	EvSessionOK    Event = "sessionOk"
	EvSessionEmpty Event = "sessionEmpty"
	EvOpenPR       Event = "openPr"
	EvMerge        Event = "merge"
	EvConflict     Event = "conflict"
	EvExecCrash    Event = "execCrash"
	EvReadFail     Event = "readFail"
	EvTick         Event = "tick"
	EvRequeue      Event = "requeue"

	// EvWorkerUnstarted is a stopped dispatch whose turn never started: an
	// infra fault, so it is redone through correction without charging it.
	EvWorkerUnstarted Event = "workerUnstarted"
)

type Limits struct{ Corrections, Infra, ExecFailures int }

var DefaultLimits = Limits{Corrections: 3, Infra: 3, ExecFailures: 2}

type State struct {
	Phase         Phase
	Owner         Owner
	Attempt       int
	ExecAlive     bool
	ExecUncertain bool
	WorkerResult  bool
	ManualGlob    bool
	Corrections   int
	Infra         int
	ExecFailures  int
	HumanReason   string
	Paused        map[Dependency]bool
}

// Effect is a side effect the executor or scheduler must perform after a transition,
// e.g. writing an ownership comment. Step itself never performs I/O.
type Effect string

type Rejected struct{ Reason string }

func (r Rejected) Error() string { return "不允许：" + r.Reason }

func Initial() State {
	return State{Phase: PhaseUnclaimed, Owner: OwnerNone, Paused: map[Dependency]bool{}}
}

func (s State) frozen() bool {
	for _, p := range s.Paused {
		if p {
			return true
		}
	}
	return false
}

func copyState(s State) State {
	paused := make(map[Dependency]bool, len(s.Paused))
	for k, v := range s.Paused {
		paused[k] = v
	}
	s.Paused = paused
	return s
}

func Step(in State, ev Event, lim Limits) (State, []Effect, error) {
	s := copyState(in)
	var fx []Effect
	reject := func(reason string) (State, []Effect, error) { return in, nil, Rejected{reason} }
	own := func(o Owner, note string) {
		s.Owner = o
		fx = append(fx, Effect(fmt.Sprintf("write-ownership owner=%s attempt=%d note=%s", o, s.Attempt, note)))
	}
	toResolver := func(reason string) {
		s.Phase, s.HumanReason, s.ExecAlive = PhaseResolver, reason, false
		own(OwnerResolver, reason)
		fx = append(fx, "enqueue-resolver")
	}
	correction := func(cause string) {
		s.Corrections++
		if s.Corrections > lim.Corrections {
			toResolver("correction exhausted: " + cause)
			return
		}
		s.Phase = PhaseCorrection
		fx = append(fx, "start-coordinator-session-for-correction", "escalate-tier")
	}
	infra := func(what string) {
		s.Infra++
		if s.Infra >= lim.Infra {
			toResolver("infra budget exhausted: " + what)
			return
		}
		fx = append(fx, Effect(fmt.Sprintf("retry-step-after-backoff n=%d", s.Infra)))
	}
	startExecutor := func(note string) {
		s.Attempt++
		s.ExecAlive = true
		fx = append(fx, "orca-terminal-create-executor")
		own(OwnerExecutor, note)
	}

	switch ev {
	case EvInfraFailure:
		switch s.Phase {
		case PhaseUnclaimed, PhaseWorker, PhaseBranchCheck, PhaseGate, PhaseReview, PhaseCorrection, PhaseOpenPR:
		default:
			return reject("不在执行器业务阶段")
		}
		if s.frozen() {
			return reject("依赖暂停中")
		}
		infra(string(s.Phase))
	case EvClaim:
		switch {
		case s.Phase != PhaseUnclaimed:
			return reject("票不在待领状态")
		case s.Paused[DepCodex]:
			return reject("Codex 暂停中")
		}
		fx = append(fx, "label-auto-impl-then-read-back")
		startExecutor("claim")
		s.Phase, s.WorkerResult = PhaseWorker, false
		fx = append(fx, "worker-start-with-request-id")
	case EvWorkerOK:
		if s.Phase != PhaseWorker || s.WorkerResult {
			return reject("worker 不在运行或已回报")
		}
		s.WorkerResult = true
		if s.ExecAlive {
			s.Phase = PhaseBranchCheck
			fx = append(fx, "worker-release")
		}
	case EvWorkerFail:
		if s.Phase != PhaseWorker || !s.ExecAlive {
			return reject("没有在运行且有人接收的 worker")
		}
		correction("worker failed")
	case EvWorkerUnstarted:
		if s.Phase != PhaseWorker || !s.ExecAlive {
			return reject("没有在运行且有人接收的 worker")
		}
		infra("worker turn did not start")
		if s.Phase == PhaseWorker {
			s.Phase = PhaseCorrection
			fx = append(fx, "start-coordinator-session-for-correction")
		}
	case EvWorkerStall:
		if s.Phase != PhaseWorker || !s.ExecAlive {
			return reject("没有在运行的 worker")
		}
		if !s.frozen() {
			infra("worker stalled")
		}
	case EvBranchOK, EvBranchNo:
		if s.Phase != PhaseBranchCheck {
			return reject("不在分支检查")
		}
		if ev == EvBranchNo {
			toResolver("worker reported success but branch did not advance")
		} else {
			s.Phase = PhaseGate
		}
	case EvGateGreen, EvGateRed:
		if s.Phase != PhaseGate {
			return reject("不在 gate")
		}
		if ev == EvGateRed {
			correction("gate red")
		} else {
			s.Phase = PhaseReview
			fx = append(fx, "run-reviewer-with-diff-on-stdin")
		}
	case EvReviewGreen, EvReviewRed, EvReviewEmpty:
		if s.Phase != PhaseReview {
			return reject("不在审查")
		}
		if s.Paused[DepClaude] {
			return reject("Claude 暂停中，审查员等待，不降级")
		}
		switch ev {
		case EvReviewGreen:
			s.Phase = PhaseOpenPR
		case EvReviewRed:
			correction("reviewer red")
		default:
			infra("reviewer empty output")
		}
	case EvSessionOK, EvSessionEmpty:
		if s.Phase != PhaseCorrection {
			return reject("不在写 correction")
		}
		if s.Paused[DepClaude] {
			return reject("Claude 暂停中")
		}
		if ev == EvSessionEmpty {
			infra("coordinator session produced no correction.md")
			break
		}
		if s.Paused[DepCodex] {
			return reject("Codex 暂停中")
		}
		s.Phase, s.WorkerResult = PhaseWorker, false
		fx = append(fx, "worker-start-child-task-reuse-worktree")
	case EvOpenPR:
		if s.Phase != PhaseOpenPR {
			return reject("不在开 PR")
		}
		if s.Paused[DepGitHub] {
			return reject("GitHub 暂停中")
		}
		fx = append(fx, "open-pr-rest-idempotent-by-branch")
		s.ExecAlive = false
		if s.ManualGlob {
			toResolver("approval required by repository policy")
		} else {
			s.Phase = PhaseMergeQueue
			own(OwnerMerge, "handoff")
		}
	case EvMerge:
		if s.Phase != PhaseMergeQueue {
			return reject("不在合并通道")
		}
		if s.Paused[DepGitHub] {
			return reject("GitHub 暂停中")
		}
		s.Phase, s.Owner = PhaseMerged, OwnerNone
		fx = append(fx, "squash-merge-read-back", "issue-done", "ledger-append", "worktree-rm")
	case EvConflict:
		if s.Phase != PhaseMergeQueue {
			return reject("不在合并通道")
		}
		startExecutor("merge conflict")
		correction("conflict with main")
	case EvExecCrash:
		if s.Owner != OwnerExecutor || !s.ExecAlive {
			return reject("当前没有活着的执行器")
		}
		s.ExecAlive = false
	case EvReadFail:
		if s.Owner != OwnerExecutor {
			return reject("当前归属不是执行器")
		}
		s.ExecUncertain = true
	case EvTick:
		if s.Owner != OwnerExecutor || s.ExecAlive {
			break
		}
		if s.ExecUncertain {
			s.ExecUncertain = false
			break
		}
		s.ExecFailures++
		if s.ExecFailures >= lim.ExecFailures {
			toResolver("executor failed twice")
			break
		}
		startExecutor("resume")
		if s.WorkerResult {
			s.Phase = PhaseBranchCheck
		}
	case EvRequeue:
		if s.Owner != OwnerHuman {
			return reject("只有归属是人的票才能放回")
		}
		s.Phase, s.Owner, s.HumanReason = PhaseUnclaimed, OwnerNone, ""
		s.Infra, s.ExecFailures, s.Corrections, s.WorkerResult = 0, 0, 0, false
	default:
		return reject("未知事件 " + string(ev))
	}
	return s, fx, nil
}
