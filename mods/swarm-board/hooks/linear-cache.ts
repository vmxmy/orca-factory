export const GRAPH_TTL_MS = 300_000
export const GRAPH_MAX_CALLS = 40
// Linear's list response has updatedAt, but no completedAt: recent means updated in 7 days.
const RECENT_MS = 7 * 24 * 60 * 60 * 1000

type RecordValue = Record<string, unknown>
export type GraphIssue = {
  id: string
  title: string
  state: string
  parent: string | null
  blockedBy: string[]
  incomplete: boolean
}
export type GraphSnapshot = {
  issues: GraphIssue[]
  lastSuccessAt: number | null
  stale: boolean
  incomplete: boolean
  status: '' | '数据不全' | '数据过期'
  error: string
}
export type GraphRuntime = {
  process: { run(argv: string[], options: { timeoutMs: number }): Promise<{ exitCode: number; stdout: string }> }
}
export type GraphScope = { project: string; team?: string; workspace?: string }
const object = (value: unknown): RecordValue =>
  value !== null && typeof value === 'object' && !Array.isArray(value) ? value as RecordValue : {}
const identifier = (value: unknown): string => {
  const item = object(value)
  return typeof item.identifier === 'string' ? item.identifier : typeof item.id === 'string' ? item.id : ''
}
const items = (value: unknown): unknown[] => Array.isArray(value) ? value : []
const partial = (result: RecordValue): boolean => {
  const meta = object(result.meta)
  return result.truncated === true || meta.partial === true || meta.hasMore === true
    || items(meta.includeErrors).length > 0 || items(meta.workspaceErrors).length > 0
    || Object.values(object(meta.sections)).some(value => {
      const section = object(value)
      return section.capReached === true || section.hasMore === true || section.mayHaveMore === true
    })
}

/** One reader per project/pane. Uses the hook sandbox process API, not Node modules.
 * Call readGraphSnapshot only from the refresh handler; render with peekGraphSnapshot.
 * Live Orca verification: list-issues and issue omit parent; issue --children supplies
 * direct children, so invert those edges when an explicit parent is not available.
 */
