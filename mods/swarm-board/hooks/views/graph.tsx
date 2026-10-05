/** The view consumes readGraphSnapshot's issues; it never fetches live data. */
export type GraphIssue = {
  id: string
  state: string
  parent?: string | null
  blockedBy?: readonly string[]
}
export type GraphInput = { issues: readonly GraphIssue[] }
export type GraphRow = GraphIssue & { depth: number; blockers: string[] }
export type GraphData = {
  ready: boolean
  rows: GraphRow[]
  criticalPath: string[]
  /** Immediate unlocks, then downstream reach as a tie-breaker. */
  unlockRank: { id: string; count: number; downstream: number }[]
  /** Cycles and their dependents cannot be assigned a DAG critical path. */
  unresolved: string[]
  summary: string
}
const finished = (state: string) => state === 'completed' || state === 'canceled'
const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0

export function buildGraph(input?: GraphInput | null): GraphData {
  if (!input || !Array.isArray(input.issues)) return {
    ready: false, rows: [], criticalPath: [], unlockRank: [], unresolved: [], summary: '数据未就绪',
  }
  const issues = [...input.issues].sort((a, b) => compare(a.id, b.id))
  const byId = new Map(issues.map(issue => [issue.id, issue]))
  const active = issues.filter(issue => !finished(issue.state))
  const blockers = new Map(issues.map(issue => [issue.id,
    [...new Set(issue.blockedBy ?? [])].filter(id => byId.has(id) && !finished(byId.get(id)!.state)).sort(compare),
  ]))
  const children = new Map<string, string[]>()
  for (const issue of active) {
    for (const id of blockers.get(issue.id) ?? []) {
      const list = children.get(id) ?? []
      list.push(issue.id)
      children.set(id, list)
    }
  }

  // Kahn traversal gives the longest unfinished dependency chain without recursion.
  const pending = new Map(active.map(issue => [issue.id,
    (blockers.get(issue.id) ?? []).length,
  ]))
  const queue = active.filter(issue => pending.get(issue.id) === 0).map(issue => issue.id)
  const paths = new Map(active.map(issue => [issue.id, [issue.id]]))
  let criticalPath: string[] = []
  const better = (a: string[], b: string[]) => a.length > b.length
    || (a.length === b.length && compare(a.join('\0'), b.join('\0')) < 0)
  const visited = new Set<string>()
  for (let i = 0; i < queue.length; i++) {
    const id = queue[i]
    visited.add(id)
    const path = paths.get(id)!
    if (better(path, criticalPath)) criticalPath = path
    for (const child of children.get(id) ?? []) {
      const next = [...path, child]
      if (better(next, paths.get(child)!)) paths.set(child, next)
      const remaining = pending.get(child)! - 1
      pending.set(child, remaining)
      if (remaining === 0) queue.push(child)
    }
  }
  const unresolved = active.filter(issue => !visited.has(issue.id)).map(issue => issue.id)
  const unlockRank = active.map(issue => {
    const reachable = new Set<string>([issue.id])
    const next = [issue.id]
    for (let i = 0; i < next.length; i++) {
      for (const child of children.get(next[i]) ?? []) {
        if (!reachable.has(child)) { reachable.add(child); next.push(child) }
      }
    }
    const count = (children.get(issue.id) ?? []).filter(id => id !== issue.id
      && blockers.get(id)?.length === 1).length
    return { id: issue.id, count, downstream: reachable.size - 1 }
  }).sort((a, b) => b.count - a.count || b.downstream - a.downstream || compare(a.id, b.id))

  // Parent hierarchy is independent of dependency edges: group siblings together.
  const families = new Map<string | null, GraphIssue[]>()
  for (const issue of issues) {
    const parent = issue.parent && byId.has(issue.parent) ? issue.parent : null
    const list = families.get(parent) ?? []
    list.push(issue)
    families.set(parent, list)
  }
  const rows: GraphRow[] = []
  const shown = new Set<string>()
  const append = (root: GraphIssue) => {
    const stack = [{ issue: root, depth: 0 }]
    while (stack.length) {
      const { issue, depth } = stack.pop()!
      if (shown.has(issue.id)) continue
      shown.add(issue.id)
      rows.push({ ...issue, blockedBy: [...(issue.blockedBy ?? [])], depth, blockers: [...(blockers.get(issue.id) ?? [])] })
      for (const child of [...(families.get(issue.id) ?? [])].reverse()) stack.push({ issue: child, depth: depth + 1 })
    }
  }
  for (const root of families.get(null) ?? []) append(root)
  // Preserve visibility even when a malformed parent relation contains a cycle.
  for (const issue of issues) if (!shown.has(issue.id)) append(issue)
  const waiting = active.filter(issue => issue.state !== 'started')
  const blocked = waiting.filter(issue => (blockers.get(issue.id)?.length ?? 0) > 0)
  const blockingIds = [...new Set(blocked.flatMap(issue => blockers.get(issue.id) ?? []))].sort(compare)
  const summary = `未完成 ${active.length} 张，在做 ${active.length - waiting.length} 张，按依赖可开始 ${waiting.length - blocked.length} 张`
    + (blocked.length ? `，${blocked.length} 张卡在 ${blockingIds.join('、')}` : '，没有依赖阻塞')
    + '（实际领取还受标签与并发限制）。'
  return { ready: true, rows, criticalPath, unlockRank, unresolved, summary }
}

/** Truncate terminal cells without splitting Unicode code points. */
const truncate = (line: string, width: number): string => {
  const limit = Math.max(0, Math.floor(width))
  let result = ''
  let used = 0
  for (const char of line) {
    const cells = /[\p{Mark}\u200d]/u.test(char) ? 0
      : /[\u1100-\u115f\u2329\u232a\u2e80-\ua4cf\uac00-\ud7a3\uf900-\ufaff\ufe10-\ufe19\ufe30-\ufe6f\uff01-\uff60\uffe0-\uffe6\p{Emoji_Presentation}\u{20000}-\u{3fffd}]/u.test(char) ? 2 : 1
    if (used + cells > limit) break
    result += char
    used += cells
  }
  return result
}

export function renderGraph(data: GraphData, width?: number): string[] {
  if (!data.ready) return ['数据未就绪']
  const lines = [data.summary]
  lines.push(`关键路径：${data.criticalPath.join(' → ') || '无'}`)
  const leaders = data.unlockRank.filter(item => item.count > 0).slice(0, 3)
  lines.push(`解锁排行：${leaders.map(item => `先做 ${item.id}（放出 ${item.count} 张）`).join('；') || '无'}`)
  if (data.unresolved.length) lines.push(`依赖成环：${data.unresolved.join('、')}`)
  for (const row of data.rows) {
    const symbol = finished(row.state) ? '✓' : row.state === 'started' ? '🔨'
      : row.blockers.length ? '⛓' : '📥'
    const blocked = row.blockers.length ? ` ⛓ ${row.blockers.join('、')}` : ''
    lines.push(`${'  '.repeat(row.depth)}${symbol} ${row.id}${blocked}`)
  }
  return width === undefined ? lines : lines.map(line => truncate(line, width))
}
