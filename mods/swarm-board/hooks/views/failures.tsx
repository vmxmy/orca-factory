import type { FailureData } from '../../types'

// h 由 mod 宿主在运行时注入。
declare function h(type: unknown, props?: unknown, ...children: unknown[]): unknown

export type LedgerRow = {
  ticket?: string
  tier?: string
  lane?: number
  first_gate?: string
  corrections?: number
  finished_at?: string
}
export type FailuresInput = {
  now?: number
  ledger: readonly LedgerRow[]
  comments: Readonly<Record<string, readonly string[]>>
}
export type FailuresView = FailureData & {
  handoffs: { reason: string; count: number }[]
  firstGate: { key: string; green: number; total: number }[]
}

const HANDOFF_WINDOW_MS = 7 * 24 * 60 * 60 * 1000

const rankedReasons = (counts: Map<string, number>) =>
  [...counts].map(([reason, count]) => ({ reason, count }))
    .sort((a, b) => b.count - a.count || a.reason.localeCompare(b.reason))

export function buildFailures(input: FailuresInput): FailuresView {
  const ledger: readonly LedgerRow[] = Array.isArray(input?.ledger) ? input.ledger : []
  const comments = input?.comments && typeof input.comments === 'object'
    && !Array.isArray(input.comments) ? input.comments : {}
  const now = input?.now
  const corrections = new Map<string, number>()
  const recentTickets = new Set<string>()
  const gates = new Map<string, { green: number; total: number }>()
  for (const row of ledger) {
    const count = row.corrections ?? 0
    if (row.ticket && Number.isFinite(count) && count > 0) {
      corrections.set(row.ticket, (corrections.get(row.ticket) ?? 0) + count)
    }
    if (now !== undefined && row.ticket && row.finished_at) {
      const finishedAt = Date.parse(row.finished_at)
      if (finishedAt >= now - HANDOFF_WINDOW_MS && finishedAt <= now) {
        recentTickets.add(row.ticket)
      }
    }
    if (row.first_gate !== 'green' && row.first_gate !== 'red') continue
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
  for (const [ticket, rows] of Object.entries(comments)) {
    const ticketComments: readonly string[] = Array.isArray(rows) ? rows : []
    for (const comment of ticketComments) {
      if (typeof comment !== 'string') continue
      const body = comment.trim()
      const lines = comment.split(/\r?\n/)
      for (const line of lines) {
        const reason = line.match(/^FAIL: approach=.*? \| reason=(.*?) \| evidence=/)?.[1]
        if (reason !== undefined) fails.set(reason, (fails.get(reason) ?? 0) + 1)
      }
      if (now !== undefined && !recentTickets.has(ticket)) continue
      if (!body.startsWith('转人:')) continue
      const reason = body.split(/\r?\n/, 1)[0].slice('转人:'.length).trim()
      if (reason) handoffs.set(reason, (handoffs.get(reason) ?? 0) + 1)
    }
  }

  return {
    topCorrections,
    repeatedFails: rankedReasons(fails),
    handoffs: rankedReasons(handoffs),
    firstGate,
  }
}

export const renderFailures = (
  Text: unknown,
  data?: FailuresView,
) => {
  if (!data || [data.handoffs, data.firstGate, data.repeatedFails, data.topCorrections]
    .every(rows => rows.length === 0)) {
    return h(Text, { dimColor: true }, '数据未就绪')
  }
  const lines = [
    '转人原因',
    ...data.handoffs.map(row => `${row.reason}: ${row.count}`),
    '首跑通过率',
    ...data.firstGate.map(row => `${row.key}: ${row.green}/${row.total} (${Math.round(row.green / row.total * 100)}%)`),
    '重复 FAIL',
    ...data.repeatedFails.map(row => `${row.reason}: ${row.count}`),
    'correction 最多',
    ...data.topCorrections.map(row => `${row.ticket}: ${row.corrections}`),
  ]
  return h(Text, null, lines.join('\n'))
}
