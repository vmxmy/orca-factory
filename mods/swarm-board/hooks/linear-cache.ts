import type { GraphData } from '../types'

const GRAPH_TTL_MS = 300_000
const GRAPH_MAX_CALLS = 40
export const GRAPH_COUNT_FILE = 'orca-factory-linear-calls'
const GRAPH_COUNT_SOURCE = 'board'
const RECENT_COMPLETED_MS = 24 * 60 * 60 * 1000

export type GraphDeps = {
  /** data is the parsed JSON envelope emitted by orca --json. */
  run(argv: string[]): Promise<{ ok: boolean; data: unknown }>
  env: { team: string; project: string }
  /** Append to GRAPH_COUNT_FILE under the caller's temporary directory. */
  appendCount(line: string): Promise<void>
}

export type GraphResult = { graph: GraphData; stale: boolean; incomplete: string[] }

type Parent = { identifier: string } | null
type Issue = { identifier: string; state: { type: string }; updatedAt: string; parent?: Parent }
type Relation = { relationship: string; relatedIssue: { identifier: string } }
type ListResult = { issues: Issue[]; truncated?: boolean; meta?: { partial?: boolean; hasMore?: boolean } }
type DetailResult = { issue: Issue; relations: Relation[] }

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isParent(value: unknown): value is Parent {
  return value === null || (isRecord(value) && typeof value.identifier === 'string')
}

function isIssue(value: unknown): value is Issue {
  return isRecord(value) && typeof value.identifier === 'string' && value.identifier.length > 0
    && isRecord(value.state) && typeof value.state.type === 'string'
    && typeof value.updatedAt === 'string' && Number.isFinite(Date.parse(value.updatedAt))
    && (value.parent === undefined || isParent(value.parent))
}

function isRelation(value: unknown): value is Relation {
  return isRecord(value) && typeof value.relationship === 'string'
    && isRecord(value.relatedIssue) && typeof value.relatedIssue.identifier === 'string'
}

function isListResult(value: unknown): value is ListResult {
  return isRecord(value) && Array.isArray(value.issues) && value.issues.every(isIssue)
    && (value.truncated === undefined || typeof value.truncated === 'boolean')
    && (value.meta === undefined || (isRecord(value.meta)
      && (value.meta.partial === undefined || typeof value.meta.partial === 'boolean')
      && (value.meta.hasMore === undefined || typeof value.meta.hasMore === 'boolean')))
}

function isDetailResult(value: unknown): value is DetailResult {
  return isRecord(value) && isIssue(value.issue)
    && Array.isArray(value.relations) && value.relations.every(isRelation)
    && isCompleteDetailMeta(value.meta)
}

function isCompleteDetailMeta(value: unknown): boolean {
  if (value === undefined) return true
  if (!isRecord(value) || (value.partial !== undefined && value.partial !== false)) return false
  if (value.sections === undefined) return true
  if (!isRecord(value.sections)) return false
  const relations = value.sections.relations
  return relations === undefined || (isRecord(relations)
    && (relations.capReached === undefined || relations.capReached === false)
    && (relations.hasMore === undefined || relations.hasMore === false))
}

// deps.run returns the parsed CLI envelope; validate its result once at this boundary.
function resultOf(data: unknown): unknown {
  return isRecord(data) && data.ok === true ? data.result : undefined
}

let cache: { at: number; result: GraphResult } | null = null
let callTimes: number[] = []

function staleResult(incomplete: string[]): GraphResult {
  return {
    graph: cache?.result.graph ?? { criticalPath: [], unlockRank: [] },
    stale: true,
    incomplete: [...new Set(incomplete)].sort(),
  }
}

