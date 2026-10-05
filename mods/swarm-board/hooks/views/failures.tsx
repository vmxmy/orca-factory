import type { ComponentType, ReactElement } from 'react'

// h 由 mod 运行时注入；本文件不用 JSX，因为 gate 在没有 node_modules 的检出里跑 CHECK。
declare function h(
  component: ComponentType<{ dimColor?: boolean; children?: string }>,
  props: { dimColor?: boolean; key?: string },
  ...children: string[]
): ReactElement
import type { FailureData } from '../../types'

export type LedgerRow = {
  ticket?: string
  tier?: string
  lane?: number
  first_gate?: string
  corrections?: number
}
export type CommentInput = string | { body: string; createdAt?: string }
export type FailureInput = {
  ledger?: readonly LedgerRow[]
  comments?: Readonly<Record<string, readonly CommentInput[]>>
  now?: number
}
export type FailureView = FailureData & {
  handoffReasons: { reason: string; count: number }[]
  firstGate: { key: string; green: number; total: number }[]
}

const rankedReasons = (counts: Map<string, number>) =>
  [...counts].map(([reason, count]) => ({ reason, count }))
    .sort((a, b) => b.count - a.count || a.reason.localeCompare(b.reason))

export function buildFailures(input: FailureInput = {}): FailureView {
  const ledger: readonly LedgerRow[] = Array.isArray(input.ledger) ? input.ledger : []
  const topCorrections = ledger
    .filter(row => row.ticket && (row.corrections ?? 0) > 0)
    .map(row => ({ ticket: row.ticket ?? '', corrections: row.corrections ?? 0 }))
    .sort((a, b) => b.corrections - a.corrections || a.ticket.localeCompare(b.ticket))
    .slice(0, 5)

  const gates = new Map<string, { green: number; total: number }>()
  for (const row of ledger) {
    if (row.first_gate !== 'green' && row.first_gate !== 'red') continue
    // 缺 tier 归 unknown，与 ledger.sh 一致。
    const keys = [`tier=${row.tier ?? 'unknown'}`]
    if (row.lane !== undefined) keys.push(`lane=${row.lane}`)
    for (const key of keys) {
      const group = gates.get(key) ?? { green: 0, total: 0 }
      gates.set(key, {
        green: group.green + Number(row.first_gate === 'green'),
        total: group.total + 1,
      })
    }
  }
  const firstGate = [...gates].map(([key, group]) => ({ key, ...group }))
    .sort((a, b) => a.key.localeCompare(b.key))

  const fails = new Map<string, number>()
  const handoffs = new Map<string, number>()
  for (const comments of Object.values(input.comments ?? {})) {
    const ticketComments: readonly CommentInput[] = Array.isArray(comments) ? comments : []
    for (const comment of ticketComments) {
      const body = typeof comment === 'string' ? comment : comment.body
      const lines = body.split(/\r?\n/)
      for (const line of lines) {
        const reason = line.match(/^FAIL: approach=.* \| reason=(.+?) \| evidence=/)?.[1]
        if (reason !== undefined) fails.set(reason, (fails.get(reason) ?? 0) + 1)
      }
      const createdAt = typeof comment === 'string' ? undefined : comment.createdAt
      if (input.now !== undefined && createdAt !== undefined) {
        const at = Date.parse(createdAt)
        if (Number.isFinite(at) && at < input.now - 7 * 86400000) continue
      }
      const handoff = lines.find(line => line.startsWith('转人:'))
      if (handoff !== undefined) {
        const reason = handoff.slice('转人:'.length).trim().split(': ', 1)[0]
        handoffs.set(reason, (handoffs.get(reason) ?? 0) + 1)
      }
    }
  }

  return {
    topCorrections,
    repeatedFails: rankedReasons(fails).filter(row => row.count >= 2),
    handoffReasons: rankedReasons(handoffs),
    firstGate,
  }
}

export const renderFailures = (
  Text: ComponentType<{ dimColor?: boolean; children?: string }>,
  data?: FailureView,
) => {
  if (!data || [data.handoffReasons, data.firstGate, data.repeatedFails, data.topCorrections]
    .every(rows => rows.length === 0)) {
    return h(Text, { dimColor: true }, '数据未就绪')
  }
  return [
    h(Text, { key: 'handoffs' }, '转人原因\n' + (data.handoffReasons
      .map(row => `${row.reason}: ${row.count}`).join('\n') || '无')),
    h(Text, { key: 'first-gate' }, '首跑通过率\n' + (data.firstGate
      .map(row => `${row.key}: ${row.green}/${row.total}`).join('\n') || '无')),
    h(Text, { key: 'fails' }, '重复 FAIL\n' + (data.repeatedFails
      .map(row => `${row.reason}: ${row.count}`).join('\n') || '无')),
    h(Text, { key: 'corrections' }, 'correction 最多\n' + (data.topCorrections
      .map(row => `${row.ticket}: ${row.corrections}`).join('\n') || '无')),
  ]
}
