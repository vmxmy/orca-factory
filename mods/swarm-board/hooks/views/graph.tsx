import type { ComponentType, ReactElement } from 'react'
import type { GraphData } from '../../types'
import { readGraphSnapshot } from '../linear-cache'

type TextProps = { dimColor?: boolean; children?: string }

// h 由 mod 运行时注入；本文件不用 JSX，因为 gate 在没有 node_modules 的检出里跑 CHECK。
declare function h(component: ComponentType<TextProps>, props: { dimColor?: boolean; key?: string }, ...children: string[]): ReactElement

export type GraphIssue = {
  id: string
  state: string
  parent?: string | null
  blockedBy: string[]
}
export type GraphInput = { issues: GraphIssue[] }
export type GraphView = GraphData & {
  headline: string
  groups: {
    parent: string | null
    rows: { id: string; state: string; depth: number; blockedBy: string[] }[]
  }[]
}

const finished = (state: string): boolean => state === 'completed' || state === 'canceled'

function longestPaths(edges: Map<string, string[]>): (id: string) => string[] {
  type Result = { path: string[]; cyclic: boolean }
  const memo = new Map<string, Result>()
  function visit(id: string, visiting: Set<string>): Result {
    if (visiting.has(id)) return { path: [], cyclic: true }
    const cached = memo.get(id)
    if (cached) return cached
    visiting.add(id)
    let path = [id]
    let cyclic = false
    for (const next of edges.get(id) ?? []) {
      const result = visit(next, visiting)
      cyclic ||= result.cyclic
      const candidate = [id, ...result.path]
      if (candidate.length > path.length) path = candidate
    }
    visiting.delete(id)
    const result = { path, cyclic }
    if (!cyclic) memo.set(id, result)
    return result
  }
  return (id: string): string[] => visit(id, new Set()).path
}

export function buildGraph(input?: GraphInput): GraphView | null {
  if (!input || !Array.isArray(input.issues)) return null
  const issues = input.issues
  const byId = new Map(issues.map(issue => [issue.id, issue]))
  const order = new Map(issues.map((issue, index) => [issue.id, index]))
  const active = issues.filter(issue => !finished(issue.state))
  const blockers = new Map<string, string[]>()
  const downstream = new Map<string, string[]>(active.map(issue => [issue.id, []]))
  for (const issue of active) {
    const pending = [...new Set(issue.blockedBy)].filter(id => {
      const blocker = byId.get(id)
      return blocker === undefined || !finished(blocker.state)
    }).sort((a, b) => (order.get(a) ?? 0) - (order.get(b) ?? 0))
    blockers.set(issue.id, pending)
    for (const id of pending) downstream.get(id)?.push(issue.id)
  }

  const pathFor = longestPaths(downstream)
  let criticalPath: string[] = []
  for (const issue of active) {
    const path = pathFor(issue.id)
    if (path.length > criticalPath.length) criticalPath = path
  }

  const unlockRank = active.map(issue => {
    const seen = new Set([issue.id])
    const pending = [issue.id]
    while (pending.length > 0) {
      const id = pending.pop()!
      for (const next of downstream.get(id) ?? []) {
        if (seen.has(next)) continue
        seen.add(next)
        pending.push(next)
      }
    }
    return { id: issue.id, count: seen.size - 1 }
  }).filter(item => item.count > 0)
    .sort((a, b) => b.count - a.count || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0))

  const families = new Map<string | null, GraphIssue[]>()
  for (const issue of issues) {
    const parent = issue.parent ?? null
    const family = families.get(parent) ?? []
    family.push(issue)
    families.set(parent, family)
  }
  const groups = [...families].map(([parent, family]) => {
    const members = new Set(family.map(issue => issue.id))
    const localBlockers = new Map(family.map(issue => [issue.id,
      [...new Set(issue.blockedBy)].filter(id => {
        const blocker = byId.get(id)
        return members.has(id) && blocker !== undefined && !finished(blocker.state)
      }).sort((a, b) => (order.get(a) ?? 0) - (order.get(b) ?? 0)),
    ]))
    const localPathFor = longestPaths(localBlockers)
    const rows = family.map(issue => ({
      id: issue.id,
      state: issue.state,
      depth: localPathFor(issue.id).length - 1,
      blockedBy: [...issue.blockedBy],
    })).sort((a, b) => a.depth - b.depth)
    return { parent, rows }
  })

  const ready = active.filter(issue => issue.state === 'unstarted' && blockers.get(issue.id)?.length === 0).length
  const blocked = active.filter(issue => blockers.get(issue.id)!.length > 0)
  const blockedOn = [...new Set(blocked.flatMap(issue => blockers.get(issue.id)!))]
  const leader = unlockRank[0]
  const headline = active.length === 0 ? '无未完成票'
    : `${active.length} 张未完成，仅 ${ready} 张可领`
      + (blocked.length ? `；${blocked.length} 张卡在 ${blockedOn.join('、')}` : '；无依赖阻塞')
      + (leader ? `；${leader.id} 影响 ${leader.count} 张下游（含间接依赖）` : '')
  return { criticalPath, unlockRank, groups, headline }
}

export function renderGraph(
  Text: ComponentType<TextProps>,
  graph: (GraphData & Partial<GraphView>) | null = readGraphSnapshot(Date.now()),
): ReactElement[] {
  if (!graph) return [h(Text, { key: 'unavailable', dimColor: true }, '数据未就绪')]
  const out: ReactElement[] = [
    h(Text, { key: 'headline' }, graph.headline ?? (graph.unlockRank.length
      ? `依赖摘要：卡在 ${graph.unlockRank[0].id}，影响 ${graph.unlockRank[0].count} 张下游；可领数量未知`
      : '依赖摘要：暂无阻塞排行；可领数量未知')),
    h(Text, { key: 'critical-path' }, `关键路径：${graph.criticalPath.join(' → ') || '无'}`),
    h(Text, { key: 'unlock-rank' }, `解锁排行：${graph.unlockRank.map(item => `${item.id}(${item.count})`).join(' · ') || '无'}`),
  ]
  for (const group of graph.groups ?? []) {
    out.push(h(Text, { key: JSON.stringify(['parent', group.parent]) }, `${group.parent ?? '无父票'}`))
    for (const row of group.rows) {
      out.push(h(Text, { key: JSON.stringify(['row', group.parent, row.id]), dimColor: finished(row.state) },
        `${'  '.repeat(row.depth + 1)}${row.id} [${row.state}]${row.blockedBy.length > 0 ? ` ← ${row.blockedBy.join(', ')}` : ''}`))
    }
  }
  return out
}
