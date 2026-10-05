import type { ComponentType, ReactElement } from 'react'
import type { GraphData } from '../../types'

// h 由 mod 运行时注入；本文件不用 JSX，因为 gate 在没有 node_modules 的检出里跑 CHECK。
declare function h(
  component: ComponentType<{ dimColor?: boolean; children?: string }>,
  props: { dimColor?: boolean; key?: number },
  ...children: string[]
): ReactElement
import { readGraphSnapshot } from '../linear-cache'

/** The view consumes readGraphSnapshot's issues; it never fetches live data. */
export type GraphIssue = {
  id: string
  state: string
  parent?: string | null
  blockedBy?: readonly string[]
}
export type GraphInput = { issues: readonly GraphIssue[] }
export type GraphRow = GraphIssue & { depth: number; blockers: string[] }
export type GraphView = GraphData & {
  ready: boolean
  rows: GraphRow[]
  /** Cycles and their dependents cannot be assigned a DAG critical path. */
  unresolved: string[]
  summary: string
}
const finished = (state: string) => state === 'completed' || state === 'canceled'
const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0

export function buildGraph(input?: GraphInput | null): GraphView {
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

export function renderGraph(
  Text: ComponentType<{ dimColor?: boolean; children?: string }>,
  data: GraphData | GraphView | null = readGraphSnapshot(Date.now()),
) {
  if (data === null || ('ready' in data && !data.ready)) return h(Text, { dimColor: true }, '数据未就绪')
  const path = data.criticalPath.join(' → ') || '无'
  const leaders = data.unlockRank.filter(item => item.count > 0).slice(0, 3)
  const summary = 'summary' in data ? data.summary
    : `领取受依赖与并发限制，关键路径为 ${path}`
      + (leaders.length ? `，先完成 ${leaders[0].id} 可放出 ${leaders[0].count} 张下游票。` : '，没有可直接解锁的下游票。')
  const out: ReactElement[] = [
    h(Text, {}, summary),
    h(Text, {}, `关键路径：${path}`),
    h(Text, {}, `解锁排行：${leaders.map(item => `先做 ${item.id}（放出 ${item.count} 张）`).join('；') || '无'}`),
  ]
  if ('unresolved' in data && data.unresolved.length > 0) {
    out.push(h(Text, {}, `依赖成环：${data.unresolved.join('、')}`))
  }
  if ('rows' in data) {
    for (const row of data.rows) {
      const symbol = finished(row.state) ? '✓' : row.state === 'started' ? '🔨'
        : row.blockers.length ? '⛓' : '📥'
      const blocked = row.blockers.length ? ` ⛓ ${row.blockers.join('、')}` : ''
      out.push(h(Text, { key: row.id }, `${'  '.repeat(row.depth)}${symbol} ${row.id} [${row.state}]${blocked}`))
    }
  }
  return out
}
