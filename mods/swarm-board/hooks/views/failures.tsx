import type { ComponentType } from 'react'
import type { FailureData } from '../../types'

// h 由宿主注入；只声明可避免干净 gate 检出依赖 jsx-dev-runtime。
declare function h(...args: unknown[]): unknown

export type LedgerRow = {
  ticket?: string
  tier?: string
  lane?: number
  first_gate?: string
  corrections?: number
}
export type FailuresInput = {
  now?: number
  ledger: LedgerRow[]
  comments: Record<string, (string | { body: string; createdAt: string })[]>
}
export type FailureView = FailureData & {
  handoffReasons: { reason: string; count: number }[]
  firstGate: { key: string; green: number; total: number }[]
}

const HANDOFF_WINDOW_MS = 7 * 24 * 60 * 60 * 1000

const rankedReasons = (counts: Map<string, number>) =>
  [...counts].map(([reason, count]) => ({ reason, count }))
    .sort((a, b) => b.count - a.count || a.reason.localeCompare(b.reason))

export function buildFailures(input: FailuresInput): FailureView {
  const ledger: readonly LedgerRow[] = Array.isArray(input?.ledger) ? input.ledger : []
  const comments = input?.comments && typeof input.comments === 'object'
    && !Array.isArray(input.comments) ? input.comments : {}
  const now = input?.now ?? Date.now()
  const corrections = new Map<string, number>()
  const gates = new Map<string, { green: number; total: number }>()
  for (const row of ledger) {
    const count = row.corrections ?? 0
    if (row.ticket && Number.isFinite(count) && count > 0) {
      corrections.set(row.ticket, Math.max(corrections.get(row.ticket) ?? 0, count))
    }
    if (row.first_gate === undefined) continue
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
  const topCorrections = [...corrections].map(([ticket, corrections]) => ({ ticket, corrections }))
    .sort((a, b) => b.corrections - a.corrections || a.ticket.localeCompare(b.ticket))
    .slice(0, 5)
  const firstGate = [...gates].map(([key, group]) => ({ key, ...group }))
    .sort((a, b) => a.key.localeCompare(b.key))

  const fails = new Map<string, number>()
  const handoffs = new Map<string, number>()
  for (const rows of Object.values(comments)) {
    const ticketComments = Array.isArray(rows) ? rows : []
    for (const comment of ticketComments) {
      const body = typeof comment === 'string' ? comment : comment.body
      const createdAt = typeof comment === 'string' ? now : Date.parse(comment.createdAt)
      const recent = createdAt >= now - HANDOFF_WINDOW_MS && createdAt <= now
      const lines = body.split(/\r?\n/)
      for (const line of lines) {
        const reason = line.match(/^FAIL:.*\|\s*reason=([^|]+?)\s*(\||$)/)?.[1].trim()
        if (reason) fails.set(reason, (fails.get(reason) ?? 0) + 1)
        if (recent && line.startsWith('转人:')) {
          const handoffReason = line.slice('转人:'.length).trim()
          if (handoffReason) handoffs.set(handoffReason, (handoffs.get(handoffReason) ?? 0) + 1)
        }
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
  const lines = [
    '转人原因',
    ...data.handoffReasons.map(row => `${row.reason}: ${row.count}`),
    '首跑通过率',
    ...data.firstGate.map(row => {
      const rate = row.total === 0 ? '—' : `${Math.round(row.green / row.total * 100)}%`
      return `${row.key}: ${row.green}/${row.total} (${rate})`
    }),
    '重复 FAIL',
    ...data.repeatedFails.map(row => `${row.reason}: ${row.count}`),
    'correction 最多',
    ...data.topCorrections.map(row => `${row.ticket}: ${row.corrections}`),
  ]
  return lines.map((line, index) => h(Text, { key: index }, line))
}
