export const GRAPH_TTL_MS = 300_000
export const GRAPH_MAX_CALLS = 40

const GRAPH_WINDOW_MS = GRAPH_TTL_MS
const SOURCE = 'board'
const COUNTER_FILE = 'orca-factory-linear-calls'
const TIMEOUT_MS = 30_000
const RECENT_COMPLETED_MS = 86_400_000

export type Runner = {
  process: {
    run(argv: string[], o: { timeoutMs: number }): Promise<{ stdout: string; exitCode: number }>
  }
  fs: { append?: (path: string, content: string) => Promise<unknown> }
}

export type GraphIssue = {
  id: string
  title: string
  state: string
  parent: string | null
  blockedBy: string[]
}

export type GraphSnapshot = {
  issues: GraphIssue[]
  stale: boolean
  incomplete: string[]
  at: number
}

type Row = Record<string, unknown>
type Issue = Row & { identifier: string; title: string; state: { type: string } }

type CacheState = {
  cache: { at: number; value: GraphSnapshot } | null
  inFlight: Promise<GraphSnapshot> | null
}
const states = new Map<string, CacheState>()
let active: CacheState | null = null
// Match the board test suite's explicit module-cache reset for host isolation.
export function __resetGraphCacheForTests() {
  states.clear()
  active = null
  windowStart = null
  calls = 0
}

// The budget window survives cache hits and failed refreshes.
let windowStart: number | null = null
let calls = 0

