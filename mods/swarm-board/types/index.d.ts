/** Token totals for one agent or one ticket; cache = cache-read (Claude) or cached-input (Codex). */
export type Tokens = { in: number; out: number; cache: number }
/** Estimated USD. `known` is false when every token came from a model with no price. `partial` when some did. */
export type Cost = { usd: number; known: boolean; partial: boolean }
export type GateStep = { name: string; sec: number; rc: number }
/** The last gate run of one unit: sum of step seconds, verdict state, and the steps that had timestamps. */
export type Gate = { state: 'green' | 'red' | 'running' | 'none'; sec: number; steps: GateStep[] }
export type Ci = { done: number; total: number; state: 'green' | 'red' | 'running' | 'none'; min: number }
/**
 * Parsed precheckResult of the latest automation run that carried one. Scheduled scans
 * attach a precheckResult; a manual run may not, so this stays undefined until a scan has run.
 */
export type Precheck = {
  runNumber: number
  exitCode: number
  timedOut: boolean
  error: string
  /** Ticket ids on the latest `claimable=` line, in order; empty when no candidate was accepted. */
  claimable: string[]
  /** `in_flight=` value: tickets still holding a concurrency slot. */
  inFlight: number
  /** `max=` value: the pool size the pre-check enforced. */
  max: number
  /** `skip <id>: <reason>` lines from stderr, keyed by ticket id. */
  skips: Record<string, string>
  /** First stderr line that is not `linear_calls=…`; empty when stderr had nothing else. */
  firstStderrLine: string
}
export type Ticket = {
  id: string; title: string; state: string; labels: string[]; priority: number; updatedAt: string
  /** When the coordinator posted the "已领取" comment; empty when not found. */
  claimedAt: string
  pr?: number
  gate?: Gate
  tokens?: Tokens
  cost?: Cost
}
export type Pr = {
  number: number; title: string; updatedAt: string; branch: string; sha: string
  adds: number; dels: number; files: number; behind: number
  ci: Ci
}
export type Agent = {
  ticket: string; role: 'coordinator' | 'worker'; name: string; phase: string; since: string
  path: string
  durationMin: number
  model: string
  tokens?: Tokens
  cost?: Cost
  /** ISO of the agent terminal's last output; empty when no terminal matched. */
  lastActive: string
  comment?: string
}
export type Board = {
  at: string
  error: string
  maxInFlight: number
  inFlight: Ticket[]
  queue: Ticket[]
  failed: Ticket[]
  doneToday: Ticket[]
  prs: Pr[]
  agents: Agent[]
  lastRun: string
  stats: string[]
  /** Sum of est. cost of today's in-flight and done tickets; 0 when nothing is known. */
  todayCost: number
  /** True once an `orca automations list` call confirmed presence/absence; sticky across failures. */
  automationKnown: boolean
  /** ms epoch of the last refresh whose Linear read succeeded; 0 before the first one. */
  lastSuccessAt: number
  /** Latest scheduled precheck we could parse; sticky across failed/empty automation reads (#45 rule). */
  lastPrecheck?: Precheck
  /** Automation's nextRunAt (ms epoch) for the countdown; undefined until first read. */
  nextRunAt?: number
  /** ms epoch of the latest automation run of any kind (manual or scheduled); undefined until first read. */
  lastRunAt?: number
}
export type View = 'swarm' | 'task'

declare module 'claude-code' {
  interface PluginState {
    'swarm-board': { board: Board; view: View; showQueue: boolean; detail: boolean; now: number; lastTickAt: number }
  }
}

/** Last-good phase observations live only in the board atom. */
export type TicketPhase = '📋 拆分' | '📤 派工' | '🔨 实现' | '📬 待收尾' | '🧪 gate' | '🔍 审查（推断）' | '🚦 CI/合并'
export type PhaseObservation = { phase: TicketPhase; since: string; lastActive?: string }
