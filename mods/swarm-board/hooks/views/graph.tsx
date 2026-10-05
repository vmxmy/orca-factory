import type { ComponentType } from 'react'
import type { GraphData } from '../../types'

export type GraphIssue = { id: string; state: string; parent?: string; blockedBy: string[] }
export type GraphInput = { issues: GraphIssue[] }
export type GraphView = GraphData & {
  groups: { parent: string; rows: { id: string; state: string; depth: number }[] }[]
  summary: string
}

const finished = (state: string): boolean => state === 'completed' || state === 'canceled'
const compare = (a: string, b: string): number => a < b ? -1 : a > b ? 1 : 0

function betterPath(candidate: string[], current: string[]): boolean {
  if (candidate.length !== current.length) return candidate.length > current.length
  for (let i = 0; i < candidate.length; i++) {
    const order = compare(candidate[i], current[i])
    if (order !== 0) return order < 0
  }
  return false
}

// An explicit DFS stack also handles long cycles without exhausting the call stack.
function dependencyPaths(blockers: Map<string, string[]>): (id: string) => string[] {
  const memo = new Map<string, string[]>()
  type Frame = { id: string; next: number; path: string[]; cyclic: boolean }
  const frame = (id: string): Frame => ({ id, next: 0, path: [id], cyclic: false })
  return (id: string): string[] => {
    const cached = memo.get(id)
    if (cached) return cached
    const stack = [frame(id)]
    const visiting = new Set([id])
    while (stack.length > 0) {
      const current = stack[stack.length - 1]
      const upstream = blockers.get(current.id) ?? []
      if (current.next < upstream.length) {
        const blocker = upstream[current.next++]
        if (visiting.has(blocker)) {
          current.cyclic = true
          continue
        }
        const known = memo.get(blocker)
        if (known) {
          const candidate = [...known, current.id]
          if (betterPath(candidate, current.path)) current.path = candidate
        } else {
          visiting.add(blocker)
          stack.push(frame(blocker))
        }
        continue
      }
      stack.pop()
      visiting.delete(current.id)
      // A cycle-truncated path depends on the root's visiting set; do not cache it.
      if (!current.cyclic) memo.set(current.id, current.path)
      if (stack.length === 0) return current.path
      const parent = stack[stack.length - 1]
      parent.cyclic ||= current.cyclic
      const candidate = [...current.path, parent.id]
      if (betterPath(candidate, parent.path)) parent.path = candidate
    }
    return []
  }
}

export function buildGraph(input?: GraphInput): GraphView | null {
  if (!input || !Array.isArray(input.issues)) return null
  const issues = [...input.issues].sort((a, b) => compare(a.id, b.id))
  const active = issues.filter(issue => !finished(issue.state))
  const activeIds = new Set(active.map(issue => issue.id))
  const blockers = new Map(issues.map(issue => [issue.id,
    [...new Set(issue.blockedBy)].filter(id => activeIds.has(id)).sort(compare),
  ]))
  const pathFor = dependencyPaths(blockers)
  let criticalPath: string[] = []
  for (const issue of active) {
    const path = pathFor(issue.id)
    if (betterPath(path, criticalPath)) criticalPath = path
  }

  const downstream = new Map<string, string[]>()
  for (const issue of active) {
    for (const blocker of blockers.get(issue.id) ?? []) {
      const dependents = downstream.get(blocker) ?? []
      dependents.push(issue.id)
      downstream.set(blocker, dependents)
    }
  }
  const unlockRank = active.map(issue => {
    const seen = new Set([issue.id])
    const pending = [issue.id]
    while (pending.length > 0) {
      const id = pending.pop()!
      for (const dependent of downstream.get(id) ?? []) {
        if (seen.has(dependent)) continue
        seen.add(dependent)
        pending.push(dependent)
      }
    }
    return { id: issue.id, count: seen.size - 1 }
  }).filter(item => item.count > 0)
    .sort((a, b) => b.count - a.count || compare(a.id, b.id))

  const families = new Map<string, GraphView['groups'][number]['rows']>()
  for (const issue of issues) {
    const parent = issue.parent ?? '无父票'
    const rows = families.get(parent) ?? []
    rows.push({ id: issue.id, state: issue.state, depth: pathFor(issue.id).length - 1 })
    families.set(parent, rows)
  }
  const groups = [...families].sort(([a], [b]) => compare(a, b)).map(([parent, rows]) => ({
    parent, rows: rows.sort((a, b) => a.depth - b.depth || compare(a.id, b.id)),
  }))
  const ready = active.filter(issue => blockers.get(issue.id)?.length === 0).length
  const leader = unlockRank[0]
  const summary = issues.length === 0 ? '暂无票，无需领取，也没有依赖阻塞。'
    : active.length === 0 ? '全部票已完成或已取消，无需领取，也没有依赖阻塞。'
      : `未完成 ${active.length} 张，可领 ${ready} 张，${active.length - ready} 张被阻塞；`
        + (leader ? `先完成 ${leader.id} 可放出 ${leader.count} 张。`
          : ready < active.length ? '依赖成环，需先解除环内阻塞。' : '没有需要先完成的上游票。')
  return { criticalPath, unlockRank, groups, summary }
}

type TextProps = { dimColor?: boolean; children?: string }
type GraphText = Extract<ComponentType<TextProps>, (props: TextProps) => unknown>

export function renderGraph(Text: GraphText, data?: GraphView | null): ReturnType<GraphText> {
  if (!data) return Text({ dimColor: true, children: '数据未就绪' })
  const symbols: Record<string, string> = {
    backlog: '·', unstarted: '○', started: '◐', completed: '✓', canceled: '×',
  }
  const lines = [
    data.summary,
    ...data.groups.flatMap(group => [
      `父票：${group.parent}`,
      ...group.rows.map(row => `${'  '.repeat(row.depth)}${row.id} ${symbols[row.state] ?? '?'}`),
    ]),
    `关键路径：${data.criticalPath.join(' → ') || '无'}`,
    `解锁排行：${data.unlockRank.slice(0, 5).map(item => `${item.id}（${item.count} 张）`).join('；') || '无'}`,
  ]
  return Text({ children: lines.join('\n') })
}
