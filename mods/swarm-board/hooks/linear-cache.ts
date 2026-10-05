export const GRAPH_TTL_MS = 300_000
export const GRAPH_MAX_CALLS = 40

export type GraphNode = {
  id: string
  state: string
  parent: string | null
  blockedBy: string[]
  complete: boolean
}

export type GraphSnapshot = {
  readAt: number
  stale: boolean
  nodes: readonly GraphNode[]
}

export type Runner = {
  process: {
    run(argv: string[], opts: { timeoutMs: number }): Promise<{ stdout: string; exitCode: number }>
  }
}

type Row = Record<string, unknown>
type Issue = Row & { identifier: string; state: { type: string } }
type LinearRead = { data: Row | null; counted: boolean }

const TIMEOUT_MS = 30_000
const RECENT_COMPLETED_MS = 86_400_000
const COUNT_COMMAND = 'printf "%s board\\n" "$(date +%s)" >> "${AGENT_LINEAR_COUNT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-calls}"'

let cache: GraphSnapshot | null = null
let pending: Promise<GraphSnapshot | null> | null = null
let windowStart: number | null = null
let windowCalls = 0

function record(value: unknown): value is Row {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function issue(value: unknown): value is Issue {
  return record(value) && typeof value.identifier === 'string' && value.identifier.length > 0
    && record(value.state) && typeof value.state.type === 'string'
}

function partial(data: Row): boolean {
  const meta = record(data.meta) ? data.meta : null
  const sections = meta && record(meta.sections) ? Object.values(meta.sections) : []
  return data.truncated === true || meta?.partial === true || meta?.hasMore === true
    || sections.some(section => record(section)
      && (section.capReached === true || section.hasMore === true || section.mayHaveMore === true))
}

async function run($: Runner, argv: string[]): Promise<{ stdout: string; exitCode: number }> {
  try {
    return await $.process.run(argv, { timeoutMs: TIMEOUT_MS })
  } catch {
    // Transport failures follow the same observable stale/incomplete path as command failures.
    return { stdout: '', exitCode: 1 }
  }
}

async function readLinear($: Runner, args: string[]): Promise<LinearRead> {
  if (windowCalls >= GRAPH_MAX_CALLS) return { data: null, counted: true }
  windowCalls += 1
  const counter = await run($, ['sh', '-c', COUNT_COMMAND])
  const counted = counter.exitCode === 0
  const response = await run($, ['orca', 'linear', ...args, '--json'])
  if (response.exitCode !== 0) return { data: null, counted }
  try {
    const envelope: unknown = JSON.parse(response.stdout)
    return {
      data: record(envelope) && envelope.ok !== false && record(envelope.result) ? envelope.result : null,
      counted,
    }
  } catch {
    return { data: null, counted }
  }
}

function parentOf(row: Row, identifiers: Map<string, string>): string | null | undefined {
  if (row.parent === null || row.parentId === null) return null
  if (record(row.parent)) {
    if (typeof row.parent.identifier === 'string' && row.parent.identifier) return row.parent.identifier
    if (typeof row.parent.id === 'string' && row.parent.id) return identifiers.get(row.parent.id) ?? row.parent.id
  }
  if (typeof row.parentId === 'string' && row.parentId) return identifiers.get(row.parentId) ?? row.parentId
  return undefined
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

function priority(row: Issue): number {
  return typeof row.priority === 'number' && row.priority > 0 ? row.priority : Infinity
}

function staleSnapshot(): GraphSnapshot | null {
  if (cache) cache = { ...cache, stale: true }
  return cache
}

export function cachedGraphSnapshot(now: number): GraphSnapshot | null {
  if (!cache) return null
  return now - cache.readAt >= GRAPH_TTL_MS ? { ...cache, stale: true } : cache
}

async function refresh(now: number, $: Runner): Promise<GraphSnapshot | null> {
  // The hook sandbox exposes neither environment variables nor Node filesystem APIs.
  const config = await run($, ['sh', '-c',
    '. scripts/agent-gates/agent-graph.env && printf "%s\\n%s\\n" "$AGENT_LINEAR_TEAM" "$AGENT_LINEAR_PROJECT"'])
  const [team, project] = config.stdout.trim().split('\n')
  if (config.exitCode !== 0 || !team || !project) return staleSnapshot()
  const listed = await readLinear($, ['list-issues', '--team', team, '--project', project])
  if (!listed.data || !Array.isArray(listed.data.issues)
    || !listed.data.issues.every(issue) || partial(listed.data)) return staleSnapshot()

  const identifiers = new Map<string, string>()
  for (const row of listed.data.issues) {
    if (typeof row.id === 'string') identifiers.set(row.id, row.identifier)
  }
  const selected = listed.data.issues.filter(row => row.state.type !== 'canceled'
    && (row.state.type !== 'completed' || completedAt(row) >= now - RECENT_COMPLETED_MS))
    .sort((a, b) => {
      const aDone = a.state.type === 'completed'
      const bDone = b.state.type === 'completed'
      if (aDone !== bDone) return aDone ? 1 : -1
      return aDone ? completedAt(b) - completedAt(a) : priority(a) - priority(b)
    })
  const nodes: GraphNode[] = []
  let stale = !listed.counted
  for (const row of selected) {
    const listedParent = parentOf(row, identifiers)
    const base: GraphNode = {
      id: row.identifier, state: row.state.type, parent: listedParent ?? null,
      blockedBy: [], complete: false,
    }
    const detail = await readLinear($, ['issue', row.identifier, '--relations'])
    stale = stale || !detail.counted
    if (!detail.data || !issue(detail.data.issue)
      || detail.data.issue.identifier !== row.identifier || partial(detail.data)) {
      nodes.push(base)
      continue
    }
    const parent = listedParent !== undefined ? listedParent : parentOf(detail.data.issue, identifiers)
    const blockedBy = blockersOf(detail.data.relations)
    // Both verified CLI payloads omit parent, so absence cannot establish a root issue.
    nodes.push({
      ...base, parent: parent ?? null, blockedBy: blockedBy ?? [],
      complete: parent !== undefined && blockedBy !== null,
    })
  }
  // Failed accounting remains visible while successful Linear data is still retained.
  cache = { readAt: now, stale, nodes }
  return cache
}

export function readGraphSnapshot(now: number, $: Runner): Promise<GraphSnapshot | null> {
  if (pending) return pending
  if (cache && now - cache.readAt < GRAPH_TTL_MS) return Promise.resolve(cache)
  if (windowStart === null || now - windowStart >= GRAPH_TTL_MS) {
    windowStart = now
    windowCalls = 0
  }
  pending = refresh(now, $).finally(() => { pending = null })
  return pending
}
