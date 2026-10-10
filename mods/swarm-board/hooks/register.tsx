import { atom, read, update } from 'claude-code'
import type { Register } from 'claude-code'
import { buildGraph, renderGraph } from './views/graph'
import type { GraphView } from './views/graph'
import { readGraphSnapshot, __resetGraphCacheForTests } from './linear-cache'
import { ledgerRows } from './view-data'
import { buildThroughput, renderThroughput } from './views/throughput'
import type { ThroughputData, Run as ThroughputRun } from './views/throughput'
import { buildFailures, renderFailures } from './views/failures'
import type { FailureView } from './views/failures'
import { WATCH_POLL_MS, shouldReload } from './snapshot-watch'
import { parseDriverStatus, pickStatusSource } from './driver-status'
import type { DriverStatus } from './driver-status'
import { driverHeadText } from './driver-head'
import { statusBarRows } from './status-bar'
import { aggregateUsage, createUsageReader, ledgerUsage } from './usage'
import type { TicketUsage, UsageRecord, LedgerUsage, WorktreeToTicket } from './usage'

declare function h(type: unknown, props?: Record<string, unknown> | null, ...children: unknown[]): unknown

import type { Agent as BaseAgent, Board as BaseBoard, Ci, Cost, Gate, GateStep, LinearUsage, Pr, Precheck, Ticket as BaseTicket, View, TicketPhase, PhaseObservation } from '../types'

type Ticket = BaseTicket & { usage?: TicketUsage; durationMin?: number; url?: string; number?: number; canonicalId?: string }
type FailedTicket = Ticket & { handoff?: string }
type Board = Omit<BaseBoard, 'failed'> & { backend?: 'github' | 'linear'; repository?: string; failed: FailedTicket[]; automation?: { id: string; enabled: boolean }; phases?: Record<string, PhaseObservation>; coordinatorAlive?: boolean; driver?: DriverStatus; graph?: GraphView | null; graphNotice?: string; throughput?: ThroughputData; failures?: FailureView; metricRuns?: ThroughputRun[]; repositoryPaths?: string[] }


type Agent = BaseAgent & { requiresAction?: boolean }
type WorkerProjection = { provider?: { model?: string } | null; attention?: { requiresAction?: boolean } }
type WorktreeCard = { path: string; workspaceStatus: string; comment: string }

type PhaseTask = { status: string }
type PhaseData = {
  run?: { id: string } | null
  tasks?: readonly PhaseTask[]
  gate?: Gate
  lane?: number
  pr?: number
  previous?: TicketPhase
  driver?: string
}

// The driver's executor writes "driver: <阶段>" onto the worker worktree at every transition.
const DRIVER_PHASES: Record<string, TicketPhase> = {
  'worker 运行中': '🔨 实现', '写 correction': '🔨 实现', '分支检查': '🧪 gate', 'gate': '🧪 gate',
  '审查': '🔍 审查（推断）', '开 PR': '🚦 CI/合并', '排队合并': '🚦 CI/合并',
}
const driverPhase = (comment?: string): TicketPhase | undefined =>
  comment?.startsWith('driver: ') ? DRIVER_PHASES[comment.split('\n', 1)[0].slice('driver: '.length).trim()] : undefined

export const ticketPhase = (ticket: Ticket, data: PhaseData): TicketPhase => {
  if (data.pr || ticket.pr) return '🚦 CI/合并'
  const reported = driverPhase(data.driver)
  if (reported) return reported
  const gate = data.gate ?? ticket.gate
  if (gate?.state === 'running') return '🧪 gate'
  const tasks = data.tasks ?? []
  if (tasks.some(t => t.status === 'dispatched')) return '🔨 实现'
  if (tasks.some(t => t.status === 'pending' || t.status === 'ready')) return '📤 派工'
  if (gate?.state === 'green' && data.lane === 2) return '🔍 审查（推断）'
  if (tasks.some(t => t.status === 'completed')) return '📬 待收尾'
  return data.previous ?? (data.run ? '📤 派工' : '📋 拆分')
}

const PANE = 'swarm-board'
// Must match mods/swarm-board/.claude-plugin/plugin.json. The hook sandbox has no plugin-root
// path (no import.meta.url, no plugin context, no node fs), so plugin.json cannot be read at
// runtime; keep this literal in lockstep with the manifest when bumping the version.
const MOD_VERSION = '0.4.0'
const PLUGIN_ID = 'swarm-board@orca-factory'
const INSTALLED_PLUGINS_PATH = '/.claude/plugins/installed_plugins.json'
const ENV = 'scripts/agent-gates/agent-graph.env'
const REFRESH_MS = 60_000
const TIMEOUT_MS = 20_000
const MAX_TICKETS = 10 // in-flight tickets that get per-ticket calls (task-list + comments)
const MAX_PRS = 6 // open PRs that get the three gh detail calls
const MAX_DONE = 4 // done-today tickets that get one task-list call (for today's cost)
// Ledger stats is slow and depends on comment reads; cache the output for ten minutes.
const LEDGER_TTL_MS = 600_000
// Linear API hourly limit the badge is measured against (see #45 rate-limit handling).
const LINEAR_HOURLY_LIMIT = 2500
const LINEAR_HOURLY_WARN = 1750 // 70% → yellow
const LINEAR_HOURLY_DANGER = 2250 // 90% → red
// Shared counter file the pipeline writes; swarm board only reads it.
const LINEAR_COUNTER_FILE = 'orca-factory-linear-calls'
// Snapshot the precheck pipeline writes; the board reads it to skip its own `orca linear …` reads.
const SNAPSHOT_FRESH_MS = 600_000
const LINEAR_FALLBACK_THROTTLE_MS = 300_000
// Resolved once per module load from the shell, since the module has no process.env.
let HOME = ''
let TMP_DIR = ''
let SNAPSHOT_FILE = ''
// ms epoch of the last live `orca linear list-issues` dispatched by refresh(); 0 before the first one.
let lastLinearFallbackAt = 0
const workspaces = () => HOME + '/orca/workspaces'
const AUTOMATION_NAME = (repo: string) => 'agent-linear-scan (' + repo + ')' // install.sh names it so

// One glyph per state, everywhere: ● working  ◐ waiting  ▲ needs a person  ✓ done  · queued
const G = { work: '●', wait: '◐', human: '▲', done: '✓', queued: '·' }
const SYM: Record<string, string> = { green: G.done, red: G.human, running: G.wait, none: '' }
// Lane emoji — one per lane, used in heads and as the card's status-emoji fallback.
const LANE_EMOJI = { queue: '📥', impl: '🔨', gate: '🔍', ci: '🚦', human: '🙋', done: '✅' }
// Card status emoji, taken from gate/CI result; falls back to the lane emoji when nothing ran.
const STATUS_EMOJI: Record<string, string> = { green: '🟢', red: '🔴', running: '🟡' }
// Display width where an emoji code point counts as 2 columns.
const dw = (s: string): number => {
  let w = 0
  for (const c of [...s]) w += /\p{Extended_Pictographic}/u.test(c) ? 2 : 1
  return w
}
// Truncate by display columns; appends '…' when it had to drop code points.
const cutW = (s: string, n: number): string => {
  if (n <= 0) return ''
  if (dw(s) <= n) return s
  let w = 0
  let out = ''
  for (const c of [...s]) {
    const cw = /\p{Extended_Pictographic}/u.test(c) ? 2 : 1
    if (w + cw > n - 1) break
    w += cw
    out += c
  }
  return out + '…'
}
// Right-pad a possibly-emoji string to n display columns.
const padW = (s: string, n: number): string => s + ' '.repeat(Math.max(0, n - dw(s)))

const STALE_MS = 3 * 60_000 // header turns yellow when the last successful Linear read is older than this
const empty: Board = { at: '', error: '', maxInFlight: 0, inFlight: [], queue: [], failed: [], doneToday: [], prs: [], agents: [], lastRun: '', stats: [], todayCost: 0, automationKnown: false, lastSuccessAt: 0 }
const board = atom({ plugin: 'swarm-board', key: 'board' } as const, empty)
const view = atom({ plugin: 'swarm-board', key: 'view' } as const, 'task' as View)
const showQueue = atom({ plugin: 'swarm-board', key: 'showQueue' } as const, false)
const detail = atom({ plugin: 'swarm-board', key: 'detail' } as const, false)
const nowAtom = atom({ plugin: 'swarm-board', key: 'now' } as const, 0)
const lastTickAt = atom({ plugin: 'swarm-board', key: 'lastTickAt' } as const, 0)
// Tracks whether the pane is currently open in this plugin VM. `command.run` for `/swarm`
// flips it on; the `ui.close` hook flips it off when the person closes the pane. The 60 s
// refresh timer skips its callback while this is false, so a closed pane stops calling
// `orca`, `gh` or `orca linear …` (the local Linear counter file is still read because
// that cost is local).
let paneOpen = false
let refreshing = false

