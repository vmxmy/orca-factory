import { createRequire } from 'node:module'
import type { ComponentType } from 'react'
import type { GraphData as SnapshotGraphData } from '../../types'
import { readGraphSnapshot } from '../linear-cache'

export type GraphIssue = {
  id: string
  state: string
  parent?: string | null
  blockedBy: readonly string[]
}
export type GraphInput = { issues: readonly GraphIssue[] }
export type GraphData = SnapshotGraphData & {
  issues: GraphIssue[]
  rows: (GraphIssue & { depth: number; waitingOn: string[] })[]
  summary: string
}

const finished = (state: string) => state === 'completed' || state === 'canceled'
const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0

/** Snapshot-independent calculation; unknown blockers remain unresolved. */
export const buildGraph = (input?: GraphInput | null): GraphData => {
  const issues = (input?.issues ?? []).map(issue => ({ ...issue, blockedBy: [...new Set(issue.blockedBy)] }))
    .sort((a, b) => compare(a.id, b.id))
  const byId = new Map(issues.map(issue => [issue.id, issue]))
  const pending = issues.filter(issue => !finished(issue.state))
  const waitingOn = (issue: GraphIssue) => issue.blockedBy.filter(id => !finished(byId.get(id)?.state ?? 'unknown'))
  const children = new Map<string, GraphIssue[]>()
  for (const issue of pending) {
    for (const blocker of waitingOn(issue)) {
      const list = children.get(blocker) ?? []
      list.push(issue)
      children.set(blocker, list)
    }
  }

  // Topological dynamic programming: completed tickets split unfinished chains.
  // Cycles cannot be scheduled, so do not present them as an executable path.
  const degrees = new Map(pending.map(issue => [issue.id, waitingOn(issue).filter(id => byId.has(id)).length]))
  const queue = pending.filter(issue => degrees.get(issue.id) === 0)
  const paths = new Map<string, string[]>()
  let criticalPath: string[] = []
  for (let index = 0; index < queue.length; index++) {
    const issue = queue[index]
    const path = paths.get(issue.id) ?? [issue.id]
    if (path.length > criticalPath.length) criticalPath = path
    for (const child of children.get(issue.id) ?? []) {
      const candidate = [...path, child.id]
      if (candidate.length > (paths.get(child.id)?.length ?? 0)) paths.set(child.id, candidate)
      const remaining = degrees.get(child.id)! - 1
      degrees.set(child.id, remaining)
      if (remaining === 0) queue.push(child)
    }
  }

  // Count downstream work that becomes possible in sequence, not just immediate
  // children; a separate unresolved prerequisite prevents that branch unlocking.
  const unlockRank = pending.map(issue => {
    const released = new Set([issue.id])
    const frontier = [issue.id]
    for (let index = 0; index < frontier.length; index++) {
      for (const child of children.get(frontier[index]) ?? []) {
        if (!released.has(child.id) && waitingOn(child).every(id => released.has(id))) {
          released.add(child.id)
          frontier.push(child.id)
        }
      }
    }
    return { id: issue.id, count: released.size - 1 }
  }).sort((a, b) => b.count - a.count || compare(a.id, b.id))

  const groups = new Map<string | null, GraphIssue[]>()
  for (const issue of issues) {
    const parent = issue.parent ?? null
    const group = groups.get(parent) ?? []
    group.push(issue)
    groups.set(parent, group)
  }
  const rows: GraphData['rows'] = []
  const seen = new Set<string>()
  const visit = (issue: GraphIssue, depth: number) => {
    if (seen.has(issue.id)) return
    seen.add(issue.id)
    rows.push({ ...issue, depth, waitingOn: waitingOn(issue) })
    for (const child of groups.get(issue.id) ?? []) visit(child, depth + 1)
  }
  for (const [parent, group] of groups) {
    if (parent !== null && byId.has(parent)) continue
    for (const issue of group) visit(issue, parent === null ? 0 : 1)
  }
  // Keep malformed parent cycles visible, once per ticket.
  for (const issue of issues) visit(issue, 0)

  const blocked = pending.filter(issue => waitingOn(issue).length > 0)
  const active = pending.filter(issue => issue.state === 'started').length
  const ready = pending.filter(issue => issue.state !== 'started' && waitingOn(issue).length === 0).length
  const blockers = [...new Set(blocked.flatMap(waitingOn))].sort(compare)
  const cycle = queue.length < pending.length ? '，存在依赖环或被环阻塞的票' : ''
  const summary = !input ? '数据未就绪' : issues.length === 0 ? '暂无票据'
    : `依赖视角：进行中 ${active} 张、可领 ${ready} 张，${blocked.length} 张卡在 ${blockers.join('、') || '无阻塞票'}${cycle}（实际领取还受并发与筛选规则限制）。`
  return { issues, rows, summary, criticalPath, unlockRank }
}

const hasIssues = (snapshot: SnapshotGraphData | GraphInput): snapshot is GraphInput => 'issues' in snapshot

export const renderGraph = (
  Text: ComponentType<{ dimColor?: boolean; children?: string }>,
  snapshot: SnapshotGraphData | GraphInput | null = readGraphSnapshot(Date.now()),
) => {
  // Only the host renderer needs React. Pure calculation/build checks work in a
  // clean checkout, without installing the host application's React package.
  const { createElement } = createRequire(import.meta.url)('react') as typeof import('react')
  if (!snapshot) return createElement(Text, { dimColor: true }, '数据未就绪')
  const graph = hasIssues(snapshot) ? buildGraph(snapshot) : null
  const data = graph ?? snapshot as SnapshotGraphData
  const lines = [graph?.summary ?? '票据明细数据未就绪，暂不能判断领取数量与阻塞原因。']
  lines.push(`关键路径：${data.criticalPath.join(' → ') || '无'}`)
  lines.push(`解锁排行（逐步完成后可放出的下游票）：${data.unlockRank.map(({ id, count }) => `${id} ${count}`).join('、') || '无'}`)
  const critical = new Set(data.criticalPath)
  let missingParent: string | null = null
  const ids = new Set(graph?.issues.map(issue => issue.id))
  for (const row of graph?.rows ?? []) {
    if (row.parent && !ids.has(row.parent) && row.parent !== missingParent) {
      missingParent = row.parent
      lines.push(`${row.parent} [父票数据缺失]`)
    }
    const dependencies = row.blockedBy.map(id => `${id}${row.waitingOn.includes(id) ? '' : ' ✓'}`).join('、')
    lines.push(`${'  '.repeat(row.depth)}${critical.has(row.id) ? '★ ' : ''}${row.id} [${row.state}]${dependencies ? ` ← ${dependencies}` : ''}`)
  }
  return createElement(Text, null, lines.join('\n'))
}
