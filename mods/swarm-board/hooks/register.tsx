import { atom, read, update } from 'claude-code'
import type { Register } from 'claude-code'

import type { Agent as BaseAgent, Board as BaseBoard, Ci, Cost, Gate, GateStep, Pr, Precheck, Ticket, Tokens, View, TicketPhase, PhaseObservation } from '../types'

type FailedTicket = Ticket & { handoff?: string }
type Board = Omit<BaseBoard, 'failed'> & { failed: FailedTicket[]; automation?: { id: string; enabled: boolean }; phases?: Record<string, PhaseObservation>; coordinatorAlive?: boolean }

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
}

export const ticketPhase = (ticket: Ticket, data: PhaseData): TicketPhase => {
  if (data.pr || ticket.pr) return '🚦 CI/合并'
  const gate = data.gate ?? ticket.gate
  if (gate?.state === 'running') return '🧪 gate'
  const tasks = data.tasks ?? []
  if (tasks.some(t => t.status === 'dispatched')) return '🔨 实现'
  if (tasks.some(t => t.status === 'pending' || t.status === 'ready')) return '📤 派工'
  if (gate?.state === 'green' && data.lane === 2) return '🔍 审查（推断）'
  if (tasks.length && tasks.every(t => t.status === 'completed')) return '📬 待收尾'
  return data.previous ?? (data.run ? '📤 派工' : '📋 拆分')
}

const PANE = 'swarm-board'
const ENV = 'scripts/agent-gates/agent-graph.env'
const REFRESH_MS = 60_000
const TIMEOUT_MS = 20_000
const MAX_TICKETS = 10 // in-flight tickets that get per-ticket calls (task-list + comments)
const MAX_PRS = 6 // open PRs that get the three gh detail calls
const MAX_DONE = 4 // done-today tickets that get one task-list call (for today's cost)
// Resolved once per module load from the shell, since the module has no process.env.
let HOME = ''
const usagePath = (kind: 'claude' | 'codex') => HOME + '/Library/Application Support/orca/orca-' + kind + '-usage.json'
const workspaces = () => HOME + '/orca/workspaces'
const AUTOMATION_NAME = (repo: string) => 'agent-linear-scan (' + repo + ')' // install.sh names it so

// USD per 1M tokens: [input, output, cache read]. Unknown model = no cost.
const PRICE: Record<string, [number, number, number]> = {
  'claude-sonnet-5-5': [3, 15, 0.3],
  'claude-opus-5-5': [15, 75, 1.5],
  'claude-haiku-4-5': [0.8, 4, 0.08],
}
const GPT_PRICE: [number, number, number] = [1.25, 10, 0.125]
const priceOf = (model: string) => PRICE[model] ?? (/(^|\/)gpt-/.test(model) ? GPT_PRICE : null)

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
let refreshing = false

