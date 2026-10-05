import type { GraphData } from '../types'

export const GRAPH_TTL_MS = 300_000
export const GRAPH_MAX_CALLS = 40
// The CLI exposes updatedAt, not completedAt: recent means updated in the last week.
const RECENT_MS = 7 * 24 * 60 * 60 * 1000
const TIMEOUT_MS = 20_000

export type GraphIssue = {
  id: string
  title: string
  state: { name: string; type: string }
  /** undefined means unknown; null means confirmed root. */
  parent?: string | null
  blockedBy: string[]
  detailsRead: boolean
}
export type GraphSnapshot = GraphData & {
  issues: GraphIssue[]
  lastSuccessAt: number | null
  stale: boolean
  incomplete: boolean
  error: string
}
/** Pass the hook's $ capability object; no Node modules or installed packages required. */
export type GraphRuntime = {
  process: { run(argv: string[], options: { timeoutMs: number }): Promise<{ exitCode: number; stdout: string }> }
}
type Row = Record<string, unknown>
const record = (v: unknown): v is Row => v !== null && typeof v === 'object' && !Array.isArray(v)
const ref = (v: unknown): string | undefined => record(v) && typeof v.identifier === 'string' ? v.identifier : undefined
const rows = (v: unknown): Row[] => {
  if (!Array.isArray(v) || !v.every(record)) throw new Error('Invalid Linear array')
  return v
}
const issue = (v: Row): GraphIssue => {
  if (typeof v.identifier !== 'string' || typeof v.title !== 'string' || !record(v.state)
      || typeof v.state.name !== 'string' || typeof v.state.type !== 'string') throw new Error('Invalid Linear issue')
  return { id: v.identifier, title: v.title, state: { name: v.state.name, type: v.state.type },
    parent: v.parent === null ? null : ref(v.parent), blockedBy: [], detailsRead: false }
}
const partial = (result: Row): boolean => {
  const meta = record(result.meta) ? result.meta : {}
  const sections = record(meta.sections) ? Object.values(meta.sections) : []
  return result.truncated === true || meta.partial === true || meta.hasMore === true
    || sections.some(s => record(s) && (s.hasMore === true || s.mayHaveMore === true || s.capReached === true))
}