const parseEnv = (text: string): Record<string, string> =>
  Object.fromEntries(
    text.split('\n').map(l => l.replace(/\s+#.*$/, '').trim()).filter(l => /^[A-Z_]+=/.test(l))
      .map(l => { const i = l.indexOf('='); return [l.slice(0, i), l.slice(i + 1).replace(/^'(.*)'$/, '$1')] }),
  )

const ticket = (i: any): Ticket => ({
  id: i.identifier, title: i.title ?? '', state: i.state?.type ?? '', url: i.url, number: i.number, canonicalId: i.id,
  labels: (i.labels ?? []).map((l: any) => l.name), priority: i.priority ?? 0, updatedAt: i.updatedAt ?? '', claimedAt: '',
})

// Orca mixes three clock formats: ISO with Z, "YYYY-MM-DD HH:MM:SS" (UTC, no T), and epoch ms.
const ts = (v: string | number | null | undefined): number => {
  if (v == null || v === '') return NaN
  if (typeof v === 'number') return v
  if (/^\d+$/.test(v)) return Number(v)
  return Date.parse(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(v) ? v.replace(' ', 'T') + 'Z' : v)
}
const minutes = (from: string, to: number) => { const t = ts(from); return Number.isFinite(t) ? Math.max(0, Math.round((to - t) / 60_000)) : 0 }
const mins = (m: number) => (m < 60 ? m + ' min' : Math.floor(m / 60) + ' h ' + (m % 60) + ' min')
const age = (iso: string, now: number) => (iso ? mins(minutes(iso, now)) : '')
const secs = (s: number) => (s < 60 ? s + 's' : Math.floor(s / 60) + 'm' + String(s % 60).padStart(2, '0') + 's')
const tok = (n: number) => (n < 1000 ? String(n) : n < 1e6 ? (n / 1e3).toFixed(n < 1e4 ? 1 : 0) + 'k' : (n / 1e6).toFixed(1) + 'M')
const usd = (c?: Cost) => (!c ? '' : !c.known ? 'est. $?' : 'est. $' + c.usd.toFixed(2) + (c.partial ? '+' : ''))
const shortModel = (m: string) => m.replace(/^.*\//, '').replace(/^claude-/, '')
const json = (s: string) => { try { return JSON.parse(s) } catch { return null } }
const base = (p: string) => p.slice(p.lastIndexOf('/') + 1)

// Semver-ish compare: 0.3.11 > 0.3.9 numerically. Non-numeric tails count as 0.
const verGT = (a: string, b: string): boolean => {
  const pa = a.split('.'), pb = b.split('.')
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = (Number(pa[i]) || 0) - (Number(pb[i]) || 0)
    if (d) return d > 0
  }
  return false
}

// ---- ledger stats cache + Linear hourly usage ----------------------------------------------------
// Ledger stats triggers Linear reads (list-issues + per-ticket comments). Caching it for ten minutes
// keeps the open board under the Linear hourly budget; the refresh button never busts this cache.
let ledgerCache: { at: number; stats: string[] } | null = null
// Timestamps (ms) of every `orca linear …` call the board itself just made, trimmed to the last hour.
const boardLinearCalls: number[] = []

let readUsage = createUsageReader()
let ticketLedgerCache: { at: number; rows: Record<string, LedgerUsage>; comments: ReturnType<typeof deepComments> } | null = null

// Test-only reset for module-level caches so each test case starts clean.
export const __resetSwarmBoardCachesForTests = () => {
  __resetGraphCacheForTests()
  ledgerCache = null
  boardLinearCalls.length = 0
  HOME = ''
  TMP_DIR = ''
  SNAPSHOT_FILE = ''
  driverStatusFile = ''
  driverCache = undefined
  lastLinearFallbackAt = 0
  readUsage = createUsageReader()
  ticketLedgerCache = null
  paneOpen = false
}

// Test-only command dispatched by fixtures that trigger several refreshes in a row,
// so each test's throttle starts clean inside the plugin VM (test imports and the
// plugin VM have separate module instances; module-level resets from the test file
// do not reach the VM, but a dispatched command runs in the VM itself).
export const RESET_LINEAR_THROTTLE_COMMAND = 'swarm:__reset-linear-throttle-for-tests__'

// Command the test runner dispatches to simulate the person closing the pane: the matching
// `command.run` handler calls `$.ui.close(...)` so the engine runs the plugin's `ui.close`
// hook in the plugin VM, which flips `paneOpen` for the timer callback that reads it there.
// The testing harness exposes no direct way to send a `ui.close` event.
export const SIMULATE_CLOSE_COMMAND = 'swarm:__simulate-close-for-tests__'

const usageFields = (usage?: TicketUsage) => usage ? {
  usage, model: usage.model,
  tokens: { in: usage.input + usage.cacheWrite, out: usage.output, cache: usage.cacheRead },
  cost: { usd: usage.usd, known: usage.known, partial: usage.known && usage.partial },
} : {}

async function loadUsage($: any, paths: string[]): Promise<UsageRecord[]> {
  if (paths.length === 0) return []
  return readUsage({
    files: async () => {
      const r = await $.process.run(['bash', '-c',
        'for dir in "$@"; do if [ -d "$dir" ]; then find "$dir" -type f -name "*.jsonl" || exit; fi; done',
        'usage', ...paths.map(p => HOME + '/.claude/projects/' + p.replace(/[^a-zA-Z0-9]/g, '-')), HOME + '/.codex/sessions'], { timeoutMs: TIMEOUT_MS })
      if (r.exitCode !== 0) throw new Error(r.stderr || '会话目录读取失败')
      return r.stdout.split('\n').filter((p: string) => p && (p.includes('/.claude/projects/') || /\/rollout-[^/]+\.jsonl$/.test(p)))
    },
    mtime: async path => (await $.fs.stat(path))?.mtimeMs,
    read: async path => {
      // Project only accounting fields, avoiding the SDK's 4 MiB transcript read limit.
      const r = await $.process.run(['jq', '-Rc', 'fromjson? | select(.message.usage or .type == "session_meta" or .type == "turn_context" or .payload.type == "token_count") | {type,cwd,uuid,message:(.message | if . then {id,model,usage} else null end),payload:(.payload | if . then {type,cwd,model,info} else null end)}', path], { timeoutMs: TIMEOUT_MS })
      if (r.exitCode !== 0) throw new Error(r.stderr || '会话用量读取失败')
      return r.stdout
    },
  }, paths)
}

// ---- precheck -------------------------------------------------------------------------------
// Scheduled runs attach a precheckResult; a manual run may not, so this returns null for those.
// A `claimable=<csv> in_flight=<n> max=<n>` line anywhere in stdout drives queue status (prune
// output may sit above it on the same stdout); stderr "skip ZII-<n>: <reason>" drives per-card
// reasons. The run is `uncertain` when the precheck could not read data: exit code 3, a
// `不确定: <reason>` stderr line, timeout, non-empty error, or any non-zero exit without a
// `claimable=` line. Exit code 1 with a `claimable=` line is a normal "no eligible tickets"
// result. Only lines emitted by precheck itself feed `firstStderrLine` and `uncertainReason`:
// `不确定: …`, `skip ZII-<n>: …`, or a token after ` failed: ` (e.g. `linear_network_error`);
// prune output, `skip <path>`, and `linear_calls=` lines are ignored. The reason priority is
// stderr `不确定:` text, then the first ` failed: ` token, then `退出码 <code>` when uncertain.
const parsePrecheck = (r: any): Precheck | null => {
  const pr = r?.precheckResult
  if (!pr || typeof pr !== 'object') return null
  const stdout = String(pr.stdout ?? '')
  const stderr = String(pr.stderr ?? '')
  const exitCode = Number(pr.exitCode ?? 0)
  const timedOut = pr.timedOut === true
  const error = String(pr.error ?? '')
  const claimMatch = /^claimable=([^\s]*)/m.exec(stdout)
  const hasClaimable = claimMatch !== null
  const claimRaw = claimMatch?.[1] ?? ''
  const claimable = claimRaw ? claimRaw.split(',').filter(Boolean) : []
  const inFlight = Number(/\bin_flight=(\d+)/.exec(stdout)?.[1] ?? 0)
  const max = Number(/\bmax=(\d+)/.exec(stdout)?.[1] ?? 0)
  const skips: Record<string, string> = {}
  const precheckLines: string[] = []
  let stderrUncertain = ''
  let failedReason = ''
  for (const raw of stderr.split('\n')) {
    const line = raw.trim()
    if (!line) continue
    const uncertainMatch = /^不确定:\s*(.*)$/.exec(line)
    if (uncertainMatch) {
      if (!stderrUncertain) stderrUncertain = uncertainMatch[1]
      precheckLines.push(line)
      continue
    }
    const skipMatch = /^skip ([A-Z]+-\d+): (.*)$/.exec(line)
    if (skipMatch) {
      skips[skipMatch[1]] = skipMatch[2]
      precheckLines.push(line)
      continue
    }
    const failedMatch = / failed:\s*(\S+)/.exec(line)
    if (failedMatch) {
      if (!failedReason) failedReason = failedMatch[1]
      precheckLines.push(line)
      continue
    }
    // Everything else — `linear_calls=`, `prune_linear_calls=`, `skip <path>…`, `prune: …`,
    // and any other noise — is ignored so the surfaced reason only comes from precheck itself.
  }
  const uncertain = timedOut || error !== '' || exitCode === 3 || stderrUncertain !== ''
    || (exitCode !== 0 && !hasClaimable)
  const uncertainReason = stderrUncertain || failedReason || (uncertain ? '退出码 ' + exitCode : '')
  return {
    runNumber: Number(r.runNumber ?? 0),
    exitCode,
    timedOut,
    error,
    claimable,
    inFlight,
    max,
    skips,
    firstStderrLine: precheckLines[0] ?? '',
    uncertain,
    uncertainReason,
  }
}

// ---- gate log --------------------------------------------------------------------------------
// Lines: "--- [<step>] <ISO> (cd …)" opens a step, "--- [<step>] exit <rc> <ISO>" closes it. Old logs without ISO give no steps.
const parseGate = (text: string): Gate => {
  const i = text.lastIndexOf('=== gate ')
  const seg = i < 0 ? text : text.slice(i)
  const steps: GateStep[] = []
  const open: Record<string, number> = {}
  for (const l of seg.split('\n')) {
    let m = /^--- \[([^\]]+)\] (\d{4}-\d\d-\d\dT\S+) \(cd /.exec(l)
    if (m) { open[m[1]] = Date.parse(m[2]); continue }
    m = /^--- \[([^\]]+)\] exit (\d+) (\d{4}-\d\d-\d\dT\S+)/.exec(l)
    if (m && open[m[1]] !== undefined) {
      steps.push({ name: m[1], sec: Math.max(0, Math.round((Date.parse(m[3]) - open[m[1]]) / 1000)), rc: Number(m[2]) })
      delete open[m[1]]
    }
  }
  const v = /^VERDICT\s+(\w+)/m.exec(seg)
  return { state: v ? (v[1] === 'green' ? 'green' : 'red') : 'running', sec: steps.reduce((a, s) => a + s.sec, 0), steps }
}
const mergeGate = (a: Gate | undefined, b: Gate): Gate => {
  if (!a) return b
  const rank = { red: 3, running: 2, green: 1, none: 0 }
  return { state: rank[b.state] > rank[a.state] ? b.state : a.state, sec: a.sec + b.sec, steps: [...a.steps, ...b.steps] }
}

// ---- CI -------------------------------------------------------------------------------------
const ciOf = (runs: any[], now: number): Ci => {
  if (!Array.isArray(runs) || runs.length === 0) return { done: 0, total: 0, state: 'none', min: 0 }
  const done = runs.filter(r => r.status === 'completed').length
  const red = runs.some(r => r.conclusion === 'failure' || r.conclusion === 'timed_out')
  const state = red ? 'red' : done < runs.length ? 'running' : 'green'
  const starts = runs.map(r => Date.parse(r.started_at ?? '')).filter(n => !Number.isNaN(n))
  const ends = runs.map(r => (r.status === 'completed' ? Date.parse(r.completed_at ?? '') : now)).filter(n => !Number.isNaN(n))
  const min = starts.length && ends.length ? Math.max(0, Math.round((Math.max(...ends) - Math.min(...starts)) / 60_000)) : 0
  return { done, total: runs.length, state, min }
}

// A worktree path hidden in an Orca object: "<repoId>::<path>@@…" or a bare path under the workspaces dir.
const deepPath = (o: any, depth = 0): string => {
  if (depth > 3 || o === null || o === undefined) return ''
  if (typeof o === 'string') return o.includes(workspaces()) ? o.replace(/^[^:]*::/, '').replace(/@@.*$/, '') : ''
  if (typeof o === 'object') for (const v of Object.values(o)) { const p = deepPath(v, depth + 1); if (p) return p }
  return ''
}
// Comments live somewhere in the issue JSON: the first array whose items have body + createdAt.
const deepComments = (o: any, depth = 0): any[] => {
  if (depth > 4 || o === null || typeof o !== 'object') return []
  if (Array.isArray(o)) return o.length && o.every(x => x && typeof x.body === 'string' && x.createdAt) ? o : o.flatMap(x => deepComments(x, depth + 1))
  for (const v of Object.values(o)) { const c = deepComments(v, depth + 1); if (c.length) return c }
  return []
}

type OrcaResult = { ok: boolean; data: any }

// Read the shared `${TMPDIR:-/tmp}/orca-factory-linear-calls` counter: one line per Linear call
// written by the pipeline (precheck / prune / ledger / coordinator). Keeps only lines inside the
// last 3600 seconds; a missing or unreadable file surfaces as `known: false`.
async function readLinearUsage($: any, now: number): Promise<LinearUsage> {
  if (!TMP_DIR) {
    try {
      const r = await $.process.run(['sh', '-c', 'printf %s "${TMPDIR:-/tmp}"'], { timeoutMs: TIMEOUT_MS })
      TMP_DIR = (r?.exitCode === 0 ? String(r.stdout ?? '') : '').trim() || '/tmp'
    } catch { TMP_DIR = '/tmp' }
  }
  const cutoffMs = now - 3600_000
  while (boardLinearCalls.length && boardLinearCalls[0] < cutoffMs) boardLinearCalls.shift()
  const board = boardLinearCalls.length
  const bySource: Record<string, number> = {}
  let known = false
  try {
    const text = await $.fs.read(TMP_DIR + '/' + LINEAR_COUNTER_FILE)
    known = true
    const cutoffSec = Math.floor(now / 1000) - 3600
    for (const raw of String(text ?? '').split('\n')) {
      const line = raw.trim()
      if (!line) continue
      const m = /^(\d+)\s+(\S+)/.exec(line)
      if (!m) continue
      const sec = Number(m[1])
      if (!Number.isFinite(sec) || sec < cutoffSec) continue
      bySource[m[2]] = (bySource[m[2]] ?? 0) + 1
    }
  } catch { known = false }
  const fromFile = Object.values(bySource).reduce((a, b) => a + b, 0)
  return { known, total: fromFile + board, bySource, board }
}

// Highest version of this mod listed in `~/.claude/plugins/installed_plugins.json`.
// Returns '' when the file is missing, unreadable, or has no entry for this plugin.
async function readInstalledVersion($: any, home: string): Promise<string> {
  if (!home) return ''
  try {
    const data = json(await $.fs.read(home + INSTALLED_PLUGINS_PATH))
    let max = ''
    for (const e of (data?.plugins?.[PLUGIN_ID] ?? []) as any[]) {
      const v = typeof e?.version === 'string' ? e.version : ''
      if (v && (max === '' || verGT(v, max))) max = v
    }
    return max
  } catch { return '' }
}

type Snapshot = {
  generatedAt: number
  issues: any[]
  comments: Record<string, any[]>
  blockedBy?: Record<string, string[]>
}

async function readSnapshot($: any): Promise<Snapshot | null> {
  try {
    const parsed = json(await $.fs.read(SNAPSHOT_FILE))
    if (parsed && typeof parsed === 'object'
        && Number.isFinite(parsed.generatedAt)
        && Array.isArray(parsed.issues)
        && parsed.comments && typeof parsed.comments === 'object') {
      return parsed
    }
  } catch { return null }
  return null
}

async function refreshSnapshot($: any, handoffs: Map<string, string>) {
  try {
    const env = parseEnv(await $.fs.read(ENV))
    if (env.AGENT_ISSUE_BACKEND === 'github') return
  } catch { return }
  const snapshot = await readSnapshot($)
  if (!snapshot) return
  const now = Date.now()
  const snapshotAgeMs = Math.max(0, now - snapshot.generatedAt * 1000)
  if (snapshotAgeMs > SNAPSHOT_FRESH_MS) return
  const old = await read($, board)
  const previous = new Map([...old.inFlight, ...old.doneToday].map(t => [t.id, t]))
  const all: Ticket[] = snapshot.issues.map(ticket).map(t => {
    const cached = previous.get(t.id)
    const comments = snapshot.comments[t.id] ?? []
    const claimedAt = comments.filter(c => String(c.body).trim().startsWith('已领取'))
      .map(c => c.createdAt).sort().pop() ?? ''
    return { ...t, tokens: cached?.tokens, cost: cached?.cost, gate: cached?.gate, pr: cached?.pr, usage: cached?.usage, durationMin: cached?.durationMin, claimedAt }
  })
  const failed = all.filter(t => t.labels.includes('auto-failed') && t.state !== 'completed' && t.state !== 'canceled')
    .map(t => {
      const handoff = (snapshot.comments[t.id] ?? []).filter(c => /^(?:Agent处置|转人):/.test(String(c.body).trim()))
        .sort((a, b) => ts(b.createdAt) - ts(a.createdAt))[0]?.body
      if (handoff) handoffs.set(t.id, handoff)
      return { ...t, handoff: handoffs.get(t.id) }
    })
  const inFlight = all.filter(t => t.labels.includes('auto-impl') && t.state === 'started')
  const phases = Object.fromEntries(inFlight.map(t => [t.id, old.phases?.[t.id] ?? {
    phase: ticketPhase(t, { pr: t.pr }), since: t.claimedAt || t.updatedAt,
  }]))
  const dayStart = new Date(now); dayStart.setHours(0, 0, 0, 0)
  const b: Board = {
    ...old, inFlight, failed, phases,
    queue: all.filter(t => t.labels.includes('ready-for-agent') && t.state === 'unstarted')
      .sort((a, b) => (a.priority || 9) - (b.priority || 9)),
    doneToday: all.filter(t => t.state === 'completed' && Date.parse(t.updatedAt) >= dayStart.getTime()),
    issueStates: Object.fromEntries(all.map(t => [t.id, t.state])),
    blockedBy: snapshot.blockedBy,
    snapshotAgeMs,
    lastSuccessAt: now,
  }
  await update($, board, () => b)
  $.ui.status(G.work + ' ' + b.inFlight.length + '/' + b.maxInFlight + ' · ' + G.wait + ' ' + b.prs.length + (b.failed.length ? ' · ' + G.human + ' ' + b.failed.length : '') + ' · ' + G.queued + ' ' + b.queue.length)
}

async function refresh($: any, handoffs: Map<string, string>, previous: Board): Promise<Board> {
  let old = previous
  const now = Date.now()
  const at = new Date(now).toTimeString().slice(0, 5)
  if (!HOME) {
    try {
      const r = await $.process.run(['sh', '-c', 'printf %s "$HOME"'], { timeoutMs: TIMEOUT_MS })
      HOME = r?.exitCode === 0 ? String(r.stdout ?? '').trim() : ''
    } catch { HOME = '' }
  }
  if (!SNAPSHOT_FILE) {
    try {
      const r = await $.process.run(['sh', '-c', 'printf %s "${AGENT_SNAPSHOT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-snapshot.json}"'], { timeoutMs: TIMEOUT_MS })
      SNAPSHOT_FILE = (r?.exitCode === 0 ? String(r.stdout ?? '') : '').trim() || '/tmp/orca-factory-linear-snapshot.json'
    } catch { SNAPSHOT_FILE = '/tmp/orca-factory-linear-snapshot.json' }
  }
  const installedVersion = await readInstalledVersion($, HOME)
  let text = ''
  try { text = await $.fs.read(ENV) } catch {
    const linearUsage = await readLinearUsage($, now)
    return { ...old, at, coordinatorAlive: undefined, linearUsage, installedVersion, error: 'No ' + ENV + ' here. Open a repo that uses the pipeline.' }
  }
  const env = parseEnv(text)
  const github = env.AGENT_ISSUE_BACKEND === 'github'
  const backend = github ? 'github' : 'linear'
  const repository = env.AGENT_GH_REPO ?? ''
  if (old.repository !== undefined && (old.repository !== repository || old.backend !== backend)) {
    old = empty
    handoffs.clear()
    ledgerCache = null
    ticketLedgerCache = null
    lastLinearFallbackAt = 0
    readUsage = createUsageReader()
    __resetGraphCacheForTests()
  }
  if (env.AGENT_ISSUE_BACKEND && !['github', 'linear'].includes(env.AGENT_ISSUE_BACKEND)) return { ...old, at, error: '未知执行账本配置' }
  const activeView = await read($, view)
  // Snapshot the pipeline's precheck writes; freshness ≤ 10 min means we skip every live
  // `orca linear …` call this refresh. A missing, corrupt, or stale snapshot sends us back
  // to the live path, but the module throttles that path to one actual read per 5 min.
  const snapshot = github ? null : await readSnapshot($)
  const snapshotAgeMs = snapshot ? Math.max(0, now - snapshot.generatedAt * 1000) : undefined
  const useSnapshot = snapshotAgeMs !== undefined && snapshotAgeMs <= SNAPSHOT_FRESH_MS
  const linearThrottled = !github && !useSnapshot && lastLinearFallbackAt > 0 && now - lastLinearFallbackAt < LINEAR_FALLBACK_THROTTLE_MS
  const repoDir = workspaces() + '/' + base(env.AGENT_GH_REPO ?? '')
  // Every external call is serial, bounded by TIMEOUT_MS. `run` returns stdout on exit 0, '' otherwise.
  // `runOrca` tracks success: a non-zero exit, unparseable JSON, or an envelope with ok=false (e.g. Linear rate-limit)
  // leaves ok=false so the caller can preserve the last-good slice instead of blanking the board.
  // Count every `orca linear …` call the board makes, regardless of which wrapper fires it:
  // `list-issues` is by far the biggest contributor and it rides on runOrca, so excluding it
  // would understate the 看板 column and defeat the purpose of this badge.
  const trackLinear = (argv: string[]) => {
    if (argv[0] === 'orca' && argv[1] === 'linear') boardLinearCalls.push(Date.now())
  }
  const run = async (argv: string[]): Promise<string> => {
    trackLinear(argv)
    try {
      const r = await $.process.run(argv, { timeoutMs: TIMEOUT_MS })
      return r.exitCode === 0 ? r.stdout : ''
    } catch { return '' }
  }
  const runJson = async (argv: string[]) => json(await run(argv))
  const runOrca = async (argv: string[]): Promise<OrcaResult> => {
    trackLinear(argv)
    let r: { stdout: string; exitCode: number } | null = null
    try { r = await $.process.run(argv, { timeoutMs: TIMEOUT_MS }) } catch { return { ok: false, data: null } }
    if (!r || r.exitCode !== 0) return { ok: false, data: null }
    const d = json(r.stdout)
    if (d === null) return { ok: false, data: null }
    if (d && typeof d === 'object' && 'ok' in d && d.ok === false) return { ok: false, data: d }
    return { ok: true, data: d }
  }
  const repositoryList = await run(['git', 'worktree', 'list', '--porcelain', '-z'])
  const discoveredPaths = repositoryList.split('\0').filter(field => field.startsWith('worktree ')).map(field => field.slice(9))
  const repositoryPaths = discoveredPaths.length ? discoveredPaths : old.repositoryPaths ?? []
  const has = (t: Ticket, l: string) => t.labels.includes(l)

  // Three paths feed `all`: (a) a fresh snapshot — reuse its issues, no live Linear call;
  // (b) throttled — skip Linear, treat as a soft failure so the old board carries over;
  // (c) live — call list-issues, remember the attempt timestamp so later refreshes throttle.
  let issuesResp: OrcaResult
  if (github) {
    issuesResp = await runOrca(['bash', 'scripts/agent-gates/agent-graph.sh', 'issues', 'list-issues', '--json'])
    if (issuesResp.data?.result?.complete !== true || !Array.isArray(issuesResp.data?.result?.issues)) issuesResp = { ok: false, data: null }
  } else if (useSnapshot) {
    issuesResp = { ok: true, data: { result: { issues: snapshot!.issues } } }
  } else if (linearThrottled) {
    issuesResp = { ok: false, data: null }
  } else {
    lastLinearFallbackAt = now
    issuesResp = await runOrca(['orca', 'linear', 'list-issues', '--team', env.AGENT_LINEAR_TEAM, '--project', env.AGENT_LINEAR_PROJECT, '--json'])
  }
  const linearOk = issuesResp.ok
  const all: Ticket[] = linearOk ? (issuesResp.data?.result?.issues ?? []).map(ticket) : []
  const error = linearOk || linearThrottled ? '' : github ? 'GitHub 数据不可用，沿用旧数据' : 'Linear 限流，沿用旧数据'
  const inFlight = all.filter(t => has(t, 'auto-impl') && t.state === 'started')
  // Comment reads for the ticket are shared between the handoff pass (≤3 failed tickets)
  // and the in-flight claimedAt loop, so an in-flight+failed ticket is fetched at most once.
  const commentsByTicket = new Map<string, ReturnType<typeof deepComments>>()
  const readComments = async (id: string) => {
    const cached = commentsByTicket.get(id)
    if (cached) return cached
    if (github) {
      const fetched = deepComments(await runJson(['bash', 'scripts/agent-gates/agent-graph.sh', 'issues', 'issue', id, '--comments', '--json']))
      commentsByTicket.set(id, fetched)
      return fetched
    }
    if (useSnapshot) {
      const fetched = snapshot!.comments[id] ?? []
      commentsByTicket.set(id, fetched)
      return fetched
    }
    if (linearThrottled) {
      commentsByTicket.set(id, [])
      return []
    }
    const fetched = deepComments(await runJson(['orca', 'linear', 'issue', id, '--comments', '--json']))
    commentsByTicket.set(id, fetched)
    return fetched
  }
  const failed: FailedTicket[] = all.filter(t => has(t, 'auto-failed') && t.state !== 'completed' && t.state !== 'canceled')
    .map(t => ({ ...t, handoff: handoffs.get(t.id) }))
  for (const t of failed.slice(0, 3)) {
    const comments = await readComments(t.id)
    const handoff = comments.filter(c => /^(?:Agent处置|转人):/.test(String(c.body).trim()))
      .sort((a, b) => ts(b.createdAt) - ts(a.createdAt))[0]?.body
    if (handoff) { handoffs.set(t.id, handoff); t.handoff = handoff }
  }
  const dayStart = new Date(now); dayStart.setHours(0, 0, 0, 0)
  const doneToday = all.filter(t => t.state === 'completed' && Date.parse(t.updatedAt) >= dayStart.getTime())

  // Open PRs with their size, drift from main and CI state. On failure keep the last-good list.
  const prsResp = await runOrca(['gh', 'api', 'repos/' + env.AGENT_GH_REPO + '/pulls?state=open&per_page=30'])
  let prs: Pr[]
  if (prsResp.ok && Array.isArray(prsResp.data)) {
    prs = prsResp.data.filter((p: any) => /\b[A-Z]+-\d+\b/.test(p.title)).map((p: any): Pr => ({
      number: p.number, title: p.title, updatedAt: p.updated_at, branch: p.head?.ref ?? '', sha: p.head?.sha ?? '',
      adds: 0, dels: 0, files: 0, behind: 0, ci: { done: 0, total: 0, state: 'none', min: 0 },
    }))
    for (const p of prs.slice(0, MAX_PRS)) {
      const d = await runJson(['gh', 'api', 'repos/' + env.AGENT_GH_REPO + '/pulls/' + p.number])
      if (d) { p.adds = d.additions ?? 0; p.dels = d.deletions ?? 0; p.files = d.changed_files ?? 0; p.sha = d.head?.sha ?? p.sha; p.branch = d.head?.ref ?? p.branch }
      if (p.branch) { const c = await runJson(['gh', 'api', 'repos/' + env.AGENT_GH_REPO + '/compare/main...' + p.branch]); p.behind = c?.behind_by ?? 0 }
      if (p.sha) { const c = await runJson(['gh', 'api', 'repos/' + env.AGENT_GH_REPO + '/commits/' + p.sha + '/check-runs?per_page=50']); p.ci = ciOf(c?.check_runs, now) }
    }
  } else {
    prs = old.prs
  }
  const prOf = (id: string) => prs.find(p => new RegExp('\\b' + id + '\\b').test(p.title))

  // Automation lookup. Only a successful + non-empty list distinguishes "not installed" from "unknown".
  const autosResp = await runOrca(['orca', 'automations', 'list', '--json'])
  const autosArr: any[] = autosResp.ok ? (autosResp.data?.result?.automations ?? []) : []
  const listTrustworthy = autosResp.ok && autosArr.length > 0
  let automation: Board['automation']
  let automationKnown: boolean
  let nextRunAt: number | undefined
  if (autosResp.ok) {
    const auto = autosArr.find(a => a.name === AUTOMATION_NAME(base(env.AGENT_GH_REPO ?? '')))
    if (auto?.id) {
      automation = { id: String(auto.id), enabled: Boolean(auto.enabled) }
      automationKnown = true
      const n = Number(auto.nextRunAt ?? NaN)
      if (Number.isFinite(n) && n > 0) nextRunAt = n
      else nextRunAt = old.nextRunAt
    }
    else if (listTrustworthy) { automation = undefined; automationKnown = true; nextRunAt = undefined }
    else { automation = old.automation; automationKnown = old.automationKnown; nextRunAt = old.nextRunAt }
  } else {
    automation = old.automation
    automationKnown = old.automationKnown
    nextRunAt = old.nextRunAt
  }

  // Automation runs: last run for the header, and each run's coordinator worktree + dispatch time.
  const autoId = automation?.id ?? ''
  const runsResp = autoId ? await runOrca(['orca', 'automations', 'runs', '--id', autoId, '--json']) : { ok: false, data: null }
  const autoRuns: any[] = runsResp.ok ? (runsResp.data?.result?.runs ?? []) : []
  const r0 = autoRuns[0]
  const lastRun = runsResp.ok
    ? (r0 ? 'run ' + r0.runNumber + ' · ' + String(r0.status ?? '').replace('_', ' ') + ' · ' + new Date(r0.scheduledFor ?? r0.dispatchedAt ?? now).toTimeString().slice(0, 5) : '')
    : old.lastRun
  // Latest run of any kind, for the "N 分钟没有新运行" warning. A failed/empty read keeps the old value.
  let lastRunAt: number | undefined = old.lastRunAt
  if (runsResp.ok && r0) {
    const t = ts(r0.scheduledFor ?? r0.dispatchedAt ?? r0.startedAt ?? r0.created_at ?? '')
    if (Number.isFinite(t)) lastRunAt = t
  }
  // Latest run that carried a precheckResult. Manual runs may not; walk forward until one is found.
  let lastPrecheck: Precheck | undefined = old.lastPrecheck
  if (runsResp.ok) {
    for (const r of autoRuns) {
      const parsed = parsePrecheck(r)
      if (parsed) { lastPrecheck = parsed; break }
    }
  }
  const autoByTime = autoRuns
    .map(a => ({ path: String(a.workspaceId ?? '').replace(/^[^:]*::/, ''), at: String(a.dispatchedAt ?? a.startedAt ?? a.scheduledFor ?? '') }))
    .filter(a => a.path.includes('/') && a.at).sort((a, b) => b.at.localeCompare(a.at))
  // The coordinator worktree of an orchestration run: a path inside the run object, else the automation run dispatched just before it.
  const coordinatorOf = (r: any) => {
    const inRun = deepPath(r)
    if (inRun) return { path: inRun, at: autoByTime.find(a => inRun.startsWith(a.path))?.at ?? r.created_at ?? '' }
    const created = r.created_at ?? ''
    const a = autoByTime.find(x => x.at <= created) ?? autoByTime[0]
    return { path: a?.path ?? '', at: a?.at ?? created }
  }

  const orcaRunsResp = await runOrca(['orca', 'orchestration', 'run-list', '--json'])
  const orcaRuns: any[] = orcaRunsResp.ok ? (orcaRunsResp.data?.result?.runs ?? []) : []
  const worktreeResp = await runOrca(['orca', 'worktree', 'ps', '--json'])
  const cardByIssue = new Map<string, WorktreeCard>()
  if (worktreeResp.ok) {
    for (const w of worktreeResp.data?.result?.worktrees ?? []) {
      const identifier = github ? /^issue: ([A-Za-z0-9][A-Za-z0-9._-]*)$/m.exec(String(w.comment ?? ''))?.[1] : w.linkedLinearIssue
      if (!identifier || !w.path) continue
      const card: WorktreeCard = { path: w.path, workspaceStatus: w.workspaceStatus ?? '', comment: w.comment ?? '' }
      if (!cardByIssue.has(identifier)) cardByIssue.set(identifier, card)
    }
  }
  const runFor = (id: string) => orcaRuns.filter(x => (x.objective ?? '').includes(id)).sort((a, b) => (b.created_at ?? '').localeCompare(a.created_at ?? ''))[0]

  // Agent terminals: last output per worktree path.
  const terminalsResp = await runOrca(['orca', 'terminal', 'list', '--json'])
  const lastOut: Record<string, string> = {}
  if (terminalsResp.ok) {
    for (const t of terminalsResp.data?.result?.terminals ?? []) {
      if (!t.agentIdentity || !t.worktreePath || !t.lastOutputAt) continue
      if (!lastOut[t.worktreePath] || lastOut[t.worktreePath] < t.lastOutputAt) lastOut[t.worktreePath] = t.lastOutputAt
    }
  }
  // Failed ps is unknown, never evidence that no coordinator is alive.
  let coordinatorAlive: boolean | undefined
  try {
    const ps = await $.process.run(['ps', '-axo', 'pid=,stat=,command='], { timeoutMs: TIMEOUT_MS })
    if (ps.exitCode === 0 && ps.stdout.trim()) {
      coordinatorAlive = ps.stdout.split('\n').some((line: string) => {
        const row = /^\s*\d+\s+(\S+)\s+(.+)$/.exec(line)
        return !!row && !row[1].includes('Z') && (row[2].includes('scripts/agent-gates/COORDINATOR') || row[2].includes('orca-factory-driver serve'))
      })
    }
  } catch { coordinatorAlive = undefined }
  const phases: Record<string, PhaseObservation> = { ...old.phases }
  const agentsOk = orcaRunsResp.ok && worktreeResp.ok

  // Agents: one coordinator per Orca run whose objective names an in-flight ticket, plus its worker tasks.
  const agents: Agent[] = []
  const ticketPaths: Record<string, string[]> = {}
  const gates: Record<string, Gate | undefined> = {}
  const workersOf = async (runId: string, path = ''): Promise<any[]> => {
    const tasks: any[] = (await runJson(['orca', 'orchestration', 'task-list', '--run', runId, '--json']))?.result?.tasks ?? []
    return tasks.map(k => ({ ...k, path: deepPath(k) || repoDir + '/' + (k.display_name ?? k.id) }))
  }
  for (const t of inFlight.slice(0, MAX_TICKETS)) {
    const r = runFor(t.id)
    const paths: string[] = []
    // When the coordinator posted "已领取". Shares the handoff pass's cache for failed+inflight tickets.
    const comments = await readComments(t.id)
    t.claimedAt = comments.filter(c => String(c.body).trim().startsWith('已领取')).map(c => c.createdAt).sort().pop() ?? ''
    if (!r) {
      agents.push({ ticket: t.id, role: 'coordinator', name: 'claiming', phase: 'split', since: t.updatedAt, path: '', durationMin: minutes(t.updatedAt, now), model: '', lastActive: '' })
    } else {
      const c = coordinatorOf(r)
      if (c.path) paths.push(c.path)
      agents.push({ ticket: t.id, role: 'coordinator', name: r.id, phase: 'supervise', since: c.at, path: c.path, durationMin: minutes(c.at, now), model: '', lastActive: lastOut[c.path] ?? '' })
    }
    const workers = r ? await workersOf(r.id) : []
    const workerRows = r ? (await runJson(['orca', 'orchestration', 'worker-list', '--run', r.id, '--json']))?.result?.workers ?? [] : []
    const projectionByTask = new Map<string, WorkerProjection>()
    for (const w of workerRows) { if (w?.taskId && w.projection) projectionByTask.set(w.taskId, w.projection) }
    const card = cardByIssue.get(t.id)
    if (!workers.some(k => k.path) && card?.path) {
      paths.push(card.path)
    }
    let gateLane: number | undefined
    let gateAt = ''
    for (const k of workers) {
      const projection = projectionByTask.get(k.id)
      const phase = k.status === 'dispatched' ? 'implement' : k.status === 'completed' ? 'done' : k.status === 'failed' ? 'returned' : k.status
      const since = k.dispatched_at ?? k.started_at ?? k.updated_at ?? k.created_at ?? ''
      const until = phase === 'done' ? ts(k.completed_at ?? k.updated_at) || now : now
      paths.push(k.path)
      agents.push({ ticket: t.id, role: 'worker', name: k.display_name ?? k.id, phase, since, path: k.path, durationMin: minutes(since, until), model: projection?.provider?.model ?? '', lastActive: lastOut[k.path] ?? '', ...(projection?.attention?.requiresAction === true ? { requiresAction: true } : {}), ...(card?.comment ? { comment: card.comment } : {}) })
      // Gate logs of every unit under the worker worktree.
      try {
        for (const e of await $.fs.list(k.path + '/.omc/gates')) {
          try {
            const log = await $.fs.read(k.path + '/.omc/gates/' + e.name + '/gate.log')
            if (!log.trim()) continue
            gates[t.id] = mergeGate(gates[t.id], parseGate(log))
            const segment = log.slice(Math.max(0, log.lastIndexOf('=== gate ')))
            const lane = /^LANE\s+(\d+)/m.exec(segment)
            if (lane) gateLane = Math.max(gateLane ?? 0, Number(lane[1]))
            const stamps = segment.match(/\d{4}-\d\d-\d\dT[\d:.]+Z/g) ?? []
            gateAt = [...stamps, gateAt].sort().pop() ?? ''
          } catch { /* no readable log: preserve the previous phase below */ }
        }
      } catch { /* no gates dir */ }
    }
    const previous = old.phases?.[t.id]
    const driver = cardByIssue.get(t.id)?.comment
    let phase = ticketPhase(t, { run: r, tasks: workers, gate: gates[t.id], lane: gateLane, pr: prOf(t.id)?.number, previous: previous?.phase, driver })
    // Preserve downstream phases on missing reads, unless fresh tasks show correction work.
    if (previous && !driverPhase(driver) && !workers.some(k => ['dispatched', 'pending', 'ready'].includes(k.status))) {
      if (!prOf(t.id) && !gates[t.id] && ['🧪 gate', '🔍 审查（推断）', '🚦 CI/合并'].includes(previous.phase)) phase = previous.phase
      if (previous.phase === '🚦 CI/合并' && !prOf(t.id)) phase = previous.phase
      if (previous.phase === '🔍 审查（推断）' && gates[t.id]?.state === 'green' && !gateLane && !prOf(t.id)) phase = previous.phase
    }
    const matching = workers.filter(k => phase === '📬 待收尾' ? k.status === 'completed' : phase === '🔨 实现' ? k.status === 'dispatched' : k.status === 'pending' || k.status === 'ready')
    const taskAt = matching.map(k => phase === '📬 待收尾' ? k.completed_at ?? k.updated_at : phase === '🔨 实现' ? k.dispatched_at ?? k.updated_at : k.updated_at ?? k.created_at).filter(Boolean).sort().pop()
    const observedAt = phase === '🚦 CI/合并' ? prOf(t.id)?.updatedAt : phase === '🧪 gate' || phase === '🔍 审查（推断）' ? gateAt : taskAt
    const lastActive = workers.map(k => lastOut[k.path]).filter(Boolean).sort().pop() || previous?.lastActive
    phases[t.id] = { phase, since: previous?.phase === phase ? previous.since : observedAt || (phase === '📋 拆分' ? t.claimedAt || t.updatedAt : new Date(now).toISOString()), lastActive }
    ticketPaths[t.id] = paths
  }
  // Tickets beyond the per-ticket read budget still show a phase using existing run/PR data.
  for (const t of inFlight.slice(MAX_TICKETS)) {
    const previous = old.phases?.[t.id]
    const phase = ticketPhase(t, { run: runFor(t.id), pr: prOf(t.id)?.number, previous: previous?.phase })
    phases[t.id] = previous?.phase === phase ? previous : { phase, since: new Date(now).toISOString() }
  }
  for (const t of doneToday.slice(0, MAX_DONE)) {
    const r = runFor(t.id)
    if (!r) continue
    const c = coordinatorOf(r)
    ticketPaths[t.id] = [...(c.path ? [c.path] : []), ...(await workersOf(r.id)).map(k => k.path)]
  }

  // Add linked cards even when per-run task reads were capped or a run has disappeared.
  for (const [id, card] of cardByIssue) ticketPaths[id] = [...new Set([...(ticketPaths[id] ?? []), card.path])]
  const worktreeToTicket: WorktreeToTicket = {}
  for (const [id, paths] of Object.entries(ticketPaths)) for (const path of new Set(paths)) {
    if (!path) continue
    const previous = worktreeToTicket[path]
    worktreeToTicket[path] = [...new Set([...(typeof previous === 'string' ? [previous] : previous ?? []), id])]
  }
  let usageError = '', rows: UsageRecord[] = []
  try { rows = await loadUsage($, Object.keys(worktreeToTicket)) }
  catch (e) { usageError = '用量读取失败：' + String(e) }
  const byTicket = aggregateUsage(rows, worktreeToTicket)
  const byPath = aggregateUsage(rows, Object.fromEntries(Object.keys(worktreeToTicket).map(p => [p, p])))
  for (const a of agents) if (a.path) {
    const usage = byPath[a.path]
    Object.assign(a, usageFields(usage), { model: a.model || usage?.model || '' })
  }
  // Reuse the precheck payload; a fresh snapshot must never trigger extra Linear reads.
  const localLedgerComments = useSnapshot ? Object.values(snapshot!.comments).flat().filter(c => c.body.startsWith('ledger ')) : []
  if (localLedgerComments.length) {
    ticketLedgerCache = { at: now, rows: ledgerUsage(localLedgerComments), comments: localLedgerComments }
  } else if (((!useSnapshot && !linearThrottled && (doneToday.length > 0 || failed.length > 0))
      || activeView === 'throughput' || activeView === 'failures')
      && (!ticketLedgerCache || now - ticketLedgerCache.at >= LINEAR_FALLBACK_THROTTLE_MS)) {
    // Supplemental view data is not part of the production precheck snapshot.
    // At most one discovery plus three ledger reads per five-minute window.
    const found = await runOrca(github ? ['bash', 'scripts/agent-gates/agent-graph.sh', 'issues', 'list-issues', '--query', '[ledger] agent-graph', '--json'] : ['orca', 'linear', 'list-issues', '--team', env.AGENT_LINEAR_TEAM, '--project', env.AGENT_LINEAR_PROJECT, '--query', '[ledger] agent-graph', '--json'])
    const candidates = found.ok && Array.isArray(found.data?.result?.issues)
      ? found.data.result.issues.filter((row: { title: string }) => /^\[ledger\] agent-graph(?: #\d+)?$/.test(row.title)).slice(0, 3) : null
    let comments: ReturnType<typeof deepComments> = []
    let complete = candidates !== null
    for (const candidate of candidates ?? []) {
      const response = await runOrca(github ? ['bash', 'scripts/agent-gates/agent-graph.sh', 'issues', 'issue', candidate.identifier ?? candidate.id, '--comments', '--json'] : ['orca', 'linear', 'issue', candidate.identifier ?? candidate.id, '--comments', '--json'])
      if (!response.ok) { complete = false; break }
      comments.push(...deepComments(response.data))
    }
    if (!complete) comments = ticketLedgerCache?.comments ?? []
    ticketLedgerCache = { at: now, rows: ledgerUsage(comments), comments }
  }
  let todayCost = 0
  for (const t of [...all, ...failed]) {
    const ledger = ticketLedgerCache?.rows[t.id]
    const usage = t.state === 'completed' ? ledger?.usage ?? byTicket[t.id] : byTicket[t.id]
    Object.assign(t, usageFields(usage))
    t.durationMin = t.state === 'started' ? undefined : ledger?.minutes
    t.gate = gates[t.id]; t.pr = prOf(t.id)?.number
  }
  for (const t of [...inFlight, ...doneToday]) if (t.cost?.known) todayCost += t.cost.usd

  // Ledger stats triggers Linear reads indirectly; cache for ten minutes and never bust from the
  // refresh button. On a failed read fall back to the last successful output (per the #45 rule).
  let stats: string[]
  if (ledgerCache && now - ledgerCache.at < LEDGER_TTL_MS) {
    stats = ledgerCache.stats
  } else {
    const statsOut = (await run(['bash', 'scripts/agent-gates/agent-graph.sh', 'ledger', 'stats'])).trim().split('\n').filter(Boolean)
    if (statsOut.length) {
      ledgerCache = { at: now, stats: statsOut }
      stats = statsOut
    } else {
      stats = ledgerCache?.stats ?? old.stats
    }
  }

  const queue = all.filter(t => has(t, 'ready-for-agent') && t.state === 'unstarted' && (!github || !has(t, 'auto-failed'))).sort((a, b) => (a.priority || 9) - (b.priority || 9))
  // state.type per identifier, used to badge dependency-chain nodes. Only populate on a
  // successful Linear read; a failed refresh carries forward the previous map.
  const issueStates: Record<string, string> = linearOk
    ? Object.fromEntries(all.map(t => [t.id, t.state]))
    : ((old as Board).issueStates ?? {})

  // On Linear failure every ticket-shaped slice falls back to the last-good board so the pane does not blank.
  const lastSuccessAt = linearOk ? now : old.lastSuccessAt
  const maxInFlight = Number(env.AGENT_MAX_IN_FLIGHT || 0) || old.maxInFlight
  // Snapshot-fed refreshes carry the snapshot's blockedBy map; live refreshes keep the last
  // one we saw so the queue cards do not blink into an "unknown" state between snapshots.
  const blockedBy = github && linearOk
    ? Object.fromEntries(issuesResp.data.result.issues.map((row: { identifier: string; complete: boolean; blockedBy: { identifier: string }[] }) => [row.identifier, row.complete ? row.blockedBy.map(blocker => blocker.identifier) : ['依赖或纳管证据不完整']]))
    : useSnapshot ? snapshot!.blockedBy : old.blockedBy
  let graph = old.graph, graphNotice = old.graphNotice
  if (useSnapshot) {
    const issues = snapshot!.issues.map(row => ({
      id: String(row.identifier ?? row.id), state: String(row.state?.type ?? ''),
      parent: row.parent?.identifier ?? null,
      blockedBy: snapshot!.blockedBy?.[String(row.identifier ?? row.id)] ?? [],
    }))
    graph = buildGraph({ issues })
    const missingParents = snapshot!.issues.some(row => row.parent === undefined)
    graphNotice = missingParents || !snapshot!.blockedBy ? '数据不全：快照缺少父票或阻塞关系' : ''
  }
  if (!github && activeView === 'graph' && (!useSnapshot || snapshot!.issues.some(row => row.parent === undefined) || !snapshot!.blockedBy)) {
    const cached = await readGraphSnapshot(now, { process: { run: (argv, options) => $.process.run(argv, options) }, fs: {} }, { team: env.AGENT_LINEAR_TEAM, project: env.AGENT_LINEAR_PROJECT, tmpDir: TMP_DIR || '/tmp' })
    graph = cached.at > 0 ? buildGraph(cached) : old.graph ?? null
    graphNotice = [cached.stale ? '缓存过期，沿用上次数据' : '', cached.incomplete.length ? '数据不全：' + cached.incomplete.join('、') : ''].filter(Boolean).join(' · ')
  } else if (!useSnapshot && graph) graphNotice = '缓存过期；打开依赖视图后刷新'
  const metricRuns: ThroughputRun[] | undefined = runsResp.ok ? autoRuns.map(run => ({
    ...run, startedAt: run.startedAt ?? run.dispatchedAt ?? run.scheduledFor ?? run.createdAt,
  })) : old.metricRuns
  const ledger = ledgerRows(ticketLedgerCache?.comments ?? [])
  const throughput = ledger.length || metricRuns !== undefined ? buildThroughput({ now, ledger, runs: metricRuns ?? [], maxInFlight }) : old.throughput
  const metricComments = useSnapshot ? snapshot!.comments : Object.fromEntries(commentsByTicket)
  const failures = ledger.length || Object.keys(metricComments).length
    ? buildFailures({ now, ledger, comments: metricComments }) : old.failures
  const linearUsage = github ? undefined : await readLinearUsage($, now)
  if (github && linearOk) {
    const rows: { id: string; identifier: string; state: { type: string }; parent: string }[] = issuesResp.data.result.issues
    const identifiers = new Map(rows.map(row => [row.id, row.identifier]))
    graph = buildGraph({ issues: rows.map(row => ({ id: row.identifier, state: row.state.type, parent: row.parent ? identifiers.get(row.parent) ?? row.parent : null, blockedBy: blockedBy?.[row.identifier] ?? [] })) })
    graphNotice = issuesResp.data.result.issues.some((row: { complete: boolean }) => !row.complete) ? '数据不全：未纳管或依赖证据不完整' : ''
  }
  const metrics = { graph, graphNotice, throughput, failures, metricRuns }
  if (!linearOk) {
    return {
      backend, repository, at, error: [error, usageError].filter(Boolean).join(' · '), maxInFlight, phases: old.phases, coordinatorAlive, driver: undefined,
      inFlight: old.inFlight, queue: old.queue, failed: old.failed as FailedTicket[], doneToday: old.doneToday,
      prs, agents: old.agents, lastRun, stats, todayCost: old.todayCost, automation, automationKnown, lastSuccessAt,
      lastPrecheck, nextRunAt, lastRunAt, linearUsage, installedVersion,
      snapshotAgeMs, linearThrottled: linearThrottled ? true : undefined, blockedBy, issueStates, ...metrics, repositoryPaths,
    }
  }
  return {
    backend, repository, at, error: [error, usageError].filter(Boolean).join(' · '), maxInFlight, phases, coordinatorAlive, driver: undefined, inFlight, queue, failed, doneToday, prs,
    agents: agentsOk ? agents : old.agents,
    lastRun, stats, todayCost: agentsOk ? todayCost : old.todayCost, automation, automationKnown, lastSuccessAt,
    lastPrecheck, nextRunAt, lastRunAt, linearUsage, installedVersion,
    snapshotAgeMs, linearThrottled: linearThrottled ? true : undefined, blockedBy, issueStates, ...metrics, repositoryPaths,
  }
}

let driverCache: DriverStatus | undefined
let driverStatusFile = ''
async function readDriverSnapshot($: any) {
  if (!driverStatusFile) {
    try {
      const r = await $.process.run(['sh', '-c', 'printf %s "${XDG_RUNTIME_DIR:-/tmp}/orca-factory/status.json"'], { timeoutMs: TIMEOUT_MS })
      if (r?.exitCode !== 0) return
      driverStatusFile = String(r.stdout ?? '').trim()
    } catch {
      driverCache = undefined
      return
    }
  }
  let status: DriverStatus | undefined
  try { status = parseDriverStatus(json(await $.fs.read(driverStatusFile))) }
  catch { status = undefined } // A missing cache immediately restores precheck.
  driverCache = status
}

function withDriver(b: Board, driver: DriverStatus | undefined, now: number): Board {
  if (!driver || driver.state !== 'ok') return { ...b, driver }
  if (pickStatusSource(driver, b, now) !== 'driver') return { ...b, driver: { state: 'stale' } }
  driver = { ...driver, tickets: driver.tickets.filter(ticket => b.repositoryPaths?.includes(ticket.repo)) }
  const known = [...b.inFlight, ...b.queue, ...b.failed, ...b.doneToday]
  const rows = driver.tickets.map(t => known.find(old => old.id === t.identifier) ?? {
    id: t.identifier, title: t.identifier, state: 'started', labels: ['auto-impl'],
    priority: 0, updatedAt: new Date(typeof driver.generatedAt === 'number' ? driver.generatedAt * 1000 : driver.generatedAt).toISOString(), claimedAt: '',
  })
  const ids = new Set(rows.map(t => t.id))
  const candidates = new Set(driver.tickets.filter(t => t.stage === 'candidate').map(t => t.identifier))
  const legacy = new Set(driver.tickets.filter(t => t.stage === 'human' || t.owner === '人').map(t => t.identifier))
  const resolving = new Set(driver.tickets.filter(t => t.owner === '处置Agent').map(t => t.identifier))
  const inFlight = rows.filter(t => !candidates.has(t.id) && !legacy.has(t.id))
  const failed = [...b.failed.filter(t => !ids.has(t.id)), ...rows.filter(t => resolving.has(t.id) || legacy.has(t.id))]
  const queue = [...b.queue.filter(t => !ids.has(t.id)), ...rows.filter(t => candidates.has(t.id))]
  return { ...b, driver, inFlight, queue, failed }
}

async function tick($: any, handoffs: Map<string, string>) {
  if (refreshing) return
  refreshing = true
  try {
    await readDriverSnapshot($)
    const old = await read($, board)
    const b = await refresh($, handoffs, old)
    await update($, board, () => b)
    await update($, lastTickAt, () => Date.now())
    $.ui.status(G.work + ' ' + b.inFlight.length + '/' + b.maxInFlight + ' · ' + G.wait + ' ' + b.prs.length + (b.failed.length ? ' · ' + G.human + ' ' + b.failed.length : '') + ' · ' + G.queued + ' ' + b.queue.length)
  } finally { refreshing = false }
}

async function toggleAutomation($: any, handoffs: Map<string, string>) {
  const b: Board = await read($, board)
  if (!b.automation) return
  try {
    const result = await $.process.run(['orca', 'automations', 'edit', b.automation.id, b.automation.enabled ? '--disabled' : '--enabled'], { timeoutMs: TIMEOUT_MS })
    if (result.exitCode !== 0) throw new Error(result.stderr || result.stdout || 'Automation edit failed')
  } catch (error) {
    const message = String(error instanceof Error ? error.message : error)
    await update($, board, current => ({ ...current, error: message }))
    return
  }
  await tick($, handoffs)
}

export const laneStage = (t: Ticket, agents: readonly BaseAgent[]): 1 | 2 | 3 => {
  if (t.pr) return 3
  return agents.some(a => a.ticket === t.id && a.role === 'worker' && a.phase === 'done') ? 2 : 1
}

export const register: Register = on => {
  // Last-good handoff per failed ticket. An empty comment read or a stale handoff
  // comment falls back to this cache instead of blanking the "Agent处置" card.
  const handoffs = new Map<string, string>()
  // The plugin loads in every Claude session on the machine, unattended coordinators included.
  // Polling starts only in a session that opens the pane: each refresh costs ~3 GitHub REST
  // calls per open PR, and one timer per session exhausted the 5000/h quota.
  let timers: { cancel(): void }[] = []
  let driverStat: { mtimeMs: number; size: number } | null = null
  let prevStat: { mtimeMs: number; size: number } | null = null
  on('session.start', async ($, e, next) => {
    await $.command.register({ name: 'swarm', description: 'Open the swarm board' })
    return next(e)
  })

  on('command.run', { command: 'swarm' }, async $ => {
    await $.ui.open({ id: PANE, title: 'Swarm' })
    paneOpen = true
    void tick($, handoffs)
    if (!timers.length) {
      await update($, nowAtom, () => Date.now())
      timers.push($.clock.every(REFRESH_MS, () => { if (paneOpen) return tick($, handoffs) }))
      timers.push($.clock.every(WATCH_POLL_MS, async () => {
        if (!paneOpen) return
        if (!SNAPSHOT_FILE || refreshing) return
        refreshing = true
        try {
          let stat: { mtimeMs: number; size: number } | null
          try { stat = await $.fs.stat(SNAPSHOT_FILE) } catch { stat = null }
          const reload = shouldReload(prevStat, stat)
          prevStat = stat
          if (reload && paneOpen) await refreshSnapshot($, handoffs)
          if (!paneOpen) return
          let nextDriverStat = null
          try { if (driverStatusFile) nextDriverStat = await $.fs.stat(driverStatusFile) } catch { /* cache was removed */ }
          if (shouldReload(driverStat, nextDriverStat)) await readDriverSnapshot($)
          if (!nextDriverStat) driverCache = undefined
          driverStat = nextDriverStat
          if (paneOpen) await update($, nowAtom, () => Date.now())
        } finally { refreshing = false }
      }))
      timers.push($.clock.every(1000, () => { if (paneOpen) void update($, nowAtom, () => Date.now()) }))
    }
    return { text: '蜂群看板已打开。' }
  })

  on('ui.close', { id: PANE }, async ($, e, next) => {
    paneOpen = false
    for (const timer of timers) timer.cancel()
    timers = []
    prevStat = driverStat = null
    return next(e)
  })

  // Test-only path: a test dispatches this command so the plugin's own `$.ui.close` call
  // fires the `ui.close` hook above, which flips `paneOpen` in this plugin VM where the
  // scheduled refresh callback also reads it. The command is never registered with the
  // command bar, so it never shows up for the person.
  on('command.run', { command: SIMULATE_CLOSE_COMMAND }, async $ => {
    await $.ui.close({ id: PANE })
    return { text: 'swarm pane closed.' }
  })

  // Test-only path: fixtures that fire multiple refreshes in a row dispatch this to clear
  // the 5-min Linear-fallback throttle (which lives on `lastLinearFallbackAt` in the plugin
  // VM). Test imports and the VM have separate module instances, so the test file cannot
  // mutate `lastLinearFallbackAt` directly; it has to run inside the VM.
  on('command.run', { command: RESET_LINEAR_THROTTLE_COMMAND }, async () => {
    lastLinearFallbackAt = 0
    return { text: 'swarm throttle reset.' }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Button } = $.ui.resolve(e)
    const [snapshotBoard, v, q, d, , tickMs] = await Promise.all([read($, board), read($, view), read($, showQueue), read($, detail), read($, nowAtom), read($, lastTickAt)])
    const b = withDriver(snapshotBoard, driverCache, Date.now())
    const automation = (b as Board).automation
    const automationKnown = (b as Board).automationKnown
    const lastSuccessAt = (b as Board).lastSuccessAt
    const now = Date.now()
    // Countdown uses the wall clock, not the throttled `nowAtom` (which starts at 0 and could race the first tick).
    const remainingSec = tickMs > 0 ? Math.min(60, Math.max(0, Math.ceil((tickMs + REFRESH_MS - now) / 1000))) : 0
    const countdown = tickMs > 0 && remainingSec > 0 ? '下次刷新 ' + remainingSec + 's' : '刷新中'
    const staleMin = lastSuccessAt > 0 ? Math.max(0, Math.floor((now - lastSuccessAt) / 60_000)) : 0
    const stale = lastSuccessAt > 0 && now - lastSuccessAt > STALE_MS
    const bodyColumns = e.props?.bodyColumns ?? e.viewport?.columns ?? 100
    const cols = Math.max(44, bodyColumns - 4)
    const wide = cols >= 96
    const cut = (s: string, n: number) => (s.length > n ? s.slice(0, Math.max(1, n - 1)) + '…' : s)
    const card = (key: string, title: string, children: any, color?: string, dim?: boolean) => (
      <Box key={key} flexDirection="column" borderStyle="round" borderColor={color} borderDimColor={!color || dim} paddingX={1} marginBottom={1}>
        <Text bold color={color} dimColor={dim}>{title}</Text>
        {children}
      </Box>
    )
    const ciText = (ci: Ci) => (ci.state === 'none' ? '' : 'CI ' + SYM[ci.state] + ' ' + ci.done + '/' + ci.total + ' ' + mins(ci.min))
    const ticketMinutes = (t: Ticket) => t.state === 'started'
      ? minutes(t.claimedAt || b.agents.find(a => a.ticket === t.id && a.role === 'coordinator')?.since || t.updatedAt, now)
      : t.durationMin
    const ticketAccounting = (t: Ticket) => {
      const duration = ticketMinutes(t)
      return '用时 ' + (duration === undefined ? '—' : mins(duration) || '0m')
        + ' · token ' + (t.usage ? tok(t.usage.total) : '—')
        + ' · ' + (usd(t.cost) || 'est. $?') + '（按 API 价折算' + (t.usage?.allocated ? ' · 分摊' : '') + '）'
    }
    const tRow = (glyph: string, t: Ticket, color?: string) => {
      const w = cols - 22
      return <Box flexDirection="column"><Text color={color}>{glyph} {t.id.padEnd(8)} {cut(t.title, w).padEnd(w)} {age(t.updatedAt, now).padStart(9)}</Text><Text dimColor>{ticketAccounting(t)}</Text></Box>
    }
    // Tasks view: swim lanes. One column per stage; a ticket is a small card that moves right.
    type Lane = { title: string; emoji: string; color?: string; items: Ticket[] }
    const stage = (t: Ticket): 1 | 2 | 3 => {
      const phase = b.driver?.state === 'ok' ? b.driver.tickets.find(row => row.identifier === t.id)?.phase : undefined
      if (phase === '排队合并' || phase === '开 PR') return 3
      if (phase === 'gate' || phase === '审查') return 2
      return phase ? 1 : laneStage(t, b.agents)
    }
    const handoffText = (t: FailedTicket) => {
      const lines = t.handoff?.trim().split(/\r?\n/) ?? []
      const reason = lines[0]?.match(/^(?:Agent处置|转人):\s*(.*)/)?.[1]
      const action = lines.find(l => /^(?:处置要求|需要你):/.test(l))?.replace(/^(?:处置要求|需要你):/, '').trim()
      return reason && action ? (
        <Box flexDirection="column">
          <Text color="yellow">卡点：{reason}</Text>
          <Text color="yellow">处置：{action}</Text>
        </Box>
      ) : lines[0] ? <Text color="yellow">{lines[0]}</Text> : null
    }
    const resolverText = (t: Ticket) => <Text dimColor>处置 Agent 接管 {t.id}，完成修复与复核后继续推进</Text>
    // A ticket that already lives in Agent处置 does not also appear in the in-flight lanes; lane counts match that dedup.
    const failedIds = new Set(b.failed.map(t => t.id))
    const inFlightNoHuman = b.inFlight.filter(t => !failedIds.has(t.id))
    const lanes: Lane[] = [
      { title: '待领', emoji: LANE_EMOJI.queue, items: b.queue },
      { title: '实现中', emoji: LANE_EMOJI.impl, items: inFlightNoHuman.filter(t => stage(t) === 1) },
      { title: 'gate/审查', emoji: LANE_EMOJI.gate, items: inFlightNoHuman.filter(t => stage(t) === 2) },
      { title: 'CI/合并', emoji: LANE_EMOJI.ci, items: inFlightNoHuman.filter(t => stage(t) === 3) },
      { title: 'Agent处置', emoji: LANE_EMOJI.human, color: b.failed.length ? 'yellow' : undefined, items: b.failed },
      { title: '今日完成', emoji: LANE_EMOJI.done, items: b.doneToday },
    ]
    // Status emoji on the card head: prefer CI, then gate, then the lane emoji.
    const statusEmojiOf = (l: Lane, t: Ticket, pr?: Pr) => {
      if (pr && pr.ci.state !== 'none' && STATUS_EMOJI[pr.ci.state]) return STATUS_EMOJI[pr.ci.state]
      if (t.gate && t.gate.state !== 'none' && STATUS_EMOJI[t.gate.state]) return STATUS_EMOJI[t.gate.state]
      return l.emoji
    }
    // Why each queued ticket is still waiting. `null` means no reason text; the row is skipped.
    const lp = (b as Board).lastPrecheck
    const driverOnline = b.driver?.state === 'ok'
    const blockedBy = (b as Board).blockedBy
    const issueStates = (b as Board).issueStates
    const blockersOf = (id: string): string[] | undefined => {
      const v = blockedBy?.[id]
      const open = Array.isArray(v) ? v.filter(x => issueStates?.[x] !== 'completed' && issueStates?.[x] !== 'canceled') : []
      return open.length > 0 ? open : undefined
    }
    // Dependencies take precedence over the online driver and precheck signals.
    type QueueGroup = 'warn' | 'claimable' | 'blocked' | 'unknown'
    const queueGroupOf = (t: Ticket): QueueGroup => {
      if (blockersOf(t.id)) return 'blocked'
      if (driverOnline && Array.isArray(blockedBy?.[t.id])) return 'claimable'
      if (!lp) return 'unknown'
      if (lp.claimable.includes(t.id)) return 'claimable'
      if (lp.uncertain) return 'warn'
      if (lp.skips[t.id]) return 'warn'
      return 'unknown'
    }
    const stateBadge = (id: string): string => {
      const s = issueStates?.[id]
      if (s === 'completed') return '✓'
      if (s === 'started') return '🔨'
      if (s === 'unstarted') return '📥'
      return s ? '⛓' : '?'
    }
    const shortId = (id: string): string => {
      const m = /(\d+)$/.exec(id)
      return m ? m[1] : id
    }
    const idNum = (id: string): number => parseInt(shortId(id), 10) || 0
    // Topological level per queue ticket; external blockers count as 0. Returns null on a cycle.
    const computeChainLevels = (): Record<string, number> | null => {
      if (!blockedBy || Object.keys(blockedBy).length === 0) return null
      const queueIds = new Set(b.queue.map(t => t.id))
      const levels: Record<string, number> = {}
      const visiting = new Set<string>()
      let cycle = false
      const dfs = (id: string): number => {
        if (id in levels) return levels[id]
        if (!queueIds.has(id)) return 0
        if (visiting.has(id)) { cycle = true; return 0 }
        visiting.add(id)
        const bs = blockedBy[id] ?? []
        let max = -1
        for (const bk of bs) {
          const lvl = dfs(bk)
          if (lvl > max) max = lvl
        }
        visiting.delete(id)
        const level = max < 0 ? 0 : max + 1
        levels[id] = level
        return level
      }
      for (const t of b.queue) dfs(t.id)
      return cycle ? null : levels
    }
    const chainLevels = computeChainLevels()
    const buildChainLine = (): string | null => {
      if (!chainLevels) return null
      const queueIds = new Set(b.queue.map(t => t.id))
      const byLevel = new Map<number, Set<string>>()
      const addToLevel = (id: string, lvl: number) => {
        if (!byLevel.has(lvl)) byLevel.set(lvl, new Set())
        byLevel.get(lvl)!.add(id)
      }
      for (const t of b.queue) addToLevel(t.id, chainLevels[t.id] ?? 0)
      for (const ids of Object.values(blockedBy ?? {})) {
        for (const id of ids) if (!queueIds.has(id)) addToLevel(id, 0)
      }
      const sortedLevels = [...byLevel.keys()].sort((a, b) => a - b)
      if (sortedLevels.length === 0) return null
      const display = sortedLevels.slice(0, 3)
      const remaining = sortedLevels.length - display.length
      const parts = display.map(lvl =>
        [...byLevel.get(lvl)!]
          .sort((a, b) => idNum(a) - idNum(b))
          .map(id => shortId(id) + stateBadge(id))
          .join(' '),
      )
      return '链：' + parts.join(' → ') + (remaining > 0 ? ' → … +' + remaining + ' 层' : '')
    }
    const classifyQueue = (): Record<QueueGroup, Ticket[]> => {
      const groups: Record<QueueGroup, Ticket[]> = { warn: [], claimable: [], blocked: [], unknown: [] }
      for (const t of b.queue) groups[queueGroupOf(t)].push(t)
      const sortGroup = (xs: Ticket[]) => xs.sort((a, b) => {
        const la = chainLevels?.[a.id] ?? 0
        const lb = chainLevels?.[b.id] ?? 0
        return la !== lb ? la - lb : idNum(a.id) - idNum(b.id)
      })
      sortGroup(groups.warn); sortGroup(groups.claimable); sortGroup(groups.blocked); sortGroup(groups.unknown)
      return groups
    }
    const allQueueBlocked = b.queue.length > 0 && b.queue.every(t => blockersOf(t.id) !== undefined)
    const earliestBlocker = (): string => {
      if (b.queue.length === 0) return ''
      return blockersOf(b.queue[0].id)?.[0] ?? ''
    }
    const phaseLine = (t: Ticket) => {
      if (!inFlightNoHuman.some(x => x.id === t.id)) return null
      const live = b.driver?.state === 'ok' ? b.driver.tickets.find(row => row.identifier === t.id) : undefined
      if (live) return <Text>{live.phase + ' · 归属 ' + live.owner + ' · ' + live.repo}</Text>
      const observation = (b as Board).phases?.[t.id]
      if (!observation) return null
      const elapsedMs = Number.isFinite(ts(observation.since)) ? now - ts(observation.since) : 0
      const duration = Math.max(0, Math.floor(elapsedMs / 60_000))
      const stranded = observation.phase === '📬 待收尾' && elapsedMs > 10 * 60_000 && (b as Board).coordinatorAlive === false
      const label = stranded ? '⚠ 待收尾 ' + Math.floor(duration) + ' 分钟，无 coordinator 在跑'
        : observation.phase + ' · ' + mins(duration) + (observation.phase === '🔨 实现' && observation.lastActive ? ' · 最后输出 ' + age(observation.lastActive, now) + '前' : '')
      return <Text color={stranded ? 'yellow' : undefined}>{label}</Text>
    }
    const laneCard = (l: Lane, t: Ticket, w: number) => {
      const pr = t.pr ? b.prs.find(p => p.number === t.pr) : undefined
      const dur = ticketMinutes(t) === undefined ? '' : mins(ticketMinutes(t)!)
      const facts: string[] = []
      if (t.gate && t.gate.sec) facts.push('🧪 ' + secs(t.gate.sec))
      if (pr && pr.ci.state !== 'none' && pr.ci.total > 0) facts.push('🚦 ' + pr.ci.done + '/' + pr.ci.total + (pr.ci.min ? ' ' + mins(pr.ci.min) : ''))
      if (d && pr && (pr.adds || pr.dels)) facts.push('📝 +' + pr.adds + '/−' + pr.dels)
      const head = statusEmojiOf(l, t, pr) + ' ' + t.id + (pr ? ' #' + pr.number : '')
      const headRoom = Math.max(0, w - (dur ? dur.length + 1 : 0))
      return (
        <Box key={'l-' + t.id} flexDirection="column" marginBottom={1}>
          <Text bold color={l.color}>{padW(cutW(head, headRoom), headRoom)}{dur ? ' ' + dur : ''}</Text>
          {phaseLine(t)}
          <Text dimColor>{ticketAccounting(t)}</Text>
          <Text>{cutW(t.title, w)}</Text>
          {t.url ? <Text dimColor>{t.url}</Text> : null}
          {l.title === 'Agent处置' ? handoffText(t) : null}
          {facts.length > 0 ? <Text dimColor>{cutW(facts.join(' · '), w)}</Text> : null}
          {l.title === 'Agent处置' ? resolverText(t) : null}
        </Box>
      )
    }
    // Queue-only card: specialised reason line keyed by the ticket's group.
    const GROUP_EMOJI: Record<QueueGroup, string> = { warn: '⚠', claimable: '🟢', blocked: '⛓', unknown: '?' }
    const queueCardReason = (t: Ticket, group: QueueGroup): string | null => {
      if (group === 'blocked') {
        const blockers = blockedBy?.[t.id] ?? []
        if (blockers.length === 0) return null
        const shown = blockers.slice(0, 3).map(id => shortId(id) + stateBadge(id)).join(' ')
        return '等 ' + shown + (blockers.length > 3 ? ' +' + (blockers.length - 3) : '')
      }
      if (group === 'claimable') return driverOnline ? '驱动器下一轮巡检领取' : '已入选，等 coordinator 领取'
      if (group === 'warn') {
        if (lp?.uncertain) return '读取失败：' + (lp.uncertainReason || '退出码 ' + lp.exitCode)
        const skip = lp?.skips[t.id]
        if (skip) return '未入选：' + skip
        return null
      }
      return null
    }
    const queueCard = (t: Ticket, w: number, group: QueueGroup, color?: string) => {
      const head = GROUP_EMOJI[group] + ' ' + t.id
      const reason = queueCardReason(t, group)
      return (
        <Box key={'ql-' + group + '-' + t.id} flexDirection="column" marginBottom={1}>
          <Text bold color={color}>{padW(cutW(head, w), w)}</Text>
          <Text>{cutW(t.title, w)}</Text>
          {t.url ? <Text dimColor>{t.url}</Text> : null}
          <Text dimColor>{ticketAccounting(t)}</Text>
          {reason ? <Text dimColor>{cutW(reason, w)}</Text> : null}
        </Box>
      )
    }
    const laneHead = (l: Lane) => <Text bold color={l.color} dimColor={l.items.length === 0}>{l.emoji} {l.title} ({l.items.length})</Text>
    // 待领 status line + warnings, shared by narrow (card) and wide (column) lane renderers.
    // Status drops entirely when no precheck has run yet; the "N 分钟没新运行" warning stands on its own.
    const nextRunAtMs = (b as Board).nextRunAt ?? 0
    const nextRunRemainSec = nextRunAtMs > 0 ? Math.max(0, Math.ceil((nextRunAtMs - now) / 1000)) : 0
    const nextRunText = nextRunAtMs > 0 ? (nextRunRemainSec > 0 ? secs(nextRunRemainSec) : '即将') : '—'
    const resultText = (code: number) => (code === 0 ? '✓' : '跳过')
    const buildQueueStatus = (): string | null => {
      const driverHead = driverHeadText(b.driver?.state === 'ok' ? { ...b.driver, generatedAt: new Date(typeof b.driver.generatedAt === 'number' ? b.driver.generatedAt * 1000 : b.driver.generatedAt).toISOString(), tickets: b.driver.tickets.length } : b.driver, now)
      if (driverHead !== null) return driverHead
      if (!lp) return null
      const head = '⏳ 下次扫描 ' + nextRunText
        + ' · 上次扫描 run ' + lp.runNumber + ' ' + resultText(lp.exitCode)
      if (lp.uncertain) return head + ' · 读取失败'
      return head
        + ' · 可领 ' + lp.claimable.length + ' 张'
        + ' · 在做 ' + lp.inFlight + '/' + lp.max
    }
    const buildQueueWarnings = (): string[] => {
      const out: string[] = []
      if (lp) {
        if (lp.uncertain) {
          const reason = lp.uncertainReason || '退出码 ' + lp.exitCode
          out.push('⚠ 读取失败：' + reason)
          if (/rate limit/i.test(reason)) out.push('Linear 限流，稍后自动恢复')
          else if (/network/i.test(reason)) out.push('Linear 网络波动，下一轮自动重试')
        } else if (b.queue.length > 0 && lp.claimable.length === 0 && lp.inFlight < lp.max && !allQueueBlocked) {
          out.push('⚠ 待领 ' + b.queue.length + ' 张。precheck 判定可领 0 张。')
          if (lp.firstStderrLine) out.push('⚠ 原因：' + lp.firstStderrLine)
        }
        if (lp.timedOut || lp.error) out.push('⚠ precheck 失败：' + (lp.error || '超时'))
      }
      const lastAt = (b as Board).lastRunAt ?? 0
      if (lastAt > 0) {
        const minsSince = Math.floor((now - lastAt) / 60_000)
        if (minsSince > 10) out.push('⚠ 定时任务超过 ' + minsSince + ' 分钟没有新扫描')
      }
      return out
    }
    const buildBlockedLine = (): string | null => {
      if (!allQueueBlocked) return null
      const first = earliestBlocker()
      return '全部待领票都在等依赖' + (first ? '（最先放行的阻塞者：' + first + '）' : '')
    }
    const GROUP_HEAD: Record<QueueGroup, string> = {
      warn: '⚠ 异常',
      claimable: '🟢 可放行',
      blocked: '⛓ 等依赖',
      unknown: '? 未知',
    }
    const LEGEND = '✓完成 🔨在做 📥待领 ⛓等依赖 ⚠异常'
    const buildStatsLine = (groups: Record<QueueGroup, Ticket[]>): string => {
      const unknownTail = groups.unknown.length > 0 ? ' · 未知 ' + groups.unknown.length : ''
      return '待领 ' + b.queue.length
        + ' · 🟢 可放行 ' + groups.claimable.length
        + ' · ⛓ 等依赖 ' + groups.blocked.length
        + ' · ⚠ 异常 ' + groups.warn.length
        + unknownTail
    }
    // Full body of the 待领 lane. The old queueExtras + simple card loop is replaced by
    // a stats line, optional chain line, status, warnings and three grouped sections
    // (异常 lifted to the top when non-empty).
    const queueLaneBody = (w: number) => {
      const empty = b.queue.length === 0
      const groups = classifyQueue()
      const stats = empty ? null : buildStatsLine(groups)
      const chain = empty || w < 100 ? null : buildChainLine()
      const status = buildQueueStatus()
      const warns = buildQueueWarnings()
      const blockedLine = buildBlockedLine()
      const order: QueueGroup[] = groups.warn.length > 0
        ? ['warn', 'claimable', 'blocked', 'unknown']
        : ['claimable', 'blocked', 'unknown']
      const legend = !empty && w >= 100 ? LEGEND : null
      const renderGroup = (g: QueueGroup) => {
        const items = groups[g]
        if (items.length === 0) return null
        const color = g === 'warn' ? 'yellow' : undefined
        const head = GROUP_HEAD[g] + ' (' + items.length + ')'
        const shown = items.slice(0, 5)
        const extra = items.length - shown.length
        return (
          <Box key={'qg-' + g} flexDirection="column">
            <Text dimColor color={color}>{cutW(head, w)}</Text>
            {shown.map(t => queueCard(t, w, g, color))}
            {extra > 0 ? <Text dimColor color={color}>{'+' + extra}</Text> : null}
          </Box>
        )
      }
      return (
        <Box key="queue-body" flexDirection="column">
          {stats ? <Text dimColor>{cutW(stats, w)}</Text> : null}
          {chain ? <Text dimColor>{cutW(chain, w)}</Text> : null}
          {status ? <Text dimColor>{cutW(status, w)}</Text> : null}
          {blockedLine ? <Text dimColor>{cutW(blockedLine, w)}</Text> : null}
          {warns.map((text, i) => <Text key={'qw-' + i} color="yellow">{cutW(text, w)}</Text>)}
          {order.map(renderGroup)}
          {legend ? <Text dimColor>{cutW(legend, w)}</Text> : null}
        </Box>
      )
    }
    const laneView = () => {
      if (bodyColumns >= 120) {
        const w = Math.floor(cols / lanes.length) - 2
        return (
          <Box flexDirection="row" marginBottom={1}>
            {lanes.map(l => (
              <Box key={'lane-' + l.title} flexDirection="column" width={w + 2} paddingX={1}>
                {laneHead(l)}
                {l.title === '实现中' ? <Text dimColor>{(['📋 拆分', '📤 派工', '🔨 实现', '📬 待收尾'] as const).map(phase => phase + '(' + inFlightNoHuman.filter(t => (b as Board).phases?.[t.id]?.phase === phase).length + ')').join(' ')}</Text> : null}
                <Text dimColor>{'─'.repeat(w)}</Text>
                {l.title === '待领'
                  ? queueLaneBody(w)
                  : l.items.length === 0 ? <Text dimColor>·</Text> : l.items.map(t => laneCard(l, t, w))}
              </Box>
            ))}
          </Box>
        )
      }
      return lanes.filter(l => l.items.length > 0 || l.title === '实现中').map(l =>
        card('lane-' + l.title, l.emoji + ' ' + l.title + ' (' + l.items.length + ')', (
          <Box flexDirection="column">
            {l.title === '待领'
              ? queueLaneBody(cols - 4)
              : l.items.length === 0 ? <Text dimColor>无</Text> : l.items.map(t => laneCard(l, t, cols - 4))}
          </Box>
        ), l.color, l.items.length === 0))
    }
    // Swarm view: one agent is one line; workers indent under their coordinator.
    const agentRow = (a: Agent) => {
      const requiresAction = a.role === 'worker' && a.requiresAction === true
      const glyph = a.phase === 'done' ? G.done : a.phase === 'returned' ? G.human : G.work
      const name = a.role === 'coordinator' ? glyph + ' ' + a.ticket.padEnd(8) + ' coordinator' : '  └ ' + glyph + ' ' + (requiresAction ? '⚠ Agent处置 ' : '') + cut(a.name, 18)
      const tokens = a.tokens ? tok(a.tokens.out) + ' out · ' + tok(a.tokens.cache) + ' cache' : ''
      const last = a.lastActive ? 'last ' + age(a.lastActive, now) : ''
      const line = name.padEnd(24) + ' ' + a.phase.padEnd(10) + ' ' + mins(a.durationMin).padStart(9)
        + (wide ? '   ' + cut(shortModel(a.model), 12).padEnd(12) + ' ' + tokens.padStart(22) + ' ' + usd(a.cost).padStart(11) + ' ' + last.padStart(14) : ' ' + usd(a.cost).padStart(11) + ' ' + last.padStart(14))
      return (
        <Box key={'a-' + a.ticket + '-' + a.name} flexDirection="column">
          <Text dimColor={a.phase === 'done' && !requiresAction} color={requiresAction || a.phase === 'returned' ? 'yellow' : undefined}>{cut(line, cols)}</Text>
          {a.role === 'worker' && a.comment?.trim() ? <Text dimColor>{'    ' + cut(a.comment.replace(/\s+/g, ' ').trim(), cols - 4)}</Text> : null}
        </Box>
      )
    }
    const prRow = (p: Pr) => {
      const ci = ciText(p.ci)
      const size = p.adds || p.dels ? '+' + p.adds + '/−' + p.dels : ''
      const behind = p.ci.state !== 'none' || size ? 'behind ' + p.behind : ''
      const w = Math.max(10, cols - 8 - 18 - 11 - 11 - 10)
      return (
        <Text key={'p-' + p.number} color={p.ci.state === 'red' ? 'red' : undefined}>
          {p.ci.state === 'red' ? G.human : G.wait} #{String(p.number).padEnd(5)} {cut(p.title, w).padEnd(w)} {ci.padStart(17)} {size.padStart(10)} {behind.padStart(10)} {age(p.updatedAt, now).padStart(9)}
        </Text>
      )
    }
    const driver = b.driver?.state === 'ok' ? b.driver : undefined
    const driverUsage = b.backend === 'github' ? driver?.github : driver?.linear
    const limit = driverUsage?.limits.filter(row => b.repositoryPaths?.includes(row.repo))
      .sort((a, b) => a.requests.remaining - b.requests.remaining)[0]
    const statusCols = Math.max(0, bodyColumns - 4)
    const driverBadgeText = driverUsage
      ? driverUsage.limits.map(l => (b.backend === 'github' ? 'GitHub ' : 'Linear ') + l.repo + ' 请求剩余 ' + l.requests.remaining + ' · 重置 ' + l.requests.reset
        + (b.backend === 'github' ? '' : ' · 复杂度剩余 ' + l.complexity.remaining + ' · 重置 ' + l.complexity.reset)).join(' · ')
      : undefined
    const rows = statusBarRows({
      inFlight: b.inFlight.length, max: b.maxInFlight || 0, failed: b.failed.length,
      queue: b.queue.length, prs: b.prs.length, doneToday: b.doneToday.length, costUsd: b.todayCost,
      quota: limit && b.backend !== 'github' ? { remaining: limit.requests.remaining, limit: LINEAR_HOURLY_LIMIT, resetAt: limit.requests.reset } : undefined,
      driver: b.driver ? b.driver.state : 'none', driverRequests: driverUsage?.requests, complexity: b.backend === 'github' ? undefined : driverUsage?.complexity,
      driverBadge: driverBadgeText, globalPauseSec: driver?.globalPause.active ? driver.globalPause.durationSeconds : undefined,
      at: b.at, refreshSec: remainingSec,
      snapshotMin: b.snapshotAgeMs === undefined ? undefined : Math.max(0, Math.floor(b.snapshotAgeMs / 60_000)),
      version: MOD_VERSION, view: v, detail: d,
      linearUsage: b.linearUsage, linearLimit: LINEAR_HOURLY_LIMIT, lastRun: b.lastRun, stale,
      restart: !!b.installedVersion && verGT(b.installedVersion, MOD_VERSION), installedVersion: b.installedVersion,
      automation: automation?.enabled, automationKnown, refreshing,
    }, statusCols)
    const statusActions = new Map<string, { key: string; hotkey: string; dimColor?: boolean; onPress: () => unknown }>()
    const viewLabels: Record<View, string> = { swarm: '总览', task: '泳道', graph: '依赖', throughput: '吞吐', failures: '失败' }
    const viewKeys: Record<View, string> = { swarm: 's', task: 't', graph: 'g', throughput: 'p', failures: 'f' }
    for (const target of Object.keys(viewLabels) as View[]) {
      statusActions.set(v === target ? '[' + viewLabels[target] + ']' : viewLabels[target], {
        key: 'view-' + target, hotkey: viewKeys[target], dimColor: v !== target,
        onPress: async () => { await update($, view, () => target); if (target !== 'swarm' && target !== 'task') await tick($, handoffs) },
      })
    }
    statusActions.set(d ? '[明细]' : '明细', { key: 'toggle-detail', hotkey: 'd', dimColor: !d, onPress: () => update($, detail, x => !x) })
    if (automation) {
      const toggleAuto = { key: 'toggle-auto', hotkey: 'a', onPress: () => toggleAutomation($, handoffs) }
      statusActions.set('● 定时 开', toggleAuto)
      statusActions.set('○ 定时 关', toggleAuto)
    }
    statusActions.set(refreshing ? '刷新中…' : '刷新', { key: 'refresh', hotkey: 'r', onPress: () => tick($, handoffs) })

    return (
      <Box flexDirection="column">
        {h(Box, { flexDirection: 'column', marginBottom: 1 }, ...rows.map((segments, row) =>
          h(Box, { key: 'status-' + row, flexDirection: 'row', gap: 1, width: statusCols }, ...segments.map((segment, index) => {
            const action = statusActions.get(segment.text)
            return action ? h(Button, { ...action, plain: true, label: segment.text })
              : h(Text, { key: index, color: segment.color, dimColor: !segment.color }, segment.text)
          }))))}
        {b.error ? <Text color="yellow">{b.error}</Text> : null}

        {v === 'swarm' ? card('agents', G.work + ' Agents · 按 API 价折算', (
          b.agents.length === 0 ? <Text dimColor>无</Text> : b.agents.map(agentRow)
        )) : v === 'graph' ? <Box flexDirection="column">{b.graphNotice && <Text dimColor>{b.graphNotice}</Text>}{renderGraph(Text, b.graph)}</Box>
          : v === 'throughput' ? renderThroughput(Text, b.throughput)
          : v === 'failures' ? renderFailures(Text, b.failures)
          : laneView()}

        {v === 'swarm' ? card('prs', G.wait + ' Open PRs', b.prs.length === 0 ? <Text dimColor>无</Text> : b.prs.map(prRow)) : null}

        {v === 'swarm' && b.failed.length > 0 ? card('failed', G.human + ' Agent处置', b.failed.map(t => (
          <Box key={'f-' + t.id} flexDirection="column" marginBottom={1}>
            {tRow(G.human, t, 'yellow')}
            {handoffText(t)}
            {resolverText(t)}
          </Box>
        )), 'yellow') : null}

        {v === 'swarm' ? (
          <Box flexDirection="row" gap={1}>
            <Button key="toggle-queue" hotkey="q" plain label={(q ? '▾' : '▸') + ' 待领 ' + b.queue.length} onPress={() => update($, showQueue, x => !x)} />
          </Box>
        ) : null}
        {v === 'swarm' && q ? b.queue.map(t => <Text key={'q-' + t.id} dimColor>{G.queued} P{t.priority || '-'} {t.id.padEnd(8)} {cut(t.title, cols - 18)}</Text>) : null}

        {d ? card('ticket-usage', '票用量明细', [...new Map([...b.inFlight, ...b.queue, ...b.failed, ...b.doneToday].map(t => [t.id, t])).values()].map(t => <Box key={'usage-' + t.id} flexDirection="column"><Text>{t.id + ' ' + t.title}</Text><Text dimColor>{ticketAccounting(t)}</Text></Box>)) : null}

        {b.stats.length > 0 ? <Box marginTop={1} flexDirection="column">{b.stats.map((s, i) => <Text key={'s-' + i} dimColor>{s}</Text>)}</Box> : null}
      </Box>
    )
  })
}