const parseEnv = (text: string): Record<string, string> =>
  Object.fromEntries(
    text.split('\n').map(l => l.replace(/\s+#.*$/, '').trim()).filter(l => /^[A-Z_]+=/.test(l))
      .map(l => { const i = l.indexOf('='); return [l.slice(0, i), l.slice(i + 1).replace(/^'(.*)'$/, '$1')] }),
  )

const ticket = (i: any): Ticket => ({
  id: i.identifier, title: i.title ?? '', state: i.state?.type ?? '',
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

const addTok = (a: Tokens | undefined, b: Tokens | undefined): Tokens | undefined =>
  !a ? b : !b ? a : { in: a.in + b.in, out: a.out + b.out, cache: a.cache + b.cache }
const addCost = (a: Cost | undefined, b: Cost | undefined): Cost | undefined =>
  !a ? b : !b ? a : { usd: a.usd + b.usd, known: a.known || b.known, partial: a.partial || b.partial || a.known !== b.known }

// ---- token usage (Orca cache files) -------------------------------------------------------------
// One row per session × location that lies inside one of the asked worktree paths.
type Row = { path: string; model: string; in: number; out: number; cache: number }
let usageCache: { key: string; rows: Row[] } | null = null

const rowsOf = (kind: 'claude' | 'codex', data: any, paths: string[]): Row[] => {
  const rows: Row[] = []
  for (const s of data?.sessions ?? []) {
    const entries = kind === 'codex' ? (s.locationModelBreakdown ?? s.locationBreakdown ?? []) : (s.locationBreakdown ?? [])
    for (const l of entries) {
      const k = String(l.locationKey ?? '').replace(/^cwd:/, '')
      const p = paths.find(x => k === x || k.startsWith(x + '/'))
      if (!p) continue
      rows.push(kind === 'codex'
        ? { path: p, model: l.modelKey ?? s.primaryModel ?? '', in: (l.inputTokens ?? 0) - (l.cachedInputTokens ?? 0), out: l.outputTokens ?? 0, cache: l.cachedInputTokens ?? 0 }
        : { path: p, model: s.model ?? '', in: l.inputTokens ?? 0, out: l.outputTokens ?? 0, cache: l.cacheReadTokens ?? 0 })
    }
  }
  return rows
}

// jq projection used when the file is over the 4 MiB $.fs.read limit: keeps only sessions that touch the paths.
const JQ = '($paths | map("cwd:" + .)) as $ks | { sessions: [ .sessions[]? | select(any((.locationModelBreakdown // .locationBreakdown)[]?; .locationKey as $k | any($ks[]; . as $p | $k == $p or ($k | startswith($p + "/"))))) | { model, primaryModel, locationBreakdown, locationModelBreakdown } ] }'

async function loadUsage($: any, paths: string[]): Promise<Row[]> {
  if (paths.length === 0) return []
  let key = paths.join('|')
  for (const f of (['claude', 'codex'] as const).map(usagePath)) { try { key += '|' + ((await $.fs.stat(f))?.mtimeMs ?? '') } catch { key += '|-' } }
  if (usageCache?.key === key) return usageCache.rows
  const rows: Row[] = []
  for (const kind of ['claude', 'codex'] as const) {
    const f = usagePath(kind)
    let data: any = null
    try { data = json(await $.fs.read(f)) } catch {
      try { data = json((await $.process.run(['jq', '-c', '--argjson', 'paths', JSON.stringify(paths), JQ, f], { timeoutMs: TIMEOUT_MS })).stdout) } catch { data = null }
    }
    if (data) rows.push(...rowsOf(kind, data, paths))
  }
  usageCache = { key, rows }
  return rows
}

const usageFor = (rows: Row[], path: string): { tokens?: Tokens; cost?: Cost; model: string } => {
  const mine = rows.filter(r => r.path === path)
  if (mine.length === 0) return { model: '' }
  const tokens: Tokens = { in: 0, out: 0, cache: 0 }
  const cost: Cost = { usd: 0, known: false, partial: false }
  const byModel: Record<string, number> = {}
  for (const r of mine) {
    tokens.in += r.in; tokens.out += r.out; tokens.cache += r.cache
    byModel[r.model] = (byModel[r.model] ?? 0) + r.out
    const p = priceOf(r.model)
    if (p) { cost.known = true; cost.usd += (r.in * p[0] + r.out * p[1] + r.cache * p[2]) / 1e6 } else cost.partial = true
  }
  if (!cost.known) cost.partial = false
  const model = Object.entries(byModel).sort((a, b) => b[1] - a[1])[0]?.[0] ?? ''
  return { tokens, cost, model }
}

// ---- precheck -------------------------------------------------------------------------------
// Scheduled runs attach a precheckResult; a manual run may not, so this returns null for those.
// stdout line 1 "claimable=<csv> in_flight=<n> max=<n>" drives queue status; stderr "skip <id>: <reason>"
// drives per-card reasons. `linear_calls=` noise lines are excluded from the first-stderr-line surface.
const parsePrecheck = (r: any): Precheck | null => {
  const pr = r?.precheckResult
  if (!pr || typeof pr !== 'object') return null
  const stdout = String(pr.stdout ?? '')
  const stderr = String(pr.stderr ?? '')
  const claimRaw = /^claimable=([^\s]*)/m.exec(stdout)?.[1] ?? ''
  const claimable = claimRaw ? claimRaw.split(',').filter(Boolean) : []
  const inFlight = Number(/\bin_flight=(\d+)/.exec(stdout)?.[1] ?? 0)
  const max = Number(/\bmax=(\d+)/.exec(stdout)?.[1] ?? 0)
  const skips: Record<string, string> = {}
  const noisy: string[] = []
  for (const raw of stderr.split('\n')) {
    const line = raw.trim()
    if (!line || /^linear_calls=/.test(line)) continue
    const m = /^skip ([A-Z]+-\d+): (.*)$/.exec(line)
    if (m) skips[m[1]] = m[2]
    noisy.push(line)
  }
  return {
    runNumber: Number(r.runNumber ?? 0),
    exitCode: Number(pr.exitCode ?? 0),
    timedOut: pr.timedOut === true,
    error: String(pr.error ?? ''),
    claimable,
    inFlight,
    max,
    skips,
    firstStderrLine: noisy[0] ?? '',
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

async function refresh($: any, handoffs: Map<string, string>, old: Board): Promise<Board> {
  const now = Date.now()
  const at = new Date(now).toTimeString().slice(0, 5)
  let text = ''
  try { text = await $.fs.read(ENV) } catch { return { ...old, at, coordinatorAlive: undefined, error: 'No ' + ENV + ' here. Open a repo that uses the pipeline.' } }
  const env = parseEnv(text)
  const repoDir = workspaces() + '/' + base(env.AGENT_GH_REPO ?? '')
  // Every external call is serial, bounded by TIMEOUT_MS. `run` returns stdout on exit 0, '' otherwise.
  // `runOrca` tracks success: a non-zero exit, unparseable JSON, or an envelope with ok=false (e.g. Linear rate-limit)
  // leaves ok=false so the caller can preserve the last-good slice instead of blanking the board.
  const run = async (argv: string[]): Promise<string> => {
    try {
      const r = await $.process.run(argv, { timeoutMs: TIMEOUT_MS })
      return r.exitCode === 0 ? r.stdout : ''
    } catch { return '' }
  }
  const runJson = async (argv: string[]) => json(await run(argv))
  const runOrca = async (argv: string[]): Promise<OrcaResult> => {
    let r: { stdout: string; exitCode: number } | null = null
    try { r = await $.process.run(argv, { timeoutMs: TIMEOUT_MS }) } catch { return { ok: false, data: null } }
    if (!r || r.exitCode !== 0) return { ok: false, data: null }
    const d = json(r.stdout)
    if (d === null) return { ok: false, data: null }
    if (d && typeof d === 'object' && 'ok' in d && d.ok === false) return { ok: false, data: d }
    return { ok: true, data: d }
  }
  if (!HOME) HOME = (await run(['sh', '-c', 'printf %s "$HOME"'])).trim()
  const has = (t: Ticket, l: string) => t.labels.includes(l)

  const issuesResp = await runOrca(['orca', 'linear', 'list-issues', '--team', env.AGENT_LINEAR_TEAM, '--project', env.AGENT_LINEAR_PROJECT, '--json'])
  const linearOk = issuesResp.ok
  const all: Ticket[] = linearOk ? (issuesResp.data?.result?.issues ?? []).map(ticket) : []
  const error = linearOk ? '' : 'Linear 限流，沿用旧数据'
  const inFlight = all.filter(t => has(t, 'auto-impl') && t.state === 'started')
  // Comment reads for the ticket are shared between the handoff pass (≤3 failed tickets)
  // and the in-flight claimedAt loop, so an in-flight+failed ticket is fetched at most once.
  const commentsByTicket = new Map<string, ReturnType<typeof deepComments>>()
  const readComments = async (id: string) => {
    const cached = commentsByTicket.get(id)
    if (cached) return cached
    const fetched = deepComments(await runJson(['orca', 'linear', 'issue', id, '--comments', '--json']))
    commentsByTicket.set(id, fetched)
    return fetched
  }
  const failed: FailedTicket[] = all.filter(t => has(t, 'auto-failed') && t.state !== 'completed' && t.state !== 'canceled')
    .map(t => ({ ...t, handoff: handoffs.get(t.id) }))
  for (const t of failed.slice(0, 3)) {
    const comments = await readComments(t.id)
    const handoff = comments.filter(c => String(c.body).trim().startsWith('转人:'))
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
      if (!w.linkedLinearIssue || !w.path) continue
      const card: WorktreeCard = { path: w.path, workspaceStatus: w.workspaceStatus ?? '', comment: w.comment ?? '' }
      if (!cardByIssue.has(w.linkedLinearIssue)) cardByIssue.set(w.linkedLinearIssue, card)
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
        return !!row && !row[1].includes('Z') && row[2].includes('scripts/agent-gates/COORDINATOR')
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
    let phase = ticketPhase(t, { run: r, tasks: workers, gate: gates[t.id], lane: gateLane, pr: prOf(t.id)?.number, previous: previous?.phase })
    // Preserve downstream phases on missing reads, unless fresh tasks show correction work.
    if (previous && !workers.some(k => ['dispatched', 'pending', 'ready'].includes(k.status))) {
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

  // Tokens and est. cost per worktree path, summed per agent and per ticket.
  const rows = await loadUsage($, [...new Set(Object.values(ticketPaths).flat())])
  for (const a of agents) {
    if (a.path) {
      const usage = usageFor(rows, a.path)
      Object.assign(a, usage, { model: a.model || usage.model })
    }
  }
  let todayCost = 0
  const costOf = (id: string) => (ticketPaths[id] ?? []).reduce<{ tokens?: Tokens; cost?: Cost }>((acc, p) => { const u = usageFor(rows, p); return { tokens: addTok(acc.tokens, u.tokens), cost: addCost(acc.cost, u.cost) } }, {})
  for (const t of [...inFlight, ...doneToday]) {
    const u = costOf(t.id)
    t.tokens = u.tokens; t.cost = u.cost; t.gate = gates[t.id]; t.pr = prOf(t.id)?.number
    if (u.cost?.known) todayCost += u.cost.usd
  }

  const statsOut = (await run(['bash', 'scripts/agent-gates/agent-graph.sh', 'ledger', 'stats'])).trim().split('\n').filter(Boolean)
  const stats = statsOut.length ? statsOut : old.stats

  const queue = all.filter(t => has(t, 'ready-for-agent') && t.state === 'unstarted').sort((a, b) => (a.priority || 9) - (b.priority || 9))

  // On Linear failure every ticket-shaped slice falls back to the last-good board so the pane does not blank.
  const lastSuccessAt = linearOk ? now : old.lastSuccessAt
  const maxInFlight = Number(env.AGENT_MAX_IN_FLIGHT || 0) || old.maxInFlight
  if (!linearOk) {
    return {
      at, error, maxInFlight, phases: old.phases, coordinatorAlive,
      inFlight: old.inFlight, queue: old.queue, failed: old.failed as FailedTicket[], doneToday: old.doneToday,
      prs, agents: old.agents, lastRun, stats, todayCost: old.todayCost, automation, automationKnown, lastSuccessAt,
      lastPrecheck, nextRunAt, lastRunAt,
    }
  }
  return {
    at, error, maxInFlight, phases, coordinatorAlive, inFlight, queue, failed, doneToday, prs,
    agents: agentsOk ? agents : old.agents,
    lastRun, stats, todayCost: agentsOk ? todayCost : old.todayCost, automation, automationKnown, lastSuccessAt,
    lastPrecheck, nextRunAt, lastRunAt,
  }
}

async function tick($: any, handoffs: Map<string, string>) {
  const old = await read($, board)
  const b = await refresh($, handoffs, old)
  await update($, board, () => b)
  await update($, lastTickAt, () => Date.now())
  $.ui.status(G.work + ' ' + b.inFlight.length + '/' + b.maxInFlight + ' · ' + G.wait + ' ' + b.prs.length + (b.failed.length ? ' · ' + G.human + ' ' + b.failed.length : '') + ' · ' + G.queued + ' ' + b.queue.length)
}

async function manualRefresh($: any, handoffs: Map<string, string>) {
  if (refreshing) return
  refreshing = true
  try { await tick($, handoffs) } finally { refreshing = false }
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
  // comment falls back to this cache instead of blanking the "需要人" card.
  const handoffs = new Map<string, string>()
  // The plugin loads in every Claude session on the machine, unattended coordinators included.
  // Polling starts only in a session that opens the pane: each refresh costs ~3 GitHub REST
  // calls per open PR, and one timer per session exhausted the 5000/h quota.
  let polling = false
  on('session.start', async ($, e, next) => {
    await $.command.register({ name: 'swarm', description: 'Open the swarm board' })
    return next(e)
  })

  on('command.run', { command: 'swarm' }, async $ => {
    await $.ui.open({ id: PANE, title: 'Swarm' })
    void tick($, handoffs)
    if (!polling) {
      polling = true
      await update($, nowAtom, () => Date.now())
      $.clock.every(REFRESH_MS, () => tick($, handoffs))
      $.clock.every(1000, () => { void update($, nowAtom, () => Date.now()) })
    }
    return { text: '蜂群看板已打开。' }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Button } = $.ui.resolve(e)
    const [b, v, q, d, , tickMs] = await Promise.all([read($, board), read($, view), read($, showQueue), read($, detail), read($, nowAtom), read($, lastTickAt)])
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
    const tRow = (glyph: string, t: Ticket, color?: string) => {
      const w = cols - 22
      return <Text color={color}>{glyph} {t.id.padEnd(8)} {cut(t.title, w).padEnd(w)} {age(t.updatedAt, now).padStart(9)}</Text>
    }
    // Tasks view: swim lanes. One column per stage; a ticket is a small card that moves right.
    type Lane = { title: string; emoji: string; color?: string; items: Ticket[] }
    const stage = (t: Ticket): 1 | 2 | 3 => laneStage(t, b.agents)
    const handoffText = (t: FailedTicket) => {
      const lines = t.handoff?.trim().split(/\r?\n/) ?? []
      const reason = lines[0]?.match(/^转人:\s*(.*)/)?.[1]
      const action = lines.find(l => l.startsWith('需要你:'))?.slice('需要你:'.length).trim()
      return reason && action ? (
        <Box flexDirection="column">
          <Text color="yellow">卡点：{reason}</Text>
          <Text color="yellow">需要你：{action}</Text>
        </Box>
      ) : lines[0] ? <Text color="yellow">{lines[0]}</Text> : null
    }
    const intakeText = (t: Ticket) => <Text dimColor>处理好后，开 intake 会话：按 scripts/agent-gates/roles/intake.md 工作，处理 {t.id}</Text>
    // A ticket that already lives in 需要人 does not also appear in the in-flight lanes; lane counts match that dedup.
    const failedIds = new Set(b.failed.map(t => t.id))
    const inFlightNoHuman = b.inFlight.filter(t => !failedIds.has(t.id))
    const lanes: Lane[] = [
      { title: '待领', emoji: LANE_EMOJI.queue, items: b.queue },
      { title: '实现中', emoji: LANE_EMOJI.impl, items: inFlightNoHuman.filter(t => stage(t) === 1) },
      { title: 'gate/评审', emoji: LANE_EMOJI.gate, items: inFlightNoHuman.filter(t => stage(t) === 2) },
      { title: 'CI/合并', emoji: LANE_EMOJI.ci, items: inFlightNoHuman.filter(t => stage(t) === 3) },
      { title: '需要人', emoji: LANE_EMOJI.human, color: b.failed.length ? 'yellow' : undefined, items: b.failed },
      { title: '今日完成', emoji: LANE_EMOJI.done, items: b.doneToday },
    ]
    // Status emoji on the card head: prefer CI, then gate, then the lane emoji.
    const statusEmojiOf = (l: Lane, t: Ticket, pr?: Pr) => {
      if (pr && pr.ci.state !== 'none' && STATUS_EMOJI[pr.ci.state]) return STATUS_EMOJI[pr.ci.state]
      if (t.gate && t.gate.state !== 'none' && STATUS_EMOJI[t.gate.state]) return STATUS_EMOJI[t.gate.state]
      return l.emoji
    }
    // Why each queued ticket is still waiting. `null` means no precheck data yet; the row is skipped.
    const lp = (b as Board).lastPrecheck
    const queueReason = (t: Ticket): string | null => {
      if (!lp) return null
      if (lp.claimable.includes(t.id)) return '已入选，等 coordinator 领取'
      if (lp.skips[t.id]) return '未入选：' + lp.skips[t.id]
      return 'precheck 未选中（原因未知）'
    }
    const phaseLine = (t: Ticket) => {
      if (!inFlightNoHuman.some(x => x.id === t.id)) return null
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
      const dur = l.title === '待领' ? '' : mins(minutes(t.claimedAt || t.updatedAt, now))
      const facts: string[] = []
      if (t.gate && t.gate.sec) facts.push('🧪 ' + secs(t.gate.sec))
      if (pr && pr.ci.state !== 'none' && pr.ci.total > 0) facts.push('🚦 ' + pr.ci.done + '/' + pr.ci.total + (pr.ci.min ? ' ' + mins(pr.ci.min) : ''))
      if (d && pr && (pr.adds || pr.dels)) facts.push('📝 +' + pr.adds + '/−' + pr.dels)
      if (t.cost) facts.push('💲 ' + usd(t.cost))
      const head = statusEmojiOf(l, t, pr) + ' ' + t.id + (pr ? ' #' + pr.number : '')
      const headRoom = Math.max(0, w - (dur ? dur.length + 1 : 0))
      const reason = l.title === '待领' ? queueReason(t) : null
      return (
        <Box key={'l-' + t.id} flexDirection="column" marginBottom={1}>
          <Text bold color={l.color}>{padW(cutW(head, headRoom), headRoom)}{dur ? ' ' + dur : ''}</Text>
          {phaseLine(t)}
          <Text>{cutW(t.title, w)}</Text>
          {reason ? <Text dimColor>{cutW(reason, w)}</Text> : null}
          {l.title === '需要人' ? handoffText(t) : null}
          {facts.length > 0 ? <Text dimColor>{cutW(facts.join(' · '), w)}</Text> : null}
          {l.title === '需要人' ? intakeText(t) : null}
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
      if (!lp) return null
      return '⏳ 下次扫描 ' + nextRunText
        + ' · 上次扫描 run ' + lp.runNumber + ' ' + resultText(lp.exitCode)
        + ' · 可领 ' + lp.claimable.length + ' 张'
        + ' · 在途 ' + lp.inFlight + '/' + lp.max
    }
    const buildQueueWarnings = (): string[] => {
      const out: string[] = []
      if (lp) {
        if (b.queue.length > 0 && lp.claimable.length === 0 && lp.inFlight < lp.max) {
          let line = '⚠ 待领 ' + b.queue.length + ' 张但 precheck 判定可领 0 张（run ' + lp.runNumber + '，退出码 ' + lp.exitCode + '）'
          if (lp.firstStderrLine) line += '：' + lp.firstStderrLine
          out.push(line)
        }
        if (lp.timedOut || lp.error) out.push('⚠ precheck 失败：' + (lp.error || '超时'))
      }
      const lastAt = (b as Board).lastRunAt ?? 0
      if (lastAt > 0) {
        const minsSince = Math.floor((now - lastAt) / 60_000)
        if (minsSince > 10) out.push('⚠ 定时任务超过 ' + minsSince + ' 分钟没有新运行')
      }
      return out
    }
    const queueExtras = (w: number) => {
      if (b.queue.length === 0) return null
      const status = buildQueueStatus()
      const warns = buildQueueWarnings()
      if (!status && warns.length === 0) return null
      return (
        <Box key="queue-extras" flexDirection="column">
          {status ? <Text dimColor>{cutW(status, w)}</Text> : null}
          {warns.map((text, i) => <Text key={'qw-' + i} color="yellow">{cutW(text, w)}</Text>)}
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
                {l.title === '待领' ? queueExtras(w) : null}
                {l.items.length === 0 ? <Text dimColor>·</Text> : l.items.map(t => laneCard(l, t, w))}
              </Box>
            ))}
          </Box>
        )
      }
      return lanes.filter(l => l.items.length > 0 || l.title === '实现中').map(l =>
        card('lane-' + l.title, l.emoji + ' ' + l.title + ' (' + l.items.length + ')', (
          <Box flexDirection="column">
            {l.title === '待领' ? queueExtras(cols - 4) : null}
            {l.items.length === 0 ? <Text dimColor>None.</Text> : l.items.map(t => laneCard(l, t, cols - 4))}
          </Box>
        ), l.color, l.items.length === 0))
    }
    // Swarm view: one agent is one line; workers indent under their coordinator.
    const agentRow = (a: Agent) => {
      const requiresAction = a.role === 'worker' && a.requiresAction === true
      const glyph = a.phase === 'done' ? G.done : a.phase === 'returned' ? G.human : G.work
      const name = a.role === 'coordinator' ? glyph + ' ' + a.ticket.padEnd(8) + ' coordinator' : '  └ ' + glyph + ' ' + (requiresAction ? '⚠ 等人 ' : '') + cut(a.name, 18)
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
    const focal = '  ' + b.inFlight.length + ' / ' + (b.maxInFlight || '?') + '  '

    return (
      <Box flexDirection="column">
        <Box flexDirection="row" alignItems="center" gap={2} marginBottom={1}>
          <Text bold inverse>{focal}</Text>
          <Text bold>in flight</Text>
          <Text>{b.failed.length === 0 ? '🟢' : b.failed.length < 3 ? '🟡' : '🔴'} 需要人 {b.failed.length}</Text>
          <Text dimColor>{G.queued} queued {b.queue.length} · {G.wait} PR {b.prs.length} · {G.human} needs you {b.failed.length} · {G.done} today {b.doneToday.length}{b.todayCost > 0 ? ' · today est. $' + b.todayCost.toFixed(2) : ''}</Text>
        </Box>
        <Box flexDirection="row" gap={2} marginBottom={1}>
          <Button key="view-swarm" hotkey="s" plain dimColor={v !== 'swarm'} label={v === 'swarm' ? '[swarm]' : ' swarm '} onPress={() => update($, view, () => 'swarm')} />
          <Button key="view-task" hotkey="t" plain dimColor={v !== 'task'} label={v === 'task' ? '[lanes]' : ' lanes '} onPress={() => update($, view, () => 'task')} />
          <Button key="toggle-detail" hotkey="d" plain dimColor={!d} label={d ? '[detail]' : ' detail '} onPress={() => update($, detail, x => !x)} />
          {automation
            ? <Button key="toggle-auto" hotkey="a" plain label={automation.enabled ? '● auto on' : '○ auto off'} onPress={() => toggleAutomation($, handoffs)} />
            : <Text dimColor>auto: {automationKnown ? '未安装' : '?'}</Text>}
          <Button key="refresh" hotkey="r" plain label={refreshing ? '↻ 刷新中…' : '↻ 刷新'} onPress={() => manualRefresh($, handoffs)} />
          <Text dimColor={!stale} color={stale ? 'yellow' : undefined}>{b.at
            ? 'updated ' + b.at + (stale ? ' · 数据已过期 ' + staleMin + ' 分' : countdown ? ' · ' + countdown : '')
            : 'loading…'}{b.lastRun ? ' · ' + b.lastRun : ''}</Text>
        </Box>
        {b.error ? <Text color="yellow">{b.error}</Text> : null}

        {v === 'swarm' ? card('agents', G.work + ' Agents', (
          b.agents.length === 0 ? <Text dimColor>No agent is working.</Text> : b.agents.map(agentRow)
        )) : laneView()}

        {v === 'swarm' ? card('prs', G.wait + ' Open PRs', b.prs.length === 0 ? <Text dimColor>None.</Text> : b.prs.map(prRow)) : null}

        {v === 'swarm' && b.failed.length > 0 ? card('failed', G.human + ' Needs you', b.failed.map(t => (
          <Box key={'f-' + t.id} flexDirection="column" marginBottom={1}>
            {tRow(G.human, t, 'yellow')}
            {handoffText(t)}
            {intakeText(t)}
          </Box>
        )), 'yellow') : null}

        {v === 'swarm' ? (
          <Box flexDirection="row" gap={1}>
            <Button key="toggle-queue" hotkey="q" plain label={(q ? '▾' : '▸') + ' queued ' + b.queue.length} onPress={() => update($, showQueue, x => !x)} />
          </Box>
        ) : null}
        {v === 'swarm' && q ? b.queue.map(t => <Text key={'q-' + t.id} dimColor>{G.queued} P{t.priority || '-'} {t.id.padEnd(8)} {cut(t.title, cols - 18)}</Text>) : null}

        {b.stats.length > 0 ? <Box marginTop={1} flexDirection="column">{b.stats.map((s, i) => <Text key={'s-' + i} dimColor>{s}</Text>)}</Box> : null}
      </Box>
    )
  })
}