export function createGraphSnapshotReader(runtime: GraphRuntime, scope: GraphScope) {
  if (!scope.project.trim()) throw new Error('Graph snapshot requires a Linear project')
  let cache: GraphSnapshot | null = null
  let pending: Promise<GraphSnapshot> | null = null
  let windowStart: number | null = null
  let calls = 0
  const scopeArgs = scope.workspace ? ['--workspace', scope.workspace] : []
  const empty = (): GraphSnapshot => ({ issues: [], lastSuccessAt: null, stale: true, incomplete: true, status: '数据过期', error: '' })
  const peekGraphSnapshot = (now: number): GraphSnapshot | null => {
    if (!cache) return null
    if (cache.lastSuccessAt === null || now < cache.lastSuccessAt || now - cache.lastSuccessAt >= GRAPH_TTL_MS) {
      return { ...cache, stale: true, status: '数据过期' }
    }
    return cache
  }
  const readGraphSnapshot = (now: number): Promise<GraphSnapshot> => {
    if (pending) return pending
    if (cache?.lastSuccessAt !== null && cache?.lastSuccessAt !== undefined
        && now >= cache.lastSuccessAt && now - cache.lastSuccessAt < GRAPH_TTL_MS) return Promise.resolve(cache)
    if (windowStart === null || now - windowStart >= GRAPH_TTL_MS) { windowStart = now; calls = 0 }
    const read = async (args: string[]): Promise<RecordValue | null> => {
      if (calls >= GRAPH_MAX_CALLS) return null
      calls++ // Failed attempts also consume budget; retries cannot bypass the window.
      // Append before dispatch. If accounting fails, do not make an uncounted Linear call.
      const counted = await runtime.process.run(['sh', '-c',
        'printf "%s board\\n" "$1" >> "${TMPDIR:-/tmp}/orca-factory-linear-calls"',
        'board', String(Math.floor(now / 1000))], { timeoutMs: 20_000 })
      if (counted.exitCode !== 0) throw new Error('Cannot record board Linear call')
      const response = await runtime.process.run(['orca', 'linear', ...args, ...scopeArgs, '--json'], { timeoutMs: 20_000 })
      if (response.exitCode !== 0) throw new Error('Linear read failed: exit ' + response.exitCode)
      const envelope = object(JSON.parse(response.stdout))
      if (envelope.ok !== true || !envelope.result || typeof envelope.result !== 'object') throw new Error('Invalid Linear response')
      return object(envelope.result)
    }
    const refresh = async (): Promise<GraphSnapshot> => {
      try {
        const result = await read(['list-issues', '--project', scope.project, ...(scope.team ? ['--team', scope.team] : [])])
        if (!result) {
          cache = { ...(cache ?? empty()), stale: true, incomplete: true, status: '数据不全' }
          return cache
        }
        if (!Array.isArray(result.issues)) throw new Error('Missing Linear issues')
        // No limit: Orca returns all project issues. Filter locally so old open tickets survive.
        const all = result.issues.map(object)
        const selected = all.filter(issue => {
          const state = object(issue.state).type
          return state !== 'canceled' && (state !== 'completed' || Date.parse(String(issue.updatedAt)) >= now - RECENT_MS)
        })
        const nodes = new Map<string, GraphIssue>()
        let incomplete = partial(result)
        for (const issue of selected) {
          const id = identifier(issue)
          if (!id || typeof object(issue.state).type !== 'string') throw new Error('Invalid Linear issue')
          nodes.set(id, { id, title: String(issue.title ?? ''), state: String(object(issue.state).type),
            parent: identifier(issue.parent) || null, blockedBy: [], incomplete: true })
        }
        // Prioritize visible tickets. Other project tickets can be parents of visible children.
        const selectedIds = new Set(selected.map(identifier))
        const ordered = [...selected, ...all.filter(issue => !selectedIds.has(identifier(issue)))]
        const childParents = new Map<string, string>()
        let parentsComplete = !partial(result)
        for (const issue of ordered) {
          const id = identifier(issue)
          if (!id) throw new Error('Missing Linear identifier')
          const detail = await read(['issue', id, '--children', '--depth', '1', '--relations'])
          if (!detail) { incomplete = true; parentsComplete = false; break }
          if (!detail.issue || !Array.isArray(detail.children) || !Array.isArray(detail.relations)) throw new Error('Missing Linear issue detail')
          if (partial(detail)) { incomplete = true; parentsComplete = false }
          for (const child of detail.children) {
            const childId = identifier(child)
            if (childId) childParents.set(childId, id)
          }
          const node = nodes.get(id)
          if (!node) continue
          const full = object(detail.issue)
          if (typeof object(full.state).type !== 'string') throw new Error('Missing Linear state')
          node.state = String(object(full.state).type)
          node.parent = identifier(full.parent) || node.parent
          node.blockedBy = [...new Set(detail.relations.filter(value => {
            const relation = object(value)
            return relation.relationship === 'blockedBy' || relation.relationship === 'blocked-by'
              || (relation.type === 'blocks' && relation.direction === 'inbound')
          }).map(value => identifier(object(value).relatedIssue)).filter(Boolean))]
          node.incomplete = partial(detail)
        }
        for (const node of nodes.values()) {
          node.parent = childParents.get(node.id) ?? node.parent
          // An unscanned parent may own this ticket: null must not silently mean "root".
          if (!parentsComplete && !node.parent) node.incomplete = true
          incomplete ||= node.incomplete
        }
        cache = { issues: [...nodes.values()], lastSuccessAt: now, stale: false, incomplete,
          status: incomplete ? '数据不全' : '', error: '' }
      } catch (error) {
        // Publish atomically: never overwrite last-good graph with a half-failed refresh.
        cache = { ...(cache ?? empty()), stale: true, status: '数据过期', error: String(error) }
      }
      return cache
    }
    pending = refresh().finally(() => { pending = null })
    return pending
  }
  return { readGraphSnapshot, peekGraphSnapshot }
}

// Bind once during hook setup; no reads or timers run at import/setup/render time.
let reader: ReturnType<typeof createGraphSnapshotReader> | null = null
export function configureGraphSnapshot(runtime: GraphRuntime, scope: GraphScope): void {
  reader = createGraphSnapshotReader(runtime, scope)
}
export const readGraphSnapshot = (now: number): Promise<GraphSnapshot | null> =>
  reader ? reader.readGraphSnapshot(now) : Promise.resolve(null)
export const peekGraphSnapshot = (now: number): GraphSnapshot | null => reader?.peekGraphSnapshot(now) ?? null