/** One instance per project. Refresh from an effect/timer; render only peekGraphSnapshot. */
export const createGraphCache = (runtime: GraphRuntime, project: string) => {
  let snapshot: GraphSnapshot | null = null
  let pending: Promise<GraphSnapshot | null> | null = null
  let windowAt: number | null = null
  let calls = 0
  const empty = (): GraphSnapshot => ({ criticalPath: [], unlockRank: [], issues: [], lastSuccessAt: null,
    stale: true, incomplete: true, error: '' })

  const read = async (args: string[], now: number): Promise<Row | null> => {
    if (calls >= GRAPH_MAX_CALLS) return null
    calls++ // Failed commands spend quota too; no hidden retries.
    // Append in the shell because the hook sandbox has no fs append API or process.env.
    // Positional parameters keep the timestamp out of shell syntax. Count before dispatch.
    const counted = await runtime.process.run(['sh', '-c',
      'printf "%s board\\n" "$1" >> "${AGENT_LINEAR_COUNT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-calls}"',
      'graph-counter', String(Math.floor(now / 1000))], { timeoutMs: TIMEOUT_MS })
    if (counted.exitCode !== 0) throw new Error('Cannot record Linear call')
    const response = await runtime.process.run(['orca', 'linear', ...args, '--json'], { timeoutMs: TIMEOUT_MS })
    if (response.exitCode !== 0) throw new Error('Linear read failed')
    const envelope: unknown = JSON.parse(response.stdout)
    if (!record(envelope) || envelope.ok !== true || !record(envelope.result)) throw new Error('Invalid Linear response')
    return envelope.result
  }

  const refresh = async (now: number): Promise<GraphSnapshot | null> => {
    try {
      if (!project.trim()) throw new Error('Linear project is required')
      if (windowAt === null || now - windowAt >= GRAPH_TTL_MS) { windowAt = now; calls = 0 }
      // No limit means the CLI returns every project match, including older parents.
      const listed = await read(['list-issues', '--project', project], now)
      if (!listed) {
        snapshot = { ...(snapshot ?? empty()), stale: true, incomplete: true, error: '数据不全：读取预算已用完' }
        return snapshot
      }
      // A missing inbound edge does not prove a root: its parent may be outside this project.
      const roots = await read(['list-issues', '--project', project, '--parent-id', 'null'], now)
      const rootIds = new Set(roots ? rows(roots.issues).map(r => issue(r).id) : [])
      let incomplete = partial(listed) || !roots || partial(roots)
      const all = rows(listed.issues).map(raw => ({ raw, node: issue(raw) }))
      const selected = all.filter(({ raw, node }) => !['completed', 'canceled'].includes(node.state.type)
        || (node.state.type === 'completed' && typeof raw.updatedAt === 'string' && Date.parse(raw.updatedAt) >= now - RECENT_MS))
      const wanted = new Set(selected.map(x => x.node.id))
      // Prioritize visible tickets; older parents still need child reads to resolve ancestry.
      const ordered = [...selected, ...all.filter(x => !wanted.has(x.node.id))]
      const parents = new Map<string, string>()
      for (const { raw, node } of ordered) {
        const workspace = record(raw.workspace) && typeof raw.workspace.id === 'string' ? ['--workspace', raw.workspace.id] : []
        // Verified CLI list/issue summaries omit parent. children gives authoritative inverse edges.
        const detail = await read(['issue', node.id, '--relations', '--children', '--depth', '1', ...workspace], now)
        if (!detail) { incomplete = true; continue }
        if (partial(detail)) { incomplete = true }
        if (!record(detail.issue) || ref(detail.issue) !== node.id) throw new Error('Invalid Linear detail')
        const current = issue(detail.issue)
        node.state = current.state
        node.title = current.title
        node.parent = current.parent ?? node.parent
        if (detail.issue.parent === null) node.parent = null
        node.blockedBy = rows(detail.relations).filter(r => r.relationship === 'blockedBy'
          || r.type === 'blocked-by' || (r.type === 'blocks' && r.direction === 'inbound')).map(r => {
            const id = ref(r.relatedIssue)
            if (!id) throw new Error('Invalid Linear blocker')
            return id
          })
        for (const child of rows(detail.children)) {
          const id = ref(child)
          if (!id) throw new Error('Invalid Linear child')
          parents.set(id, node.id)
        }
        node.detailsRead = !partial(detail)
      }
      for (const { node } of selected) {
        node.parent = parents.get(node.id) ?? node.parent
        if (node.parent === undefined && rootIds.has(node.id)) node.parent = null
        if (node.parent === undefined) incomplete = true
      }
      snapshot = { criticalPath: [], unlockRank: [], issues: selected.map(x => x.node), lastSuccessAt: now,
        stale: false, incomplete, error: incomplete ? '数据不全' : '' }
    } catch (error) {
      // A failed batch never replaces last-good issues, including failures during enrichment.
      snapshot = { ...(snapshot ?? empty()), stale: true,
        error: error instanceof Error ? error.message : String(error) }
    }
    return snapshot
  }

  return {
    readGraphSnapshot(now: number): Promise<GraphSnapshot | null> {
      if (!Number.isFinite(now)) return Promise.reject(new Error('Invalid graph timestamp'))
      if (pending) return pending
      if (snapshot?.lastSuccessAt !== null && snapshot?.lastSuccessAt !== undefined
          && now >= snapshot.lastSuccessAt && now - snapshot.lastSuccessAt < GRAPH_TTL_MS) return Promise.resolve(snapshot)
      pending = refresh(now).finally(() => { pending = null })
      return pending
    },
    /** Pure: even an expired snapshot never triggers a Linear call during redraw. */
    peekGraphSnapshot(now: number): GraphSnapshot | null {
      if (!snapshot || snapshot.stale || snapshot.lastSuccessAt === null || now - snapshot.lastSuccessAt < GRAPH_TTL_MS) return snapshot
      return { ...snapshot, stale: true }
    },
  }
}

let cache: ReturnType<typeof createGraphCache> | undefined
/** Bind once in the refresh hook, after reading AGENT_LINEAR_PROJECT; rebind on project change. */
export const configureGraphCache = (runtime: GraphRuntime, project: string): void => {
  cache = createGraphCache(runtime, project)
}
export const readGraphSnapshot = (now: number): Promise<GraphSnapshot | null> =>
  cache ? cache.readGraphSnapshot(now) : Promise.resolve(null)
export const peekGraphSnapshot = (now: number): GraphSnapshot | null => cache?.peekGraphSnapshot(now) ?? null
