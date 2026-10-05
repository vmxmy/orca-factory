import type { ComponentType, ReactElement } from 'react'
import type { GraphData } from '../../types'

declare function h(
  type: ComponentType<{ dimColor?: boolean; children?: string }>,
  props: { dimColor?: boolean },
  children: string,
): ReactElement

export type GraphInput = {
  issues: { id: string; state: string; parent?: string; blockedBy: string[] }[]
}

type GraphView = GraphData & {
  groups: { parent?: string; rows: { id: string; state: string; depth: number }[] }[]
  summary: string
}

const finished = (state: string) => state === 'completed' || state === 'canceled'
const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0
const betterPath = (a: string[], b: string[]) => {
  if (a.length !== b.length) return a.length > b.length
  for (let i = 0; i < a.length; i++) {
    const order = compare(a[i], b[i])
    if (order !== 0) return order < 0
  }
  return false
}

function longestDependencyPath(
  id: string,
  blockers: Map<string, string[]>,
  visiting = new Set<string>(),
): string[] {
  if (visiting.has(id)) return []
  const next = new Set([...visiting, id])
  let path: string[] = []
  for (const blocker of blockers.get(id) ?? []) {
    const candidate = longestDependencyPath(blocker, blockers, next)
    if (betterPath(candidate, path)) path = candidate
  }
  return [...path, id]
}

// Count only unfinished direct dependents whose sole unfinished blocker is id.
function countDirectDependentsUnlockedByCompletion(id: string, blockers: Map<string, string[]>): number {
  let count = 0
  for (const [dependent, pending] of blockers) {
    if (dependent !== id && pending.length === 1 && pending[0] === id) count++
  }
  return count
}

export function buildGraph(input?: GraphInput | null): GraphView {
  if (!input) return { criticalPath: [], unlockRank: [], groups: [], summary: '数据未就绪' }
  const issues = [...input.issues].sort((a, b) => compare(a.id, b.id))
  const byId = new Map(issues.map(issue => [issue.id, issue]))
  const active = issues.filter(issue => !finished(issue.state))
  // Unknown references are ignored, like completed blockers.
  const blockers = new Map(active.map(issue => [issue.id,
    [...new Set(issue.blockedBy)].filter(id => {
      const blocker = byId.get(id)
      return blocker !== undefined && !finished(blocker.state)
    }).sort(compare),
  ]))
  let criticalPath: string[] = []
  for (const issue of active) {
    const path = longestDependencyPath(issue.id, blockers)
    if (betterPath(path, criticalPath)) criticalPath = path
  }
  const unlockRank = active.map(issue => ({
    id: issue.id, count: countDirectDependentsUnlockedByCompletion(issue.id, blockers),
  })).filter(item => item.count > 0)
    .sort((a, b) => b.count - a.count || compare(a.id, b.id))

  const families = new Map<string | undefined, typeof issues>()
  for (const issue of issues) {
    const family = families.get(issue.parent) ?? []
    family.push(issue)
    families.set(issue.parent, family)
  }
  const groups = [...families].sort(([a], [b]) => {
    if (a === undefined) return b === undefined ? 0 : -1
    return b === undefined ? 1 : compare(a, b)
  }).map(([parent, family]) => {
    const ids = new Set(family.map(issue => issue.id))
    const dependencies = new Map(family.map(issue => [issue.id,
      [...new Set(issue.blockedBy)].filter(id => ids.has(id)).sort(compare),
    ]))
    const rows = family.map(issue => ({
      id: issue.id, state: issue.state,
      depth: longestDependencyPath(issue.id, dependencies).length - 1,
    })).sort((a, b) => a.depth - b.depth || compare(a.id, b.id))
    return { parent, rows }
  })
  const ready = active.filter(issue => blockers.get(issue.id)?.length === 0).length
  const blocked = active.length - ready
  const leader = unlockRank[0]
  const summary = active.length === 0 ? '全部票已完成或已取消，无需领取，也没有依赖阻塞。'
    : `可立即开工 ${ready} 张，${blocked} 张被依赖阻塞`
      + (leader ? `；先完成 ${leader.id} 可直接解锁 ${leader.count} 张下游票。`
        : blocked > 0 ? '；没有单张票可直接解除全部阻塞。' : '；没有依赖阻塞。')
  return { criticalPath, unlockRank, groups, summary }
}

export function graphLines(data?: GraphView | null): string[] {
  if (!data) return ['数据未就绪']
  return [
    data.summary,
    `关键路径：${data.criticalPath.join(' → ') || '无'}`,
    `解锁排行（直接下游）：${data.unlockRank.map(item => `${item.id}（${item.count} 张）`).join('；') || '无'}`,
    ...data.groups.flatMap(group => [
      `父票：${group.parent ?? '无父票'}`,
      ...group.rows.map(row => `${'  '.repeat(row.depth)}${row.id} [${row.state}]`),
    ]),
  ]
}

export function renderGraph(
  Text: ComponentType<{ dimColor?: boolean; children?: string }>,
  data?: GraphView | null,
): ReactElement {
  return h(Text, { dimColor: data == null }, graphLines(data).join('\n'))
}