function buildGraph(issues: Issue[], blockers: Map<string, string[]>): GraphData {
  const ids = issues.filter(issue => issue.state.type !== 'completed' && issue.state.type !== 'canceled')
    .map(issue => issue.identifier).sort()
  const pending = new Set(ids)
  const counts = new Map(ids.map(id => [id, 0]))
  for (const id of ids) {
    for (const blocker of new Set(blockers.get(id) ?? [])) {
      if (pending.has(blocker)) counts.set(blocker, (counts.get(blocker) ?? 0) + 1)
    }
  }

  const better = (a: string[], b: string[]) => a.length > b.length
    || (a.length === b.length && a.join('\n') < b.join('\n'))
  // Memoize acyclic chains only: paths cut at a cycle depend on their ancestors.
  const memo = new Map<string, string[]>()
  const chain = (id: string, visiting: Set<string>): { path: string[]; cyclic: boolean } => {
    if (visiting.has(id)) return { path: [], cyclic: true }
    const saved = memo.get(id)
    if (saved) return { path: saved, cyclic: false }
    visiting.add(id)
    let path: string[] = []
    let cyclic = false
    for (const blocker of [...new Set(blockers.get(id) ?? [])].filter(id => pending.has(id)).sort()) {
      const upstream = chain(blocker, visiting)
      cyclic = cyclic || upstream.cyclic
      if (better(upstream.path, path)) path = upstream.path
    }
    visiting.delete(id)
    path = [...path, id]
    if (!cyclic) memo.set(id, path)
    return { path, cyclic }
  }
  let criticalPath: string[] = []
  for (const id of ids) {
    const { path } = chain(id, new Set())
    if (better(path, criticalPath)) criticalPath = path
  }
  return {
    criticalPath,
    unlockRank: ids.map(id => ({ id, count: counts.get(id) ?? 0 }))
      .sort((a, b) => b.count - a.count || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)),
  }
}

export async function readGraphSnapshot(now: number, deps: GraphDeps): Promise<GraphResult> {
  if (cache && now - cache.at < GRAPH_TTL_MS) return cache.result

  const read = async (args: string[]): Promise<{ kind: 'budget' | 'failed' } | { kind: 'read'; data: unknown }> => {
    callTimes = callTimes.filter(time => time >= now - GRAPH_TTL_MS)
    if (callTimes.length >= GRAPH_MAX_CALLS) return { kind: 'budget' }
    // Reserve before awaiting so overlapping readers cannot exceed the shared budget.
    callTimes.push(now)
    // Keep append errors outside the run catch so the caller observes their rejection.
    await deps.appendCount(`${Math.floor(now / 1000)} ${GRAPH_COUNT_SOURCE}\n`)
    try {
      const response = await deps.run(['orca', 'linear', ...args, '--json'])
      return response.ok ? { kind: 'read', data: resultOf(response.data) } : { kind: 'failed' }
    } catch {
      return { kind: 'failed' }
    }
  }

  const listed = await read(['list-issues', '--team', deps.env.team, '--project', deps.env.project])
  if (listed.kind !== 'read' || !isListResult(listed.data)
    || listed.data.truncated || listed.data.meta?.partial || listed.data.meta?.hasMore) {
    return staleResult(cache?.result.graph.unlockRank.map(issue => issue.id) ?? [])
  }
  // Retain the last day's completed tickets as context; only pending tickets rank.
  const issues = listed.data.issues.filter(issue => issue.state.type !== 'canceled'
    && (issue.state.type !== 'completed' || Date.parse(issue.updatedAt) >= now - RECENT_COMPLETED_MS))
    .sort((a, b) => Number(a.state.type === 'completed') - Number(b.state.type === 'completed')
      || (a.identifier < b.identifier ? -1 : a.identifier > b.identifier ? 1 : 0))
  const blockers = new Map<string, string[]>()
  const incomplete = new Set<string>()
  let failed = false
  for (const issue of issues) {
    // Verified CLI lists omit parent. The relation read also supplies issue context,
    // avoiding a second detail call solely for parent when the list already has it.
    const detail = await read(['issue', issue.identifier, '--relations'])
    if (detail.kind !== 'read' || !isDetailResult(detail.data)
      || detail.data.issue.identifier !== issue.identifier) {
      incomplete.add(issue.identifier)
      if (detail.kind !== 'budget') failed = true
      continue
    }
    // Current CLI detail samples also omit parent; report this missing data explicitly.
    if (issue.parent === undefined && detail.data.issue.parent === undefined) incomplete.add(issue.identifier)
    blockers.set(issue.identifier, detail.data.relations
      .filter(relation => relation.relationship === 'blockedBy')
      .map(relation => relation.relatedIssue.identifier))
  }
  if (failed) return staleResult([...incomplete])
  const result: GraphResult = { graph: buildGraph(issues, blockers), stale: false, incomplete: [...incomplete].sort() }
  cache = { at: now, result }
  return result
}
