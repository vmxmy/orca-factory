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

let cache: { at: number; value: GraphSnapshot } | null = null
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
  if (!cache) return null
  return Date.now() - cache.at >= GRAPH_TTL_MS ? { ...cache.value, stale: true } : cache.value
}

export const readGraphSnapshot = async (
  now: number,
  $: Runner,
  ctx: { team: string; project: string; tmpDir: string },
): Promise<GraphSnapshot> => {
  if (cache && now - cache.at < GRAPH_TTL_MS) return cache.value

  async function spend(argv: string[]): Promise<Row | null> {
    if (windowStart === null || now - windowStart >= GRAPH_WINDOW_MS) {
      windowStart = now
      calls = 0
    }
    if (calls >= GRAPH_MAX_CALLS) return null
    calls += 1
    const append = $.fs.append
    const counted = append
      ? Promise.resolve().then(() => append.call($.fs,
        `${ctx.tmpDir.replace(/\/$/, '')}/${COUNTER_FILE}`, `${Math.floor(now / 1000)} ${SOURCE}\n`))
      : Promise.resolve().then(() => $.process.run(['sh', '-c',
        'printf "%s board\\n" "$(date +%s)" >> "${TMPDIR:-/tmp}/orca-factory-linear-calls"'],
      { timeoutMs: TIMEOUT_MS }))
    // Counter writes are best effort, as in the existing shell writers.
    await counted.catch(() => undefined)
    try {
      const response = await $.process.run(argv, { timeoutMs: TIMEOUT_MS })
      if (response.exitCode !== 0) return null
      const envelope: unknown = JSON.parse(response.stdout)
      return record(envelope) && envelope.ok !== false && record(envelope.result) ? envelope.result : null
    } catch {
      return null
    }
  }

  const listed = await spend(['orca', 'linear', 'list-issues', '--team', ctx.team,
    '--project', ctx.project, '--json'])
  if (!listed || !Array.isArray(listed.issues) || !listed.issues.every(issue)) {
    return cache ? { ...cache.value, stale: true } : { issues: [], stale: true, incomplete: [], at: 0 }
  }

  const selected = listed.issues.filter(row => row.state.type !== 'canceled'
    && (row.state.type !== 'completed' || completedAt(row) >= now - RECENT_COMPLETED_MS))
    .sort((a, b) => Number(a.state.type === 'completed') - Number(b.state.type === 'completed'))
  const issues: GraphIssue[] = []
  const incomplete: string[] = []
  for (const row of selected) {
    const current: GraphIssue = {
      id: row.identifier, title: row.title, state: row.state.type,
      parent: parentOf(row) ?? null, blockedBy: [],
    }
    issues.push(current)
    let complete = true
    if (!Object.prototype.hasOwnProperty.call(row, 'parent')) {
      const detail = await spend(['orca', 'linear', 'issue', row.identifier, '--json'])
      if (!detail && calls >= GRAPH_MAX_CALLS) {
        incomplete.push(row.identifier)
        continue
      }
      const parent = detail && record(detail.issue) ? parentOf(detail.issue) : undefined
      if (parent === undefined) complete = false
      else current.parent = parent
    } else if (parentOf(row) === undefined) {
      complete = false
    }
    const relations = await spend(['orca', 'linear', 'issue', row.identifier, '--relations', '--json'])
    const blockedBy = relations ? blockersOf(relations.relations) : null
    if (blockedBy === null) complete = false
    else current.blockedBy = blockedBy
    if (!complete) incomplete.push(row.identifier)
  }

  const value: GraphSnapshot = { issues, stale: false, incomplete, at: now }
  cache = { at: now, value }
  return value
}