function record(value: unknown): value is Row {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function issue(value: unknown): value is Issue {
  return record(value) && typeof value.identifier === 'string' && value.identifier.length > 0
    && typeof value.title === 'string' && record(value.state) && typeof value.state.type === 'string'
}

function parentOf(row: Row): string | null | undefined {
  if (row.parent === null) return null
  return record(row.parent) && typeof row.parent.identifier === 'string'
    && row.parent.identifier.length > 0 ? row.parent.identifier : undefined
}

function blockersOf(value: unknown): string[] | null {
  if (!Array.isArray(value)) return null
  const blockers: string[] = []
  for (const relation of value) {
    if (!record(relation) || typeof relation.relationship !== 'string') return null
    if (relation.relationship !== 'blockedBy') continue
    if (!record(relation.relatedIssue) || typeof relation.relatedIssue.identifier !== 'string'
      || !relation.relatedIssue.identifier) return null
    blockers.push(relation.relatedIssue.identifier)
  }
  return [...new Set(blockers)]
}

function completedAt(row: Issue): number {
  const at = typeof row.completedAt === 'string' ? row.completedAt : row.updatedAt
  return typeof at === 'string' ? Date.parse(at) : NaN
}

// Redraws only inspect the cache; this path never calls Linear.
export function peekGraphSnapshot(): GraphSnapshot | null {
  const cache = active?.cache
  if (!cache) return null
  return Date.now() - cache.at >= GRAPH_TTL_MS ? { ...cache.value, stale: true } : cache.value
}

export const readGraphSnapshot = async (
  now: number,
  $: Runner,
  ctx: { team: string; project: string; tmpDir: string },
): Promise<GraphSnapshot> => {
  const key = JSON.stringify([ctx.team, ctx.project])
  let state = states.get(key)
  if (!state) {
    state = { cache: null, inFlight: null }
    states.set(key, state)
  }
  active = state
  if (state.cache && now - state.cache.at < GRAPH_TTL_MS) return state.cache.value
  if (state.inFlight) return state.inFlight
  state.inFlight = refreshGraphSnapshot(now, $, ctx, state)
  try { return await state.inFlight } finally { state.inFlight = null }
}

const refreshGraphSnapshot = async (
  now: number,
  $: Runner,
  ctx: { team: string; project: string; tmpDir: string },
  state: CacheState,
): Promise<GraphSnapshot> => {
  async function spend(argv: string[]): Promise<Row | null> {
    if (windowStart === null || now - windowStart >= GRAPH_WINDOW_MS) {
      windowStart = now
      calls = 0
    }
    if (calls >= GRAPH_MAX_CALLS) return null
    calls += 1
    const counterPath = `${ctx.tmpDir.replace(/\/$/, '')}/${COUNTER_FILE}`
    const timestamp = String(Math.floor(now / 1000))
    const append = $.fs.append
    const counted = append
      ? Promise.resolve().then(() => append.call($.fs, counterPath, `${timestamp} ${SOURCE}\n`))
      : Promise.resolve().then(() => $.process.run(['sh', '-c',
        'printf "%s board\\n" "$1" >> "$2"', 'board-counter', timestamp, counterPath],
      { timeoutMs: TIMEOUT_MS }))
    // Counter writes are best effort, as in the existing shell writers.
    await counted.catch(() => undefined)
    const response = await $.process.run(argv, { timeoutMs: TIMEOUT_MS })
    if (response.exitCode !== 0) throw new Error('Linear read failed')
    const envelope: unknown = JSON.parse(response.stdout)
    if (!record(envelope) || envelope.ok === false || !record(envelope.result)) {
      throw new Error('Invalid Linear response')
    }
    const result = envelope.result
    const meta = record(result.meta) ? result.meta : {}
    const sections = record(meta.sections) ? Object.values(meta.sections) : []
    if (result.truncated === true || meta.partial === true || meta.hasMore === true
      || (Array.isArray(meta.includeErrors) && meta.includeErrors.length > 0)
      || sections.some(section => record(section) && (section.hasMore === true || section.capReached === true))) {
      throw new Error('Partial Linear response')
    }
    return result
  }

  try {
    const listed = await spend(['orca', 'linear', 'list-issues', '--team', ctx.team,
      '--project', ctx.project, '--json'])
    if (!listed || !Array.isArray(listed.issues) || !listed.issues.every(issue)) {
      throw new Error('Incomplete Linear list')
    }

    const selected = listed.issues.filter(row => row.state.type !== 'canceled'
      && (row.state.type !== 'completed' || completedAt(row) >= now - RECENT_COMPLETED_MS))
      .sort((a, b) => Number(a.state.type === 'completed') - Number(b.state.type === 'completed'))
    // Orca currently omits parent from both list and issue payloads. Root filtering
    // proves null parents; immediate children establish the remaining links.
    const parents = new Map<string, string | null>()
    for (const row of listed.issues) {
      const parent = parentOf(row)
      if (parent !== undefined) parents.set(row.identifier, parent)
    }
    if (selected.some(row => !parents.has(row.identifier))) {
      const roots = await spend(['orca', 'linear', 'list-issues', '--team', ctx.team,
        '--project', ctx.project, '--parent-id', 'null', '--json'])
      if (roots) {
        if (!Array.isArray(roots.issues) || !roots.issues.every(issue)) throw new Error('Invalid Linear roots')
        for (const root of roots.issues) parents.set(root.identifier, null)
      }
    }
    const issues: GraphIssue[] = selected.map(row => ({
      id: row.identifier, title: row.title, state: row.state.type,
      parent: parents.get(row.identifier) ?? null, blockedBy: [],
    }))
    function readChildren(detail: Row, id: string) {
      if (!issue(detail.issue) || detail.issue.identifier !== id || !Array.isArray(detail.children)
        || !detail.children.every((child): child is Row & { identifier: string } =>
          record(child) && typeof child.identifier === 'string' && child.identifier.length > 0)) {
        throw new Error('Invalid Linear children')
      }
      for (const child of detail.children) parents.set(child.identifier, id)
    }
    const incomplete = new Set<string>()
    for (const current of issues) {
      const detail = await spend(['orca', 'linear', 'issue', current.id,
        '--children', '--depth', '1', '--relations', '--json'])
      const parent = detail && record(detail.issue) ? parentOf(detail.issue) : undefined
      if (parent !== undefined) parents.set(current.id, parent)
      if (detail) readChildren(detail, current.id)
      const blockedBy = detail ? blockersOf(detail.relations) : null
      if (detail && blockedBy === null) throw new Error('Invalid Linear relations')
      if (blockedBy === null) incomplete.add(current.id)
      else current.blockedBy = blockedBy
    }
    const selectedIds = new Set(issues.map(row => row.id))
    for (const candidate of listed.issues) {
      if (!issues.some(row => !parents.has(row.id))) break
      if (selectedIds.has(candidate.identifier)) continue
      const detail = await spend(['orca', 'linear', 'issue', candidate.identifier,
        '--children', '--depth', '1', '--json'])
      if (!detail) break
      readChildren(detail, candidate.identifier)
    }
    for (const current of issues) {
      current.parent = parents.get(current.id) ?? null
      if (!parents.has(current.id)) incomplete.add(current.id)
    }

    const value: GraphSnapshot = { issues, stale: false, incomplete: [...incomplete], at: now }
    state.cache = { at: now, value }
    return value
  } catch {
    const cache = state.cache
    const value: GraphSnapshot = cache ? { ...cache.value, stale: true }
      : { issues: [], stale: true, incomplete: [], at: 0 }
    if (cache) cache.value = value
    return value
  }
}
